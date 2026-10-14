package server_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/wnarutou/gitrieve/internal/config"
	server "github.com/wnarutou/gitrieve/internal/server"
)

// loadTempConfig writes a config file containing the given server section
// (empty string means no `server:` section) and points config.Init at it so
// GetServerConfig reads from the file-loaded viper instance, mirroring how the
// `server` command runs in production.
func loadTempConfig(t *testing.T, serverSection string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "repository:\n  - name: test\n    url: github.com/test/repo\n"
	if serverSection != "" {
		content += serverSection
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	config.Path = path
	config.Init()
}

func TestLogRetentionConfigDefaultsAndExport(t *testing.T) {
	for _, tc := range []struct {
		section string
		want    int
	}{
		{"", 2},
		{"server:\n  logRetentionMonths: 6\n", 6},
		{"server:\n  logRetentionMonths: 0\n", 2},
		{"server:\n  logRetentionMonths: -1\n", 2},
	} {
		loadTempConfig(t, tc.section)
		cfg := server.GetServerConfig()
		require.Equal(t, "0.0.0.0", cfg.Host)
		require.Equal(t, "8080", cfg.Port)
		require.Equal(t, "gitrieve.db", cfg.DbPath)
		// The exported document must carry the same effective setting the cleanup reads.
		exported, err := config.Export()
		require.NoError(t, err)
		var doc map[string]interface{}
		require.NoError(t, yaml.Unmarshal([]byte(exported), &doc))
		require.Equal(t, tc.want, doc["server"].(map[string]interface{})["logRetentionMonths"])
	}
}

// Regression test: the server section in config.yaml must be honored. Before
// the fix, GetServerConfig read from the global viper singleton (which never
// loads the config file) and used viper.Unmarshal, which cannot resolve the
// nested `server.*` keys — so host/port came back empty and the server bound
// to a random port instead of 0.0.0.0:8080.
func TestGetServerConfigReadsFromConfigFile(t *testing.T) {
	loadTempConfig(t, "server:\n  host: 0.0.0.0\n  port: \"8080\"\n  dbPath: /tmp/gitrieve-test.db\n  authEnabled: true\n  authToken: secret\n")

	cfg := server.GetServerConfig()
	if cfg.Host != "0.0.0.0" {
		t.Errorf("expected host 0.0.0.0, got %q", cfg.Host)
	}
	if cfg.Port != "8080" {
		t.Errorf("expected port 8080, got %q", cfg.Port)
	}
	if cfg.DbPath != "/tmp/gitrieve-test.db" {
		t.Errorf("expected dbPath /tmp/gitrieve-test.db, got %q", cfg.DbPath)
	}
	if !cfg.AuthEnabled {
		t.Error("expected authEnabled true")
	}
	if cfg.AuthToken != "secret" {
		t.Errorf("expected authToken secret, got %q", cfg.AuthToken)
	}
}

// When the config file has no `server:` section, defaults must apply
// (0.0.0.0:8080, gitrieve.db, auth disabled). The default host is 0.0.0.0 so
// the web UI is reachable from outside the container even when the user does
// not configure a server section (localhost would silently bind to loopback
// and break Docker port mapping).
func TestGetServerConfigDefaultsWhenSectionMissing(t *testing.T) {
	loadTempConfig(t, "")

	cfg := server.GetServerConfig()
	if cfg.Host != "0.0.0.0" {
		t.Errorf("expected default host 0.0.0.0, got %q", cfg.Host)
	}
	if cfg.Port != "8080" {
		t.Errorf("expected default port 8080, got %q", cfg.Port)
	}
	if cfg.DbPath != "gitrieve.db" {
		t.Errorf("expected default dbPath gitrieve.db, got %q", cfg.DbPath)
	}
	if cfg.AuthEnabled {
		t.Error("expected authEnabled to default to false")
	}
}

package server_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/wnarutou/gitrieve/internal/config"
	"github.com/wnarutou/gitrieve/internal/db"
	server "github.com/wnarutou/gitrieve/internal/server"
	"github.com/wnarutou/gitrieve/internal/typedef"
)

// Preview/apply must return useful rejection diagnostics without allowing
// decoder-generated token prefixes into actual HTTP response bodies.
func TestAccountPoolMalformedShapesHTTPDoNotLeakOrPublish(t *testing.T) {
	for _, route := range []string{"/api/config/import/preview", "/api/config/import"} {
		for _, content := range []string{
			"githubAccounts: [{name: alice, token: {value: ghp_sensitive-secret}}]",
			"githubAccounts: [ghp_sensitive-secret]",
		} {
			t.Run(route+content[:17], func(t *testing.T) {
				loadTempConfig(t, "githubAccounts: [{name: alice, token: original-token}]\n")
				testDB, err := db.Initialize(":memory:")
				require.NoError(t, err)
				defer testDB.Close()
				s := server.NewConfigTestServer(config.GetIns(), testDB)
				before, err := os.ReadFile(config.Path)
				require.NoError(t, err)
				body, err := json.Marshal(map[string]string{"config": content})
				require.NoError(t, err)
				code, response := getJSON(t, s, http.MethodPost, route, string(body))
				require.Equal(t, http.StatusBadRequest, code)
				encoded, err := json.Marshal(response)
				require.NoError(t, err)
				require.NotContains(t, string(encoded), "ghp_sen")
				require.Equal(t, "original-token", s.Cfg().GitHubAccounts[0].Token)
				require.Equal(t, "original-token", config.GetIns().GitHubAccounts[0].Token)
				after, err := os.ReadFile(config.Path)
				require.NoError(t, err)
				require.Equal(t, before, after)
			})
		}
	}
}

// Omitting the secret-containing list from global diffs hides changes; echoing
// its raw value leaks every credential in the HTTP response.
func TestPreviewAccountPoolMasksTokensAndKeepsLabels(t *testing.T) {
	loadTempConfig(t, "githubAccounts: [{name: alice, token: old-private-token}]\ngithubAccountConcurrency: 2\n")
	testDB, err := db.Initialize(":memory:")
	require.NoError(t, err)
	defer testDB.Close()
	s := server.NewConfigTestServer(config.GetIns(), testDB)
	body, err := json.Marshal(map[string]string{"config": "githubAccounts: [{name: bob, token: new-private-token}, {name: carol, token: another-private-token}]\ngithubAccountConcurrency: 3\n"})
	require.NoError(t, err)
	code, resp := getJSON(t, s, http.MethodPost, "/api/config/import/preview", string(body))
	require.Equal(t, http.StatusOK, code)
	changes, ok := resp["data"].(map[string]interface{})["globals"].([]interface{})
	require.True(t, ok, "preview must report changed account globals")
	byField := map[string]map[string]interface{}{}
	for _, change := range changes {
		c := change.(map[string]interface{})
		byField[c["field"].(string)] = c
	}
	require.Contains(t, byField, "githubAccounts")
	require.Equal(t, []interface{}{map[string]interface{}{"name": "alice", "token": "***"}}, byField["githubAccounts"]["existing"])
	require.Equal(t, []interface{}{map[string]interface{}{"name": "bob", "token": "***"}, map[string]interface{}{"name": "carol", "token": "***"}}, byField["githubAccounts"]["imported"])
	require.Equal(t, float64(3), byField["githubAccountConcurrency"]["imported"])
	encoded, err := json.Marshal(resp)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "private-token")
}

// Ignoring either choice or failing to save it changes the pool on reload.
func TestApplyAccountPoolGlobalChoicesPersist(t *testing.T) {
	for _, choice := range []string{"existing", "imported"} {
		t.Run(choice, func(t *testing.T) {
			loadTempConfig(t, "githubAccounts: [{name: alice, token: old-private-token}]\ngithubAccountConcurrency: 2\n")
			testDB, err := db.Initialize(":memory:")
			require.NoError(t, err)
			defer testDB.Close()
			s := server.NewConfigTestServer(config.GetIns(), testDB)
			body, err := json.Marshal(map[string]interface{}{
				"config":  "githubAccounts: [{name: bob, token: new-private-token}]\ngithubAccountConcurrency: 3\n",
				"choices": map[string]interface{}{"global_choices": map[string]string{"githubAccounts": choice, "githubAccountConcurrency": choice}},
			})
			require.NoError(t, err)
			code, _ := getJSON(t, s, http.MethodPost, "/api/config/import", string(body))
			require.Equal(t, http.StatusOK, code)
			code, _ = getJSON(t, s, http.MethodPost, "/api/config/reload", "")
			require.Equal(t, http.StatusOK, code)
			code, response := getJSON(t, s, http.MethodGet, "/api/config/export", "")
			require.Equal(t, http.StatusOK, code)
			exported := response["data"].(map[string]interface{})["yaml"].(string)
			if choice == "existing" {
				require.Contains(t, exported, "name: alice")
				require.Contains(t, exported, "token: old-private-token")
				require.Contains(t, exported, "githubAccountConcurrency: 2")
			} else {
				require.Contains(t, exported, "name: bob")
				require.Contains(t, exported, "token: new-private-token")
				require.Contains(t, exported, "githubAccountConcurrency: 3")
			}
		})
	}
}

// Importing a pre-pool document must either retain the chosen current pool or
// clear it and use the imported legacy token, with the default account cap.
func TestApplyLegacyDocumentAccountPoolChoices(t *testing.T) {
	for _, choice := range []string{"existing", "imported"} {
		t.Run(choice, func(t *testing.T) {
			loadTempConfig(t, "githubAccounts: [{name: alice, token: pool-secret}]\ngithubAccountConcurrency: 5\n")
			testDB, err := db.Initialize(":memory:")
			require.NoError(t, err)
			defer testDB.Close()
			s := server.NewConfigTestServer(config.GetIns(), testDB)
			body, err := json.Marshal(map[string]interface{}{
				"config":  "githubToken: legacy-import-secret\n",
				"choices": map[string]interface{}{"global_choices": map[string]string{"githubAccounts": choice, "githubAccountConcurrency": choice}},
			})
			require.NoError(t, err)
			code, _ := getJSON(t, s, http.MethodPost, "/api/config/import", string(body))
			require.Equal(t, http.StatusOK, code)
			code, _ = getJSON(t, s, http.MethodPost, "/api/config/reload", "")
			require.Equal(t, http.StatusOK, code)
			cfg := s.Cfg()
			if choice == "existing" {
				require.Len(t, cfg.GitHubAccounts, 1)
				require.Equal(t, "pool-secret", cfg.GitHubAccounts[0].Token)
				require.Equal(t, uint(5), cfg.GitHubAccountConcurrency)
			} else {
				require.Empty(t, cfg.GitHubAccounts)
				require.Equal(t, uint(2), cfg.GitHubAccountConcurrency)
				require.Equal(t, "legacy-import-secret", cfg.GitHubToken)
			}
		})
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func getJSON(t *testing.T, s *server.TestServer, method, path, body string) (int, map[string]interface{}) {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, bytes.NewBufferString(body))
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return w.Code, resp
}

type recordingScheduleRefresher struct {
	configs []*config.Config
}

func (r *recordingScheduleRefresher) RefreshSchedules(cfg *config.Config) error {
	r.configs = append(r.configs, cfg)
	return nil
}

func TestExportConfigYAML(t *testing.T) {
	loadTempConfig(t, "server:\n  host: 127.0.0.1\n  port: \"8081\"\n")
	testDB, err := db.Initialize(":memory:")
	require.NoError(t, err)
	defer testDB.Close()
	cfg := &config.Config{
		Repository:     []typedef.Repository{{Name: "test-repo", URL: "github.com/test/repo"}},
		Storage:        []typedef.MultiStorage{{Storage: typedef.Storage{Name: "local", Type: "file", Path: "/tmp"}}},
		GitHubToken:    "tok",
		RetryBaseDelay: 5 * time.Second,
	}
	s := server.NewConfigTestServer(cfg, testDB)

	code, resp := getJSON(t, s, http.MethodGet, "/api/config/export", "")
	require.Equal(t, 200, code)
	data := resp["data"].(map[string]interface{})
	yamlStr := data["yaml"].(string)
	require.Contains(t, yamlStr, "repository:")
	require.Contains(t, yamlStr, "url: github.com/test/repo")
	require.Contains(t, yamlStr, "githubToken: tok")
	require.Contains(t, yamlStr, "retryBaseDelay: 5s")
	require.Contains(t, yamlStr, "server:")
	require.Contains(t, yamlStr, "host: 127.0.0.1")
}

func TestPreviewImportValidationErrors(t *testing.T) {
	loadTempConfig(t, "")
	testDB, err := db.Initialize(":memory:")
	require.NoError(t, err)
	defer testDB.Close()
	cfg := &config.Config{
		Repository: []typedef.Repository{{Name: "test-repo", URL: "github.com/test/repo"}},
	}
	s := server.NewConfigTestServer(cfg, testDB)

	// Invalid YAML -> 400.
	code, _ := getJSON(t, s, http.MethodPost, "/api/config/import/preview", `{"config":"repository: [unclosed\n"}`)
	require.Equal(t, 400, code)

	// Identityless repository -> 400 with a list of violations.
	code, resp := getJSON(t, s, http.MethodPost, "/api/config/import/preview", `{"config":"repository:\n  - name: orphan\n"}`)
	require.Equal(t, 400, code)
	errors := resp["data"].(map[string]interface{})["errors"].([]interface{})
	require.Len(t, errors, 1)
	require.Contains(t, errors[0], "orphan")
}

func TestPreviewImportDiff(t *testing.T) {
	loadTempConfig(t, "")
	testDB, err := db.Initialize(":memory:")
	require.NoError(t, err)
	defer testDB.Close()
	cfg := &config.Config{
		Repository: []typedef.Repository{
			{Name: "mod-test", URL: "github.com/test/mod", Cron: "@daily"},
			{Name: "del-test", URL: "github.com/test/del"},
		},
		Storage: []typedef.MultiStorage{
			{Storage: typedef.Storage{Name: "st1", Type: "file", Path: "/tmp/one"}},
		},
		// Globals already at the seeded defaults except GitHubToken and
		// RetryBaseDelay, so the globals diff is exactly those two fields.
		GitHubToken:                 "tok-existing",
		ConcurrencyNum:              3,
		ReleaseSizeLimit:            300000000,
		ReleaseNumLimit:             3,
		RetryMaxCount:               3,
		SyncOverdueGrace:            config.DefaultSyncOverdueGrace,
		SyncStuckThreshold:          config.DefaultSyncStuckThreshold,
		GitHubAPIConcurrency:        2,
		GitHubAccountConcurrency:    2,
		GitHubMinRequestInterval:    200 * time.Millisecond,
		GitHubLowRemainingThreshold: 100,
		GitHubScheduleJitter:        30 * time.Second,
	}
	s := server.NewConfigTestServer(cfg, testDB)

	importYAML := `repository:
  - name: mod-test
    url: github.com/test/mod
  - name: add-test
    url: github.com/test/added
storage:
  - name: st1
    type: file
    path: /tmp/two
githubToken: tok-import
retryBaseDelay: "5s"
server:
  port: "9090"
`
	body, _ := json.Marshal(map[string]string{"config": importYAML})
	code, resp := getJSON(t, s, http.MethodPost, "/api/config/import/preview", string(body))
	require.Equal(t, 200, code)
	data := resp["data"].(map[string]interface{})

	summary := data["summary"].(map[string]interface{})
	repos := summary["repositories"].(map[string]interface{})
	require.Equal(t, float64(1), repos["added"])
	require.Equal(t, float64(1), repos["deleted"])
	require.Equal(t, float64(1), repos["modified"])
	storages := summary["storages"].(map[string]interface{})
	require.Equal(t, float64(0), storages["added"])
	require.Equal(t, float64(0), storages["deleted"])
	require.Equal(t, float64(1), storages["modified"])
	require.Equal(t, float64(2), summary["globals"].(map[string]interface{})["changed"])
	require.Equal(t, float64(1), summary["server"].(map[string]interface{})["changed"])

	repoDiff := data["repositories"].(map[string]interface{})
	modified := repoDiff["modified"].([]interface{})
	require.Len(t, modified, 1)
	m := modified[0].(map[string]interface{})
	require.Equal(t, "github.com/test/mod", m["key"])
	changes := m["changes"].([]interface{})
	require.Len(t, changes, 1)
	c := changes[0].(map[string]interface{})
	require.Equal(t, "cron", c["field"])
	require.Equal(t, "@daily", c["existing"])
	require.Equal(t, "", c["imported"])

	deleted := repoDiff["deleted"].([]interface{})
	require.Len(t, deleted, 1)
	require.Equal(t, "github.com/test/del", deleted[0].(map[string]interface{})["key"])

	added := repoDiff["added"].([]interface{})
	require.Len(t, added, 1)
	require.Equal(t, "github.com/test/added", added[0].(map[string]interface{})["key"])

	stDiff := data["storages"].(map[string]interface{})
	stModified := stDiff["modified"].([]interface{})
	require.Len(t, stModified, 1)
	sm := stModified[0].(map[string]interface{})
	require.Equal(t, "st1", sm["name"])
	smChanges := sm["changes"].([]interface{})
	require.Len(t, smChanges, 1)
	require.Equal(t, "path", smChanges[0].(map[string]interface{})["field"])

	// githubToken is masked, never echoed in plaintext.
	globals := data["globals"].([]interface{})
	var tokenChange map[string]interface{}
	for _, g := range globals {
		if g.(map[string]interface{})["field"] == "githubToken" {
			tokenChange = g.(map[string]interface{})
		}
	}
	require.NotNil(t, tokenChange)
	require.Equal(t, "***", tokenChange["existing"])
	require.Equal(t, "***", tokenChange["imported"])

	// retryBaseDelay is compared in its string form ("0s" vs "5s"), not as
	// raw nanoseconds.
	var delayChange map[string]interface{}
	for _, g := range globals {
		if g.(map[string]interface{})["field"] == "retryBaseDelay" {
			delayChange = g.(map[string]interface{})
		}
	}
	require.NotNil(t, delayChange)
	require.Equal(t, "0s", delayChange["existing"])
	require.Equal(t, "5s", delayChange["imported"])

	// Server section changed -> warning present.
	warnings := data["warnings"].([]interface{})
	require.Len(t, warnings, 1)
}

func TestPreviewAndApplyImportSyncHealthGlobals(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	writeFile(t, path, "repository:\n  - name: one\n    url: github.com/one/repo\nsyncOverdueGrace: 20m\nsyncStuckThreshold: 8h\n")
	config.Path = path
	config.Init()
	t.Cleanup(func() { config.Path = "" })

	testDB, err := db.Initialize(":memory:")
	require.NoError(t, err)
	defer testDB.Close()
	s := server.NewConfigTestServer(config.GetIns(), testDB)

	importYAML := "repository:\n  - name: one\n    url: github.com/one/repo\nsyncOverdueGrace: 45m\nsyncStuckThreshold: 12h\n"
	body, err := json.Marshal(map[string]string{"config": importYAML})
	require.NoError(t, err)

	code, resp := getJSON(t, s, http.MethodPost, "/api/config/import/preview", string(body))
	require.Equal(t, http.StatusOK, code)
	summary := resp["data"].(map[string]interface{})["summary"].(map[string]interface{})
	require.Equal(t, float64(2), summary["globals"].(map[string]interface{})["changed"])

	code, resp = getJSON(t, s, http.MethodPost, "/api/config/import", string(body))
	require.Equal(t, http.StatusOK, code)
	result := resp["data"].(map[string]interface{})
	require.Equal(t, float64(2), result["globals_updated"])
	require.Equal(t, 45*time.Minute, s.Cfg().SyncOverdueGrace)
	require.Equal(t, 12*time.Hour, s.Cfg().SyncStuckThreshold)
}

func TestApplyImportGitHubAPIGlobals(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	writeFile(t, path, "repository:\n  - name: one\n    url: github.com/one/repo\n")
	config.Path = path
	config.Init()
	t.Cleanup(func() { config.Path = "" })

	testDB, err := db.Initialize(":memory:")
	require.NoError(t, err)
	defer testDB.Close()
	s := server.NewConfigTestServer(config.GetIns(), testDB)

	importYAML := "repository:\n  - name: one\n    url: github.com/one/repo\ngithubApiConcurrency: 5\ngithubMinRequestInterval: 900ms\ngithubLowRemainingThreshold: 321\ngithubScheduleJitter: 0s\n"
	body, _ := json.Marshal(map[string]string{"config": importYAML})
	code, _ := getJSON(t, s, http.MethodPost, "/api/config/import", string(body))
	require.Equal(t, 200, code)
	require.Equal(t, uint(5), s.Cfg().GitHubAPIConcurrency)
	require.Equal(t, 900*time.Millisecond, s.Cfg().GitHubMinRequestInterval)
	require.Equal(t, 321, s.Cfg().GitHubLowRemainingThreshold)
	require.Zero(t, s.Cfg().GitHubScheduleJitter)

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(content), "githubapiconcurrency: 5")
}

func TestApplyImportDefaultChoices(t *testing.T) {
	loadTempConfig(t, "")
	testDB, err := db.Initialize(":memory:")
	require.NoError(t, err)
	defer testDB.Close()
	cfg := &config.Config{
		Repository: []typedef.Repository{
			{Name: "mod-test", URL: "github.com/test/mod", Cron: "@daily"},
			{Name: "del-test", URL: "github.com/test/del"},
		},
		Storage: []typedef.MultiStorage{
			{Storage: typedef.Storage{Name: "st1", Type: "file", Path: "/tmp/one"}},
		},
	}
	s := server.NewConfigTestServer(cfg, testDB)

	importYAML := `repository:
  - name: mod-test
    url: github.com/test/mod
  - name: add-test
    url: github.com/test/added
storage:
  - name: st1
    type: file
    path: /tmp/two
`
	body, _ := json.Marshal(map[string]string{"config": importYAML})
	code, resp := getJSON(t, s, http.MethodPost, "/api/config/import", string(body))
	require.Equal(t, 200, code)
	data := resp["data"].(map[string]interface{})
	require.Equal(t, float64(1), data["repositories_added"])
	require.Equal(t, float64(1), data["repositories_updated"])
	require.Equal(t, float64(0), data["repositories_deleted"])
	require.Equal(t, float64(0), data["storages_added"])
	require.Equal(t, float64(1), data["storages_updated"])
	require.Equal(t, float64(0), data["storages_deleted"])

	// Defaults: added+modified applied, deleted kept.
	repos := s.Cfg().Repository
	require.Len(t, repos, 3)
	byName := map[string]typedef.Repository{}
	for _, r := range repos {
		byName[r.Name] = r
	}
	require.Equal(t, "", byName["mod-test"].Cron) // modified -> imported wins
	_, delKept := byName["del-test"]
	require.True(t, delKept) // deleted -> kept
	_, addApplied := byName["add-test"]
	require.True(t, addApplied) // added -> imported
	require.Equal(t, "/tmp/two", s.Cfg().Storage[0].Path)
}

func TestApplyImportRefreshesServerSchedules(t *testing.T) {
	loadTempConfig(t, "")
	testDB, err := db.Initialize(":memory:")
	require.NoError(t, err)
	defer testDB.Close()
	cfg := &config.Config{Repository: []typedef.Repository{
		{Name: "cron-test", URL: "github.com/test/import-cron"},
	}}
	refresher := &recordingScheduleRefresher{}
	s := server.NewConfigTestServer(cfg, testDB, refresher)

	importYAML := `repository:
  - name: cron-test
    url: github.com/test/import-cron
    cron: "@every 1m"
`
	body, _ := json.Marshal(map[string]string{"config": importYAML})
	code, _ := getJSON(t, s, http.MethodPost, "/api/config/import", string(body))
	require.Equal(t, http.StatusOK, code)
	require.Len(t, refresher.configs, 1)
	require.Equal(t, "@every 1m", refresher.configs[0].Repository[0].Cron)
}

func TestApplyImportWithChoices(t *testing.T) {
	loadTempConfig(t, "")
	testDB, err := db.Initialize(":memory:")
	require.NoError(t, err)
	defer testDB.Close()
	cfg := &config.Config{
		Repository: []typedef.Repository{
			{Name: "mod-test", URL: "github.com/test/mod", Cron: "@daily"},
			{Name: "del-test", URL: "github.com/test/del"},
		},
		Storage: []typedef.MultiStorage{
			{Storage: typedef.Storage{Name: "st1", Type: "file", Path: "/tmp/one"}},
		},
	}
	s := server.NewConfigTestServer(cfg, testDB)

	importYAML := `repository:
  - name: mod-test
    url: github.com/test/mod
  - name: add-test
    url: github.com/test/added
storage:
  - name: st1
    type: file
    path: /tmp/two
`
	payload := map[string]interface{}{
		"config": importYAML,
		"choices": map[string]interface{}{
			"repository_choices":   map[string]string{"github.com/test/mod": "existing"},
			"repository_deletions": []string{"github.com/test/del"},
			"storage_choices":      map[string]string{"st1": "existing"},
		},
	}
	body, _ := json.Marshal(payload)
	code, resp := getJSON(t, s, http.MethodPost, "/api/config/import", string(body))
	require.Equal(t, 200, code)
	data := resp["data"].(map[string]interface{})
	require.Equal(t, float64(1), data["repositories_added"])
	require.Equal(t, float64(0), data["repositories_updated"])
	require.Equal(t, float64(1), data["repositories_deleted"])
	require.Equal(t, float64(0), data["storages_updated"])

	repos := s.Cfg().Repository
	require.Len(t, repos, 2)
	byName := map[string]typedef.Repository{}
	for _, r := range repos {
		byName[r.Name] = r
	}
	require.Equal(t, "@daily", byName["mod-test"].Cron) // choice "existing" wins
	_, delGone := byName["del-test"]
	require.False(t, delGone) // deletion applied
	_, addApplied := byName["add-test"]
	require.True(t, addApplied)                           // added always imported
	require.Equal(t, "/tmp/one", s.Cfg().Storage[0].Path) // choice "existing" wins
}

func TestReloadConfig(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	writeFile(t, path, "repository:\n  - name: one\n    url: github.com/one/repo\n")
	config.Path = path
	config.Init()

	testDB, err := db.Initialize(":memory:")
	require.NoError(t, err)
	defer testDB.Close()
	refresher := &recordingScheduleRefresher{}
	s := server.NewConfigTestServer(config.GetIns(), testDB, refresher)
	require.Equal(t, "one", s.Cfg().Repository[0].Name)

	onDisk := []byte("# this is the generation Reload must publish\nrepository:\n  - name: two\n    url: github.com/two/repo\n    cron: '@every 1m'\n")
	require.NoError(t, os.WriteFile(path, onDisk, 0o644))
	afterRead := []byte("# operator edit made after Reload read the file\nrepository:\n  - name: three\n    url: github.com/three/repo\n")
	s.SetReadReloadSnapshot(func() (*config.ReloadSnapshot, error) {
		snapshot, err := config.ReadReloadSnapshot()
		if err != nil {
			return nil, err
		}
		return snapshot, os.WriteFile(path, afterRead, 0o644)
	})
	code, _ := getJSON(t, s, http.MethodPost, "/api/config/reload", "")
	require.Equal(t, 200, code)
	require.Equal(t, "two", s.Cfg().Repository[0].Name)
	require.Len(t, refresher.configs, 1)
	require.Equal(t, "@every 1m", refresher.configs[0].Repository[0].Cron)
	afterReload, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, afterRead, afterReload, "reload must not overwrite bytes edited after its read")
}

func TestConfigSnapshotAccessorCannotMutatePublishedAPIConfig(t *testing.T) {
	testDB, err := db.Initialize(":memory:")
	require.NoError(t, err)
	defer testDB.Close()
	cfg := &config.Config{Repository: []typedef.Repository{{
		Name:    "original",
		URL:     "github.com/acme/original",
		Storage: []string{"archive"},
	}}}
	s := server.NewConfigTestServer(cfg, testDB)

	snapshot := s.Cfg()
	snapshot.Repository[0].Name = "caller mutation"
	snapshot.Repository[0].Storage[0] = "caller mutation"

	actual := s.Cfg()
	require.Equal(t, "original", actual.Repository[0].Name)
	require.Equal(t, []string{"archive"}, actual.Repository[0].Storage)
}

func TestReloadConfigKeepsOldOnError(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	writeFile(t, path, "repository:\n  - name: one\n    url: github.com/one/repo\n")
	config.Path = path
	config.Init()

	testDB, err := db.Initialize(":memory:")
	require.NoError(t, err)
	defer testDB.Close()
	s := server.NewConfigTestServer(config.GetIns(), testDB)

	writeFile(t, path, "repository: [broken\n")
	code, resp := getJSON(t, s, http.MethodPost, "/api/config/reload", "")
	require.Equal(t, 400, code)
	require.Contains(t, resp["message"].(string), "重载配置失败")
	require.Equal(t, "one", s.Cfg().Repository[0].Name)
}

// The server section is never hot-applied, but an accepted server choice must
// persist to config.yaml via SetServerField + Save (the "persist on next
// restart" contract). This guards the whole chain end to end.
func TestApplyImportPersistsServerSection(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	writeFile(t, path, "repository:\n  - name: one\n    url: github.com/one/repo\n")
	config.Path = path
	config.Init()

	testDB, err := db.Initialize(":memory:")
	require.NoError(t, err)
	defer testDB.Close()
	s := server.NewConfigTestServer(config.GetIns(), testDB)

	importYAML := `repository:
  - name: one
    url: github.com/one/repo
server:
  port: "9090"
`
	payload := map[string]interface{}{
		"config": importYAML,
		"choices": map[string]interface{}{
			"server_choices": map[string]string{"port": "imported"},
		},
	}
	body, _ := json.Marshal(payload)
	code, resp := getJSON(t, s, http.MethodPost, "/api/config/import", string(body))
	require.Equal(t, 200, code)
	data := resp["data"].(map[string]interface{})
	require.Equal(t, float64(1), data["server_updated"])

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(content), "server:")
	require.Contains(t, string(content), "9090")
}

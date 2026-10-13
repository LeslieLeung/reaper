package config

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
	"github.com/wnarutou/gitrieve/internal/typedef"
	"gopkg.in/yaml.v3"
)

// Decoder type errors must not render secret-bearing values before validation.
func TestAccountPoolMalformedShapesDoNotLeakSecrets(t *testing.T) {
	for name, content := range map[string]string{
		"token map":    "githubAccounts: [{name: alice, token: {value: ghp_sensitive-secret}}]",
		"scalar entry": "githubAccounts: [ghp_sensitive-secret]",
		"token list":   "githubAccounts: [{name: alice, token: [ghp_sensitive-secret]}]",
		"scalar pool":  "githubAccounts: ghp_sensitive-secret",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseImport(content)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "ghp_sen")
			writeTmpConfig(t, accountPoolYAML)
			require.NoError(t, os.WriteFile(Path, []byte(content), 0o644))
			err = Reload()
			require.Error(t, err)
			require.NotContains(t, err.Error(), "ghp_sen")
			require.Equal(t, "alice-secret", GetIns().GitHubAccounts[0].Token)
		})
	}
}

// Init exits on invalid input; exercise its actual diagnostic in a child process.
func TestAccountPoolInitRejectsMalformedShapeWithoutLeaking(t *testing.T) {
	if path := os.Getenv("GITRIEVE_TEST_MALFORMED_POOL_PATH"); path != "" {
		Path = path
		Init()
		return
	}
	for _, content := range []string{
		"githubAccounts: [{name: alice, token: {value: ghp_sensitive-secret}}]",
		"githubAccounts: [ghp_sensitive-secret]",
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
		cmd := exec.Command(os.Args[0], "-test.run=^TestAccountPoolInitRejectsMalformedShapeWithoutLeaking$")
		cmd.Env = append(os.Environ(), "GITRIEVE_TEST_MALFORMED_POOL_PATH="+path)
		output, err := cmd.CombinedOutput()
		require.Error(t, err)
		require.NotContains(t, string(output), "ghp_sen")
		require.Contains(t, string(output), "githubAccounts")
	}
}

const accountPoolYAML = `githubToken: legacy-secret
githubAccounts:
  - name: alice
    token: alice-secret
  - name: bob
    token: bob-secret
githubAccountConcurrency: 3
`

func requireAccountPoolDocument(t *testing.T, document string, concurrency int) {
	t.Helper()
	var decoded map[string]interface{}
	require.NoError(t, yaml.Unmarshal([]byte(document), &decoded))
	require.Equal(t, []interface{}{
		map[string]interface{}{"name": "alice", "token": "alice-secret"},
		map[string]interface{}{"name": "bob", "token": "bob-secret"},
	}, decoded["githubAccounts"])
	require.Equal(t, concurrency, decoded["githubAccountConcurrency"])
}

// Dropping pool fields from parsing/export or persistence loses credentials.
func TestAccountPoolYAMLParseExportSaveReload(t *testing.T) {
	doc, err := ParseImport(accountPoolYAML)
	require.NoError(t, err)
	parsed, err := yaml.Marshal(doc)
	require.NoError(t, err)
	requireAccountPoolDocument(t, string(parsed), 3)
	writeTmpConfig(t, accountPoolYAML)
	before := NewExecutionSnapshot(GetIns())
	require.NoError(t, Save())
	require.NoError(t, Reload())
	exported, err := Export()
	require.NoError(t, err)
	requireAccountPoolDocument(t, exported, 3)
	frozen, err := ExportFrom(GetExecutionConfig(WithExecutionSnapshot(context.Background(), before)))
	require.NoError(t, err)
	requireAccountPoolDocument(t, frozen, 3)
	// Save changed credentials/caps, rather than merely retaining Viper's
	// original file values, and verify reload reads the persisted replacement.
	next := GetIns()
	next.GitHubAccounts[0].Token = "replacement-secret"
	next.GitHubAccountConcurrency = 4
	require.NoError(t, SaveSnapshot(next))
	require.NoError(t, Reload())
	require.Equal(t, "replacement-secret", GetIns().GitHubAccounts[0].Token)
	require.Equal(t, uint(4), GetIns().GitHubAccountConcurrency)
	frozen, err = ExportFrom(GetExecutionConfig(WithExecutionSnapshot(context.Background(), before)))
	require.NoError(t, err)
	requireAccountPoolDocument(t, frozen, 3)
}

// Missing account validation would allow malformed credentials through imports.
func TestAccountPoolValidationRejectsBlankAndDuplicateEntries(t *testing.T) {
	for name, src := range map[string]string{
		"blank name":     "githubAccounts: [{name: ' ', token: sensitive-token}]",
		"blank token":    "githubAccounts: [{name: alice, token: ' '}]",
		"duplicate name": "githubAccounts: [{name: alice, token: sensitive-token}, {name: alice, token: other-sensitive-token}]",
	} {
		t.Run(name, func(t *testing.T) {
			doc, err := ParseImport(src)
			require.NoError(t, err)
			errs := ValidateImport(doc)
			require.NotEmpty(t, errs)
			for _, message := range errs {
				require.NotContains(t, message, "sensitive-token")
			}
		})
	}
}

// A reload must reject the same malformed account entries as import, retaining
// the previously published generation and never including credentials in errors.
func TestAccountPoolReloadRejectsMalformedCredentials(t *testing.T) {
	writeTmpConfig(t, accountPoolYAML)
	for _, content := range []string{
		"githubAccounts: [{name: ' ', token: sensitive-token}]",
		"githubAccounts: [{name: alice, token: ' '}]",
		"githubAccounts: [{name: alice, token: sensitive-token}, {name: alice, token: other-sensitive-token}]",
	} {
		require.NoError(t, os.WriteFile(Path, []byte(content), 0o644))
		err := Reload()
		require.Error(t, err)
		require.NotContains(t, err.Error(), "sensitive-token")
		require.Equal(t, "alice-secret", GetIns().GitHubAccounts[0].Token)
	}
}

func TestRejectUnsupportedStorageConfig(t *testing.T) {
	for _, kind := range []string{"s3", "ftp", ""} {
		t.Run(kind, func(t *testing.T) {
			writeTmpConfig(t, "storage:\n  - name: local\n    type: file\n    path: ./repo\n")
			before := GetIns()
			content := "storage:\n  - name: remote\n    type: '" + kind + "'\n"
			doc, err := ParseImport(content)
			require.NoError(t, err)
			errs := ValidateImport(doc)
			require.NotEmpty(t, errs)
			require.Contains(t, errs[0], "only 'file' storage is supported")
			require.NoError(t, os.WriteFile(Path, []byte(content), 0o644))
			err = Reload()
			require.ErrorContains(t, err, "only 'file' storage is supported")
			require.Equal(t, before, GetIns())
		})
	}
}

func TestDurationStringYAML(t *testing.T) {
	// Marshal produces the Go string form, usable as config.yaml.
	out, err := yaml.Marshal(ExportConfig{RetryBaseDelay: DurationString(5 * time.Second)})
	require.NoError(t, err)
	require.Contains(t, string(out), "retryBaseDelay: 5s")

	// Unmarshal accepts both the string form and integer nanoseconds.
	for _, src := range []string{"retryBaseDelay: 5s", "retryBaseDelay: 5000000000"} {
		var doc ExportConfig
		require.NoError(t, yaml.Unmarshal([]byte(src), &doc))
		require.Equal(t, DurationString(5*time.Second), doc.RetryBaseDelay)
	}

	// A bad duration is rejected.
	var doc ExportConfig
	err = yaml.Unmarshal([]byte("retryBaseDelay: 5pigs"), &doc)
	require.ErrorContains(t, err, "invalid duration")
}

func TestExportFromRoundTrip(t *testing.T) {
	writeTmpConfig(t, "server:\n  host: 127.0.0.1\n  port: \"8081\"\n")

	cfg := &Config{
		Repository:         []typedef.Repository{{Name: "r1", URL: "github.com/a/b"}},
		Storage:            []typedef.MultiStorage{{Storage: typedef.Storage{Name: "local", Type: "file", Path: "/tmp"}}},
		GitHubToken:        "tok",
		ConcurrencyNum:     3,
		RetryBaseDelay:     5 * time.Second,
		SyncOverdueGrace:   45 * time.Minute,
		SyncStuckThreshold: 12 * time.Hour,
	}
	yamlStr, err := ExportFrom(cfg)
	require.NoError(t, err)
	require.Contains(t, yamlStr, "repository:")
	require.Contains(t, yamlStr, "url: github.com/a/b")
	require.Contains(t, yamlStr, "githubToken: tok")
	require.Contains(t, yamlStr, "retryBaseDelay: 5s")
	require.Contains(t, yamlStr, "syncOverdueGrace: 45m0s")
	require.Contains(t, yamlStr, "syncStuckThreshold: 12h0m0s")
	require.Contains(t, yamlStr, "server:")
	require.Contains(t, yamlStr, "host: 127.0.0.1")

	// Round-trip: the exported YAML parses back with the same globals.
	doc, err := ParseImport(yamlStr)
	require.NoError(t, err)
	require.Equal(t, DurationString(5*time.Second), doc.RetryBaseDelay)
	require.Equal(t, DurationString(45*time.Minute), doc.SyncOverdueGrace)
	require.Equal(t, DurationString(12*time.Hour), doc.SyncStuckThreshold)
	require.Equal(t, "tok", doc.GitHubToken)
	require.Equal(t, "127.0.0.1", doc.Server.Host)
}

func TestReload(t *testing.T) {
	writeTmpConfig(t, "repository:\n  - name: one\n    url: github.com/one/repo\n")
	require.Equal(t, "one", GetIns().Repository[0].Name)

	// Rewrite the file, reload, and verify the new config is in effect.
	tmp, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	require.NoError(t, err)
	_, err = tmp.WriteString("repository:\n  - name: two\n    url: github.com/two/repo\n")
	require.NoError(t, err)
	require.NoError(t, tmp.Close())
	Path = tmp.Name()
	require.NoError(t, Reload())
	require.Equal(t, "two", GetIns().Repository[0].Name)
	Path = ""

	// A broken file leaves the previous config untouched and returns an error.
	bad, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	require.NoError(t, err)
	_, err = bad.WriteString("repository: [unclosed\n")
	require.NoError(t, err)
	require.NoError(t, bad.Close())
	Path = bad.Name()
	require.Error(t, Reload())
	require.Equal(t, "two", GetIns().Repository[0].Name)
	Path = ""
}

func TestReadReloadSnapshotDoesNotPublishUntilExplicitBoundary(t *testing.T) {
	writeTmpConfig(t, "githubToken: old\nrepository:\n  - name: old\n    url: github.com/acme/old\n")
	reloadFile, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	require.NoError(t, err)
	readBytes := []byte("githubToken: read\nrepository:\n  - name: read\n    url: github.com/acme/read\n")
	_, err = reloadFile.Write(readBytes)
	require.NoError(t, err)
	require.NoError(t, reloadFile.Close())
	Path = reloadFile.Name()
	t.Cleanup(func() { Path = "" })

	loaded, err := ReadReloadSnapshot()
	require.NoError(t, err)
	require.Equal(t, "old", GetIns().GitHubToken, "validated disk read must remain unpublished")
	require.Equal(t, "read", loaded.Config().GitHubToken)

	afterRead := []byte("# external edit after read\ngithubToken: edited\n")
	require.NoError(t, os.WriteFile(Path, afterRead, 0o644))
	PublishReloadSnapshot(loaded, nil)
	require.Equal(t, "read", GetIns().GitHubToken)
	actual, err := os.ReadFile(Path)
	require.NoError(t, err)
	require.Equal(t, afterRead, actual, "publishing a read snapshot must never rewrite later disk edits")
}

func TestReloadRejectsIdentitylessRepo(t *testing.T) {
	writeTmpConfig(t, "repository:\n  - name: one\n    url: github.com/one/repo\n")
	tmp, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	require.NoError(t, err)
	_, err = tmp.WriteString("repository:\n  - name: orphan\n")
	require.NoError(t, err)
	require.NoError(t, tmp.Close())
	Path = tmp.Name()
	require.Error(t, Reload())
	require.Equal(t, "one", GetIns().Repository[0].Name)
	Path = ""
}

func TestReloadAfterSave(t *testing.T) {
	// Apply-style flow: a Save() (which vp.Set()s applied values) must not
	// poison a later Reload's re-read of a changed file.
	writeTmpConfig(t, "githubToken: aaa\nrepository:\n  - name: one\n    url: github.com/one/repo\n")
	require.Equal(t, "aaa", GetIns().GitHubToken)

	// Simulate applyImport publishing a copy-on-write config, then persisting.
	next := GetIns()
	next.GitHubToken = "zzz"
	SetIns(next)
	require.NoError(t, Save())

	// The operator restores/edits config.yaml on disk to something else.
	tmp, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	require.NoError(t, err)
	_, err = tmp.WriteString("githubToken: bbb\nrepository:\n  - name: two\n    url: github.com/two/repo\n")
	require.NoError(t, err)
	require.NoError(t, tmp.Close())
	Path = tmp.Name()

	require.NoError(t, Reload())
	require.Equal(t, "bbb", GetIns().GitHubToken, "reload must reflect the file, not stale Save() overrides")
	require.Equal(t, "two", GetIns().Repository[0].Name)
	Path = ""
}

func TestSaveSnapshotPersistsTheSuppliedGeneration(t *testing.T) {
	writeTmpConfig(t, "githubToken: initial\nrepository:\n  - name: initial\n    url: github.com/acme/initial\n")
	newer := &Config{GitHubToken: "newer", Repository: []typedef.Repository{{Name: "newer", URL: "github.com/acme/newer"}}}
	SetIns(newer)
	older := &Config{GitHubToken: "older", Repository: []typedef.Repository{{Name: "older", URL: "github.com/acme/older"}}}

	require.NoError(t, SaveSnapshot(older))
	saved, err := os.ReadFile(Path)
	require.NoError(t, err)
	require.Contains(t, string(saved), "githubtoken: older")
	require.Contains(t, string(saved), "name: older")
	require.Equal(t, "newer", GetIns().GitHubToken, "saving an explicit snapshot must not republish it")
}

func TestReloadAndSaveShareOneCoherentStateBoundary(t *testing.T) {
	writeTmpConfig(t, "githubToken: old\nrepository:\n  - name: old\n    url: github.com/acme/old\n")
	newFile, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	require.NoError(t, err)
	_, err = newFile.WriteString("githubToken: new\ngithubApiConcurrency: 7\nrepository:\n  - name: new\n    url: github.com/acme/new\n")
	require.NoError(t, err)
	require.NoError(t, newFile.Close())
	Path = newFile.Name()
	t.Cleanup(func() { Path = "" })

	previousReadConfigFile := readConfigFile
	readComplete := make(chan struct{})
	readRelease := make(chan struct{})
	readConfigFile = func(v *viper.Viper) error {
		err := previousReadConfigFile(v)
		if err != nil {
			return err
		}
		close(readComplete)
		<-readRelease
		return nil
	}
	t.Cleanup(func() { readConfigFile = previousReadConfigFile })

	reloadDone := make(chan error, 1)
	go func() { reloadDone <- Reload() }()
	<-readComplete

	// The disk generation has been read, but Reload has not parsed or installed
	// it yet. Reload must already own stateMu before Save is launched here.
	reloadOwnsState := !stateMu.TryLock()
	if !reloadOwnsState {
		stateMu.Unlock()
	}
	saveStarted := make(chan struct{})
	saveDone := make(chan error, 1)
	go func() {
		close(saveStarted)
		saveDone <- Save()
	}()
	<-saveStarted

	// On the broken implementation Reload does not own the lock, so Save can
	// finish in the read-to-install window and overwrite generation B with A.
	var saveErr error
	if !reloadOwnsState {
		saveErr = <-saveDone
	}
	close(readRelease)

	reloadErr := <-reloadDone
	if reloadOwnsState {
		saveErr = <-saveDone
	}
	require.NoError(t, reloadErr)
	require.NoError(t, saveErr)
	require.True(t, reloadOwnsState, "reload did not serialize the disk read-to-install window against Save")
	require.Equal(t, "new", GetIns().GitHubToken)
	saved, err := os.ReadFile(Path)
	require.NoError(t, err)
	require.Contains(t, string(saved), "githubtoken: new")
	require.NotContains(t, string(saved), "githubtoken: old")
}

func TestGetServerSectionReadsFromConfigFile(t *testing.T) {
	writeTmpConfig(t, "server:\n  host: 0.0.0.0\n  port: \"8080\"\n  dbPath: /tmp/t.db\n  authEnabled: true\n  authToken: secret\n")
	s := GetServerSection()
	require.Equal(t, "0.0.0.0", s.Host)
	require.Equal(t, "8080", s.Port)
	require.Equal(t, "/tmp/t.db", s.DbPath)
	require.True(t, s.AuthEnabled)
	require.Equal(t, "secret", s.AuthToken)
}

func TestGetServerSectionDefaults(t *testing.T) {
	writeTmpConfig(t, "")
	s := GetServerSection()
	require.Equal(t, "0.0.0.0", s.Host)
	require.Equal(t, "8080", s.Port)
	require.Equal(t, "gitrieve.db", s.DbPath)
	require.False(t, s.AuthEnabled)
	require.Equal(t, "", s.AuthToken)
}

func TestParseImportFullDoc(t *testing.T) {
	doc, err := ParseImport(`repository:
  - name: acme
    type: org
    orgName: acme
  - name: repo
    url: https://github.com/acme/repo
storage:
  - name: local
    type: file
    path: /tmp/x
githubToken: tok
retryBaseDelay: "5s"
server:
  host: 127.0.0.1
  port: "8081"
  logRetentionMonths: 6
`)
	require.NoError(t, err)
	require.Len(t, doc.Repository, 2)
	// org entry gets a synthesized concrete URL so its identity resolves.
	require.Equal(t, "https://github.com/acme", doc.Repository[0].EffectiveURL())
	require.Equal(t, "github.com/acme", doc.Repository[0].Key())
	require.Equal(t, "repo", doc.Repository[1].Name)
	require.Len(t, doc.Storage, 1)
	require.Equal(t, "local", doc.Storage[0].Name)
	require.Equal(t, "tok", doc.GitHubToken)
	require.Equal(t, DurationString(5*time.Second), doc.RetryBaseDelay)
	require.Equal(t, DurationString(DefaultSyncOverdueGrace), doc.SyncOverdueGrace)
	require.Equal(t, DurationString(DefaultSyncStuckThreshold), doc.SyncStuckThreshold)
	require.Equal(t, "127.0.0.1", doc.Server.Host)
	require.Equal(t, "8081", doc.Server.Port)
	require.Equal(t, 6, doc.Server.LogRetentionMonths)
}

func TestParseImportSeedsDefaults(t *testing.T) {
	// Omitted globals behave as configured (same seeding as Init), so an import
	// that omits them diffs as unchanged against a default-seeded current config.
	doc, err := ParseImport("repository:\n  - name: r\n    url: github.com/a/b\n")
	require.NoError(t, err)
	require.Equal(t, 3, doc.RetryMaxCount)
	require.Equal(t, DurationString(5*time.Second), doc.RetryBaseDelay)
	require.Equal(t, uint(3), doc.ConcurrencyNum)
	require.Equal(t, uint(2), doc.GitHubAPIConcurrency)
	require.Equal(t, DurationString(200*time.Millisecond), doc.GitHubMinRequestInterval)
	require.Equal(t, 100, doc.GitHubLowRemainingThreshold)
	require.Equal(t, DurationString(30*time.Second), doc.GitHubScheduleJitter)

	// Missing/partial server section falls back to defaults so it does not force
	// empty host/port/dbPath onto the config on apply.
	require.Equal(t, "0.0.0.0", doc.Server.Host)
	require.Equal(t, "8080", doc.Server.Port)
	require.Equal(t, "gitrieve.db", doc.Server.DbPath)
	require.False(t, doc.Server.AuthEnabled)
	require.Equal(t, 2, doc.Server.LogRetentionMonths)
}

func TestParseImportPreservesExplicitZeroScheduleJitter(t *testing.T) {
	doc, err := ParseImport("githubScheduleJitter: 0s\n")
	require.NoError(t, err)
	require.Zero(t, doc.GitHubScheduleJitter)
}

func TestParseImportInvalidYAML(t *testing.T) {
	_, err := ParseImport("repository: [unclosed\n")
	require.Error(t, err)
}

func TestValidateImport(t *testing.T) {
	// Identityless repo is rejected.
	doc, err := ParseImport("repository:\n  - name: orphan\n")
	require.NoError(t, err)
	errs := ValidateImport(doc)
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], "orphan")

	// Duplicate normalized URL across entries is rejected. The second URL must
	// carry the full host (https://github.com/X/Y.git) so it normalizes to the
	// same "github.com/x/y" key as the first — a bare https://X/Y.git would
	// normalize to "x/y" (host omitted) and not collide.
	doc, err = ParseImport("repository:\n  - name: a\n    url: github.com/x/y\n  - name: b\n    url: https://github.com/X/Y.git\n")
	require.NoError(t, err)
	errs = ValidateImport(doc)
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], "duplicates")

	// Duplicate storage names are rejected.
	doc, err = ParseImport("storage:\n  - name: local\n    type: file\n    path: /a\n  - name: local\n    type: file\n    path: /b\n")
	require.NoError(t, err)
	errs = ValidateImport(doc)
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], "local")

	// A fully valid doc returns no errors.
	doc, err = ParseImport("repository:\n  - name: r\n    url: github.com/a/b\n")
	require.NoError(t, err)
	require.Empty(t, ValidateImport(doc))
}

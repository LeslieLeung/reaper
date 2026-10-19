package issue

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/wnarutou/gitrieve/internal/config"
	"github.com/wnarutou/gitrieve/internal/maintenance"
	"github.com/wnarutou/gitrieve/internal/typedef"
)

type maintenanceTransport func(*http.Request) (*http.Response, error)

func (f maintenanceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func issueMaintenanceFixture(t *testing.T, signal *maintenance.Signal, empty *bool) context.Context {
	t.Helper()
	oldDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(t.TempDir()))
	t.Cleanup(func() { require.NoError(t, os.Chdir(oldDir)) })
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = maintenanceTransport(func(r *http.Request) (*http.Response, error) {
		body := `[]`
		header := make(http.Header)
		header.Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/issues") {
			if r.URL.Query().Get("page") != "" {
				return nil, fmt.Errorf("started another issue page after soft stop")
			}
			if !*empty {
				body = `[{"number":1,"title":"one","updated_at":"2026-10-20T01:00:00Z"},{"number":2,"title":"two","updated_at":"2026-10-20T01:00:00Z"}]`
				header.Set("Link", `<https://api.github.com/repos/test/repo/issues?page=2>; rel="next"`)
			}
		} else if strings.HasSuffix(r.URL.Path, "/comments") {
			signal.Request()
			require.NoError(t, r.Context().Err(), "soft stop must not cancel comment requests")
			if r.URL.Query().Get("page") == "2" {
				body = `[{"id":2,"body":"last comment"}]`
			} else {
				body = `[{"id":1,"body":"first comment"}]`
				header.Set("Link", fmt.Sprintf(`<https://api.github.com%s?page=2>; rel="next"`, r.URL.Path))
			}
		} else {
			t.Errorf("unexpected request: %s", r.URL)
			return nil, fmt.Errorf("unexpected request")
		}
		return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	return config.WithExecutionSnapshot(maintenance.WithSignal(context.Background(), signal), config.NewExecutionSnapshot(&config.Config{GitHubToken: "test"}))
}

func readIssueArchive(t *testing.T, name string) map[string]string {
	t.Helper()
	f, err := os.Open(name)
	require.NoError(t, err)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	require.NoError(t, err)
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if h.Typeflag == tar.TypeReg {
			data, err := io.ReadAll(tr)
			require.NoError(t, err)
			files[h.Name] = string(data)
		}
	}
	return files
}

func TestMaintenanceFinishesIssuePageAndPublishesArchive(t *testing.T) {
	signal := maintenance.NewSignal()
	empty := false
	ctx := issueMaintenanceFixture(t, signal, &empty)
	out := t.TempDir()
	err := Sync(ctx, typedef.Repository{URL: "github.com/test/repo", UseCache: true}, []typedef.MultiStorage{{Storage: typedef.Storage{Type: "file", Path: out}}})
	require.ErrorIs(t, err, maintenance.ErrRequested)
	files := readIssueArchive(t, filepath.Join(out, "github.com/test/repo/issues.tar.gz"))
	for _, name := range []string{"issues/#1.md", "issues/#2.md"} {
		require.Contains(t, files[name], "last comment")
	}
	require.NoError(t, ctx.Err())
}

func TestMaintenanceIssueStorageFailureIsRetriedWithoutUpdates(t *testing.T) {
	signal := maintenance.NewSignal()
	empty := false
	ctx := issueMaintenanceFixture(t, signal, &empty)
	out := filepath.Join(t.TempDir(), "storage")
	require.NoError(t, os.WriteFile(out, []byte("blocks storage directory"), 0600))
	repo := typedef.Repository{URL: "github.com/test/repo", UseCache: true}
	storages := []typedef.MultiStorage{{Storage: typedef.Storage{Type: "file", Path: out}}}
	err := Sync(ctx, repo, storages)
	require.Error(t, err)
	require.NotErrorIs(t, err, maintenance.ErrRequested)
	require.ErrorContains(t, err, out)
	require.NoError(t, os.Remove(out))
	empty = true
	ctx = config.WithExecutionSnapshot(context.Background(), config.NewExecutionSnapshot(&config.Config{GitHubToken: "test"}))
	require.NoError(t, Sync(ctx, repo, storages))
	files := readIssueArchive(t, filepath.Join(out, "github.com/test/repo/issues.tar.gz"))
	require.Contains(t, files["issues/#2.md"], "last comment")
}

func TestMaintenanceIssuePreRequestedSkipsWork(t *testing.T) {
	signal := maintenance.NewSignal()
	signal.Request()
	err := Sync(maintenance.WithSignal(context.Background(), signal), typedef.Repository{URL: "invalid URL"}, nil)
	require.ErrorIs(t, err, maintenance.ErrRequested)
}

func TestMaintenanceNoCacheIssuePreservesCanonicalUntilListingCompletes(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(fmt.Sprint(complete), func(t *testing.T) {
			signal := maintenance.NewSignal()
			empty := false
			ctx := issueMaintenanceFixture(t, signal, &empty)
			if complete {
				transport := http.DefaultTransport
				http.DefaultTransport = maintenanceTransport(func(r *http.Request) (*http.Response, error) {
					resp, err := transport.RoundTrip(r)
					if resp != nil && strings.HasSuffix(r.URL.Path, "/issues") {
						resp.Header.Del("Link")
					}
					return resp, err
				})
			}
			out := t.TempDir()
			canonical := filepath.Join(out, "github.com/test/repo/issues.tar.gz")
			require.NoError(t, os.MkdirAll(filepath.Dir(canonical), 0700))
			require.NoError(t, os.WriteFile(canonical, []byte("previous complete archive"), 0600))
			err := Sync(ctx, typedef.Repository{URL: "github.com/test/repo", UseCache: false}, []typedef.MultiStorage{{Storage: typedef.Storage{Type: "file", Path: out}}})
			require.ErrorIs(t, err, maintenance.ErrRequested)
			partial, err := filepath.Glob(filepath.Join(filepath.Dir(canonical), "issues.partial.*.tar.gz"))
			require.NoError(t, err)
			archive := canonical
			if complete {
				require.Empty(t, partial, "fully listed data may replace canonical even when stopping")
			} else {
				previous, err := os.ReadFile(canonical)
				require.NoError(t, err)
				require.Equal(t, "previous complete archive", string(previous))
				require.Len(t, partial, 1)
				archive = partial[0]
			}
			files := readIssueArchive(t, archive)
			for _, name := range []string{"issues/#1.md", "issues/#2.md"} {
				require.Contains(t, files[name], "last comment")
			}
		})
	}
}

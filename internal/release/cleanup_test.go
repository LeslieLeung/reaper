package release

import (
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
	"github.com/wnarutou/gitrieve/internal/typedef"
)

type releaseTransport func(*http.Request) (*http.Response, error)

func (f releaseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Exercise the real download/retention flow without making GitHub requests.
func mockReleaseAPI(t *testing.T, tag string) context.Context {
	t.Helper()
	previous := http.DefaultTransport
	http.DefaultTransport = releaseTransport(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/repos/test/repo/releases":
			body = fmt.Sprintf(`[{"id":1,"tag_name":%q,"published_at":"2026-09-20T00:00:00Z"},{"id":2,"tag_name":"v1","published_at":"2026-09-19T00:00:00Z"}]`, tag)
		case "/repos/test/repo/releases/1/assets":
			body = `[{"id":10,"name":"asset.zip","size":4,"state":"uploaded"}]`
		default:
			return nil, fmt.Errorf("unexpected GitHub request: %s", r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	return config.WithExecutionSnapshot(context.Background(), config.NewExecutionSnapshot(&config.Config{
		ReleaseSizeLimit: 4, ReleaseNumLimit: -1,
	}))
}

func writeReleaseFile(t *testing.T, name string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(name), 0700))
	require.NoError(t, os.WriteFile(name, []byte("data"), 0600))
}

func TestDownloadAllAssetsPrunesNonEmptyOldRelease(t *testing.T) {
	for _, tag := range []string{"v2", "stable/v2"} {
		t.Run(tag, func(t *testing.T) {
			ctx := mockReleaseAPI(t, tag)
			base := t.TempDir()
			root := filepath.Join(base, "github.com", "test", "repo", "release")
			old := filepath.Join(root, "v1")
			kept := filepath.Join(root, filepath.FromSlash(tag), "asset.zip")
			other := filepath.Join(base, "github.com", "test", "other", "release", "v1", "asset.zip")
			writeReleaseFile(t, filepath.Join(old, "nested", "asset.zip"))
			writeReleaseFile(t, kept)
			writeReleaseFile(t, other)
			err := DownloadAllAssets(ctx, typedef.Repository{URL: "github.com/test/repo"}, []typedef.MultiStorage{{Storage: typedef.Storage{Type: "file", Path: base}}})
			require.NoError(t, err)
			require.NoDirExists(t, old)
			require.FileExists(t, kept)
			require.FileExists(t, other)
		})
	}
}

func TestRemoveReleaseDirectoryRejectsUnsafeTargets(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "release")
	inside := filepath.Join(root, "v1", "nested", "asset.zip")
	outside := filepath.Join(base, "release-other", "asset.zip")
	writeReleaseFile(t, inside)
	writeReleaseFile(t, outside)
	for _, target := range []string{
		"", root, base, filepath.Dir(outside),
		root + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "release-other",
		filepath.Dir(inside), inside,
	} {
		t.Run(target, func(t *testing.T) {
			require.Error(t, removeReleaseDirectory(root, target))
			require.FileExists(t, inside)
			require.FileExists(t, outside)
		})
	}
	require.Error(t, removeReleaseDirectory("", filepath.Join(root, "v1")))
}

func TestCleanupFileReleasesPreservesUnrelatedFiles(t *testing.T) {
	root := t.TempDir()
	note := filepath.Join(root, "notes.txt")
	writeReleaseFile(t, note)
	writeReleaseFile(t, filepath.Join(root, "v1", "nested", "asset.zip"))
	require.NoError(t, cleanupFileReleases(context.Background(), root, nil))
	require.FileExists(t, note)
	require.NoDirExists(t, filepath.Join(root, "v1"))
}

func TestCleanupFileReleasesRetainsCaseAlias(t *testing.T) {
	root := t.TempDir()
	asset := filepath.Join(root, "V2", "asset.zip")
	writeReleaseFile(t, asset)
	if _, err := os.Stat(filepath.Join(root, "v2")); os.IsNotExist(err) {
		t.Skip("filesystem is case-sensitive")
	}
	require.NoError(t, cleanupFileReleases(context.Background(), root, []string{"v2"}))
	require.FileExists(t, asset)
}

func TestCleanupFileReleasesCancelled(t *testing.T) {
	root := t.TempDir()
	asset := filepath.Join(root, "v1", "asset.zip")
	writeReleaseFile(t, asset)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, cleanupFileReleases(ctx, root, nil), context.Canceled)
	require.FileExists(t, asset)
}

func TestCleanupFileReleasesMissingRoot(t *testing.T) {
	require.NoError(t, cleanupFileReleases(context.Background(), filepath.Join(t.TempDir(), "missing"), nil))
}

func TestRemoveReleaseDirectorySymlinks(t *testing.T) {
	for _, kind := range []string{"root", "version", "nested", "dangling"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "release")
			outside := filepath.Join(base, "outside")
			marker := filepath.Join(outside, "keep.txt")
			writeReleaseFile(t, marker)
			require.NoError(t, os.MkdirAll(root, 0700))
			target := filepath.Join(root, "v1")
			link := target
			linkTarget := outside
			switch kind {
			case "root":
				root = filepath.Join(base, "linked-release")
				link = root
				target = filepath.Join(root, "v1")
				writeReleaseFile(t, filepath.Join(outside, "v1", "asset.zip"))
			case "nested":
				writeReleaseFile(t, filepath.Join(target, "asset.zip"))
				link = filepath.Join(target, "link")
			case "dangling":
				linkTarget = filepath.Join(base, "missing")
			}
			if err := os.Symlink(linkTarget, link); err != nil {
				t.Skipf("symlink creation unavailable: %v", err)
			}
			if kind == "nested" {
				require.NoError(t, removeReleaseDirectory(root, target))
				require.NoDirExists(t, target)
			} else {
				require.Error(t, removeReleaseDirectory(root, target))
				require.Error(t, cleanupFileReleases(context.Background(), root, nil))
				_, err := os.Lstat(link)
				require.NoError(t, err)
			}
			require.FileExists(t, marker)
		})
	}
}

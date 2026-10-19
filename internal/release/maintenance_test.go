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
	"github.com/wnarutou/gitrieve/internal/maintenance"
	"github.com/wnarutou/gitrieve/internal/typedef"
)

type maintenanceTransport func(*http.Request) (*http.Response, error)

func (f maintenanceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMaintenanceReleaseFinishesCurrentFileBeforeStopping(t *testing.T) {
	for _, failStorage := range []bool{false, true} {
		t.Run(fmt.Sprint(failStorage), func(t *testing.T) {
			oldDir, err := os.Getwd()
			require.NoError(t, err)
			require.NoError(t, os.Chdir(t.TempDir()))
			t.Cleanup(func() { require.NoError(t, os.Chdir(oldDir)) })
			signal := maintenance.NewSignal()
			original := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = original })
			http.DefaultTransport = maintenanceTransport(func(r *http.Request) (*http.Response, error) {
				body := ""
				switch r.URL.Path {
				case "/repos/test/repo/releases":
					body = `[{"id":1,"tag_name":"v1","published_at":"2026-10-20T00:00:00Z"}]`
				case "/repos/test/repo/releases/1/assets":
					body = `[{"id":11,"name":"first.bin","state":"uploaded","size":7},{"id":12,"name":"second.bin","state":"uploaded","size":7}]`
				case "/repos/test/repo/releases/assets/11":
					signal.Request()
					require.NoError(t, r.Context().Err())
					body = "payload"
				default:
					return nil, fmt.Errorf("started another asset after maintenance: %s", r.URL)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			out := t.TempDir()
			out2 := t.TempDir()
			stale := filepath.Join(out, "github.com/test/repo/release/old/keep.bin")
			require.NoError(t, os.MkdirAll(filepath.Dir(stale), 0700))
			require.NoError(t, os.WriteFile(stale, []byte("old"), 0600))
			if failStorage {
				out2 = filepath.Join(out2, "blocked")
				require.NoError(t, os.WriteFile(out2, []byte("blocks storage"), 0600))
			}
			ctx := config.WithExecutionSnapshot(maintenance.WithSignal(context.Background(), signal), config.NewExecutionSnapshot(&config.Config{GitHubToken: "test", ReleaseNumLimit: -1, ReleaseSizeLimit: -1}))
			storages := []typedef.MultiStorage{{Storage: typedef.Storage{Type: "file", Path: out}}, {Storage: typedef.Storage{Type: "file", Path: out2}}}
			err = DownloadAllAssets(ctx, typedef.Repository{URL: "github.com/test/repo"}, storages)
			if failStorage {
				require.Error(t, err)
				require.NotErrorIs(t, err, maintenance.ErrRequested)
			} else {
				require.ErrorIs(t, err, maintenance.ErrRequested)
				data, err := os.ReadFile(filepath.Join(out2, "github.com/test/repo/release/v1/first.bin"))
				require.NoError(t, err)
				require.Equal(t, "payload", string(data))
			}
			data, err := os.ReadFile(filepath.Join(out, "github.com/test/repo/release/v1/first.bin"))
			require.NoError(t, err)
			require.Equal(t, "payload", string(data))
			_, err = os.Stat(stale)
			require.NoError(t, err, "soft stop must skip stale-release cleanup")
			require.NoError(t, ctx.Err())
		})
	}
}

func TestMaintenanceReleasePreRequestedSkipsWork(t *testing.T) {
	signal := maintenance.NewSignal()
	signal.Request()
	err := DownloadAllAssets(maintenance.WithSignal(context.Background(), signal), typedef.Repository{URL: "invalid URL"}, nil)
	require.ErrorIs(t, err, maintenance.ErrRequested)
}

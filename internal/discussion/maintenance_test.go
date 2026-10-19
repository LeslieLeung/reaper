package discussion

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shurcooL/githubv4"
	"github.com/stretchr/testify/require"
	"github.com/wnarutou/gitrieve/internal/config"
	"github.com/wnarutou/gitrieve/internal/maintenance"
	"github.com/wnarutou/gitrieve/internal/typedef"
)

func TestDiscussionListRequestsAscendingUpdates(t *testing.T) {
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		query = body.Query
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"repository":{"discussions":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}`))
	}))
	defer server.Close()
	client := githubv4.NewEnterpriseClient(server.URL, server.Client())
	err := client.Query(context.Background(), &discussionsQuery{}, map[string]interface{}{
		"owner": githubv4.String("owner"), "name": githubv4.String("repo"), "discussionCount": githubv4.Int(50), "discussionCursor": (*githubv4.String)(nil),
	})
	require.NoError(t, err)
	require.Contains(t, strings.ReplaceAll(query, " ", ""), "orderBy:{field:UPDATED_AT,direction:ASC}")
}

type maintenanceTransport func(*http.Request) (*http.Response, error)

func (f maintenanceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func discussionFixture(t *testing.T, signal **maintenance.Signal, mode *string) context.Context {
	t.Helper()
	oldDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(t.TempDir()))
	t.Cleanup(func() { require.NoError(t, os.Chdir(oldDir)) })
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = maintenanceTransport(func(r *http.Request) (*http.Response, error) {
		var body struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return nil, err
		}
		var result string
		switch {
		case strings.Contains(body.Query, "replies("):
			id := 10
			if body.Variables["commentCursor"] != nil {
				id = 20
			}
			more := body.Variables["replyCursor"] == nil
			text := "last reply"
			if more {
				text = "first reply"
			}
			result = fmt.Sprintf(`{"discussion":{"comments":{"nodes":[{"databaseId":%d,"replies":{"nodes":[{"databaseId":99,"body":%q}],"pageInfo":{"hasNextPage":%t,"endCursor":"reply-next"}}}]}}}`, id, text, more)
		case strings.Contains(body.Query, "comments("):
			if *mode == "stop" {
				(*signal).Request()
			}
			require.NoError(t, r.Context().Err())
			more := body.Variables["commentCursor"] == nil
			id := 20
			if more {
				id = 10
			}
			result = fmt.Sprintf(`{"discussion":{"comments":{"nodes":[{"databaseId":%d,"body":"comment"}],"pageInfo":{"hasNextPage":%t,"endCursor":"comment-next"}}}}`, id, more)
		default:
			if body.Variables["discussionCursor"] != nil && *mode == "stop" {
				return nil, fmt.Errorf("started another discussion page after soft stop")
			}
			nodes := `[{"number":1,"title":"one","updatedAt":"2026-10-20T01:00:00Z"},{"number":2,"title":"two","updatedAt":"2026-10-20T01:00:00Z"}]`
			more := true
			if body.Variables["discussionCursor"] != nil {
				nodes = `[{"number":3,"title":"three","updatedAt":"2026-10-20T01:00:00Z"}]`
				more = false
			}
			if *mode == "empty" {
				nodes = `[]`
				more = false
			}
			result = fmt.Sprintf(`{"discussions":{"nodes":%s,"pageInfo":{"hasNextPage":%t,"endCursor":"discussion-next"}}}`, nodes, more)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"data":{"repository":` + result + `}}`)), Request: r}, nil
	})
	return config.WithExecutionSnapshot(maintenance.WithSignal(context.Background(), *signal), config.NewExecutionSnapshot(&config.Config{GitHubToken: "test"}))
}

func discussionArchive(t *testing.T, name string) map[string]string {
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

func TestMaintenanceFinishesDiscussionPageAndAllReplies(t *testing.T) {
	signal := maintenance.NewSignal()
	mode := "stop"
	ctx := discussionFixture(t, &signal, &mode)
	out := t.TempDir()
	repo := typedef.Repository{URL: "github.com/test/repo", UseCache: true}
	storages := []typedef.MultiStorage{{Storage: typedef.Storage{Type: "file", Path: out}}}
	require.ErrorIs(t, Sync(ctx, repo, storages), maintenance.ErrRequested)
	files := discussionArchive(t, filepath.Join(out, "github.com/test/repo/discussions.tar.gz"))
	for _, name := range []string{"discussion/1.md", "discussion/2.md"} {
		require.Equal(t, 2, strings.Count(files[name], "last reply"), "all reply and comment pages must finish")
	}
	mode = "resume"
	signal = maintenance.NewSignal()
	ctx = config.WithExecutionSnapshot(maintenance.WithSignal(context.Background(), signal), config.NewExecutionSnapshot(&config.Config{GitHubToken: "test"}))
	require.NoError(t, Sync(ctx, repo, storages))
	files = discussionArchive(t, filepath.Join(out, "github.com/test/repo/discussions.tar.gz"))
	require.Contains(t, files["discussion/3.md"], "last reply", "same-time record on the next page must not be skipped")
}

func TestDiscussionPublicationFailureRetriesWithoutUpdates(t *testing.T) {
	signal := maintenance.NewSignal()
	mode := "stop"
	ctx := discussionFixture(t, &signal, &mode)
	out := filepath.Join(t.TempDir(), "storage")
	require.NoError(t, os.WriteFile(out, []byte("blocks directory"), 0600))
	repo := typedef.Repository{URL: "github.com/test/repo", UseCache: true}
	storages := []typedef.MultiStorage{{Storage: typedef.Storage{Type: "file", Path: out}}}
	err := Sync(ctx, repo, storages)
	require.Error(t, err)
	require.NotErrorIs(t, err, maintenance.ErrRequested)
	require.ErrorContains(t, err, out)
	require.NoError(t, os.Remove(out))
	mode = "empty"
	ctx = config.WithExecutionSnapshot(context.Background(), config.NewExecutionSnapshot(&config.Config{GitHubToken: "test"}))
	require.NoError(t, Sync(ctx, repo, storages))
	files := discussionArchive(t, filepath.Join(out, "github.com/test/repo/discussions.tar.gz"))
	require.Contains(t, files["discussion/2.md"], "comment")
}

func TestMaintenanceDiscussionPreRequestedSkipsWork(t *testing.T) {
	signal := maintenance.NewSignal()
	signal.Request()
	err := Sync(maintenance.WithSignal(context.Background(), signal), typedef.Repository{URL: "invalid URL"}, nil)
	require.ErrorIs(t, err, maintenance.ErrRequested)
}

func TestMaintenanceNoCacheDiscussionPreservesCanonicalUntilListingCompletes(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(fmt.Sprint(complete), func(t *testing.T) {
			signal := maintenance.NewSignal()
			mode := "stop"
			ctx := discussionFixture(t, &signal, &mode)
			if complete {
				transport := http.DefaultTransport
				http.DefaultTransport = maintenanceTransport(func(r *http.Request) (*http.Response, error) {
					resp, err := transport.RoundTrip(r)
					if resp != nil {
						body, readErr := io.ReadAll(resp.Body)
						if readErr != nil {
							return nil, readErr
						}
						_ = resp.Body.Close()
						// Only the outer list loses its next page; nested comment
						// and reply pagination must still finish in full.
						text := string(body)
						if strings.Contains(text, `"discussions"`) {
							text = strings.ReplaceAll(text, `"hasNextPage":true`, `"hasNextPage":false`)
						}
						resp.Body = io.NopCloser(strings.NewReader(text))
					}
					return resp, err
				})
			}
			out := t.TempDir()
			canonical := filepath.Join(out, "github.com/test/repo/discussions.tar.gz")
			require.NoError(t, os.MkdirAll(filepath.Dir(canonical), 0700))
			require.NoError(t, os.WriteFile(canonical, []byte("previous complete archive"), 0600))
			err := Sync(ctx, typedef.Repository{URL: "github.com/test/repo", UseCache: false}, []typedef.MultiStorage{{Storage: typedef.Storage{Type: "file", Path: out}}})
			require.ErrorIs(t, err, maintenance.ErrRequested)
			partial, err := filepath.Glob(filepath.Join(filepath.Dir(canonical), "discussions.partial.*.tar.gz"))
			require.NoError(t, err)
			archive := canonical
			if complete {
				require.Empty(t, partial)
			} else {
				previous, err := os.ReadFile(canonical)
				require.NoError(t, err)
				require.Equal(t, "previous complete archive", string(previous))
				require.Len(t, partial, 1)
				archive = partial[0]
			}
			files := discussionArchive(t, archive)
			for _, name := range []string{"discussion/1.md", "discussion/2.md"} {
				require.Equal(t, 2, strings.Count(files[name], "last reply"))
			}
		})
	}
}

package issue

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v56/github"
	"github.com/stretchr/testify/require"
	"github.com/wnarutou/gitrieve/internal/config"
	"github.com/wnarutou/gitrieve/internal/lock"
	"github.com/wnarutou/gitrieve/internal/scm"
	"github.com/wnarutou/gitrieve/internal/typedef"
)

type issueRoundTripper func(*http.Request) (*http.Response, error)

func (fn issueRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestSyncFollowsCompleteNextIssueLink(t *testing.T) {
	for _, tc := range []struct {
		name        string
		query       string
		incremental bool
		retry       bool
		failure     bool
	}{
		{name: "page_and_cursor", query: "direction=asc&per_page=100&sort=updated&state=all&after=opaque%2Bcursor%2F%3D&page=100"},
		{name: "cursor_only", query: "direction=asc&per_page=100&sort=updated&state=all&after=opaque%2Bcursor%2F%3D"},
		{name: "page_only", query: "direction=asc&per_page=100&sort=updated&state=all&page=2"},
		{name: "incremental", query: "direction=asc&per_page=100&sort=updated&state=all&since=2026-01-01T00%3A00%3A00Z&after=opaque%3D", incremental: true},
		{name: "retry_next_page", query: "after=opaque%2Bcursor%2F%3D&page=100", retry: true},
		{name: "next_page_error", query: "after=opaque%3D", failure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			originalDir, err := os.Getwd()
			require.NoError(t, err)
			workingDir := t.TempDir()
			require.NoError(t, os.Chdir(workingDir))
			t.Cleanup(func() { require.NoError(t, os.Chdir(originalDir)) })
			issuesDir := filepath.Join(workingDir, ".gitrieve", "github.com", "test", "repo", "issues")
			if tc.incremental {
				require.NoError(t, os.MkdirAll(issuesDir, 0755))
				require.NoError(t, os.WriteFile(filepath.Join(issuesDir, "#0.md"), []byte("- Updated Time: 2026-01-01 00:00:00\n"), 0644))
			}

			// GitHub's Link uses the repository ID path, not the initial owner/name path.
			nextURL := "https://api.github.com/repositories/123/issues?" + tc.query
			listCalls := 0
			previousTransport := http.DefaultTransport
			http.DefaultTransport = issueRoundTripper(func(req *http.Request) (*http.Response, error) {
				require.Equal(t, "Bearer test-token", req.Header.Get("Authorization"))
				response := &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Request:    req,
				}
				body := "[]"
				if !strings.HasSuffix(req.URL.Path, "/comments") {
					listCalls++
					switch listCalls {
					case 1:
						require.Equal(t, "/repos/test/repo/issues", req.URL.Path)
						require.Equal(t, "all", req.URL.Query().Get("state"))
						require.Equal(t, "updated", req.URL.Query().Get("sort"))
						require.Equal(t, "asc", req.URL.Query().Get("direction"))
						require.Equal(t, "100", req.URL.Query().Get("per_page"))
						if tc.incremental {
							require.Equal(t, "2026-01-01T00:00:00Z", req.URL.Query().Get("since"))
						} else {
							require.NotContains(t, req.URL.Query(), "since")
						}
						response.Header.Set("Link", `<https://api.github.com/repositories/123/issues?page=1>; rel="first", <`+nextURL+`>; rel="next"`)
					case 2, 3:
						require.True(t, listCalls == 2 || tc.retry, "unexpected extra issues request")
						if req.URL.String() != nextURL {
							response.StatusCode = http.StatusUnprocessableEntity
							body = `{"message":"Pagination with the page parameter is not supported for large datasets"}`
							response.Body = io.NopCloser(strings.NewReader(body))
							return response, nil
						}
						if tc.failure || (tc.retry && listCalls == 2) {
							response.StatusCode = http.StatusUnprocessableEntity
							if tc.retry {
								response.StatusCode = http.StatusServiceUnavailable
							}
							response.Body = io.NopCloser(strings.NewReader(`{"message":"next page failed"}`))
							return response, nil
						}
						// A previous-page link must not cause another request.
						response.Header.Set("Link", `<https://api.github.com/repositories/123/issues?before=previous>; rel="prev"`)
					default:
						t.Fatalf("unexpected extra issues request: %s", req.URL)
					}
					issueNumber := min(listCalls, 2)
					body = fmt.Sprintf(`[{"number":%d,"title":"Issue %d","body":"page content","state":"open","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","user":{"login":"test"}}]`, issueNumber, issueNumber)
				}
				response.Body = io.NopCloser(strings.NewReader(body))
				return response, nil
			})
			t.Cleanup(func() { http.DefaultTransport = previousTransport })
			ctx := config.WithExecutionSnapshot(context.Background(), config.NewExecutionSnapshot(&config.Config{
				GitHubToken: "test-token", RetryMaxCount: 1, RetryBaseDelay: time.Millisecond,
			}))
			err = Sync(ctx, typedef.Repository{URL: "github.com/test/repo", UseCache: true}, nil)
			if tc.failure {
				var apiErr *gh.ErrorResponse
				require.ErrorAs(t, err, &apiErr)
				require.Equal(t, http.StatusUnprocessableEntity, apiErr.Response.StatusCode)
				require.Equal(t, 2, listCalls, "permanent errors must not be retried")
				require.NoFileExists(t, filepath.Join(issuesDir, "#2.md"))
				return
			}
			require.NoError(t, err)
			wantCalls := 2
			if tc.retry {
				wantCalls++
			}
			require.Equal(t, wantCalls, listCalls, "must consume every page, including cursor-only pages")
			for _, file := range []string{"#1.md", "#2.md"} {
				content, err := os.ReadFile(filepath.Join(issuesDir, file))
				require.NoError(t, err)
				require.Contains(t, string(content), "page content")
			}
		})
	}
}

func TestSyncCancelledContextReturnsImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := Sync(ctx, typedef.Repository{URL: "github.com/test/repo"}, nil)
	require.ErrorIs(t, err, context.Canceled)
}

func TestSyncBlocksWhileLockHeld(t *testing.T) {
	repo := typedef.Repository{URL: "github.com/test/repo", UseCache: true}
	r, err := scm.NewRepository(repo.URL)
	require.NoError(t, err)
	release, err := lock.Acquire(context.Background(), r, "issue")
	require.NoError(t, err)
	defer release()
	t.Cleanup(func() { os.RemoveAll(".gitrieve") })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err = Sync(ctx, repo, nil)
	require.Equal(t, context.DeadlineExceeded, err, "issue Sync must block on the held issue lock")
}

func captureIssueListQuery(t *testing.T, opt *gh.IssueListByRepoOptions) url.Values {
	t.Helper()
	var got url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte("[]")); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	client := gh.NewClient(server.Client())
	baseURL, err := url.Parse(server.URL + "/")
	require.NoError(t, err)
	client.BaseURL = baseURL
	_, _, err = client.Issues.ListByRepo(context.Background(), "owner", "repo", opt)
	require.NoError(t, err)
	return got
}

func TestNewIssueListOptionsInitialSyncOmitsSince(t *testing.T) {
	query := captureIssueListQuery(t, newIssueListOptions(time.Time{}))
	require.NotContains(t, query, "since")
	require.Equal(t, "all", query.Get("state"))
	require.Equal(t, "updated", query.Get("sort"))
	require.Equal(t, "asc", query.Get("direction"))
	require.Equal(t, "100", query.Get("per_page"))
}

func TestNewIssueListOptionsPreservesInstantInUTC(t *testing.T) {
	shanghai := time.FixedZone("Asia/Shanghai", 8*60*60)
	lastUpdate := time.Date(2026, 8, 17, 9, 30, 45, 0, shanghai)
	query := captureIssueListQuery(t, newIssueListOptions(lastUpdate))
	require.Equal(t, "2026-08-17T01:30:45Z", query.Get("since"))
}

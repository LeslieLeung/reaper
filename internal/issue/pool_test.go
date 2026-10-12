package issue

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/wnarutou/gitrieve/internal/githubapi/githubapitest"
	"github.com/wnarutou/gitrieve/internal/typedef"
)

func TestPoolIssueAndCommentPagination(t *testing.T) {
	for _, failure := range []string{"none", "primary", "permission"} {
		t.Run(failure, func(t *testing.T) {
			ctx, tokens := githubapitest.Context(t)
			var auth, pages []string
			githubapitest.Local(t, func(w http.ResponseWriter, r *http.Request) {
				auth = append(auth, r.Header.Get("Authorization"))
				pages = append(pages, r.URL.Path+"?"+r.URL.Query().Get("page"))
				w.Header().Set("Content-Type", "application/json")
				if len(auth) == 1 && failure != "none" {
					if failure == "primary" {
						w.Header().Set("X-RateLimit-Remaining", "0")
						w.Header().Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(time.Hour).Unix()))
						w.Header().Set("X-RateLimit-Resource", "core")
					}
					w.WriteHeader(403)
					fmt.Fprint(w, `{"message":"forbidden"}`)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/comments") {
					if r.URL.Query().Get("page") == "2" {
						fmt.Fprint(w, `[{"id":2,"body":"second comment"}]`)
						return
					}
					w.Header().Set("Link", `<https://api.github.com/repos/pool-issue/repo/issues/1/comments?page=2>; rel="next"`)
					fmt.Fprint(w, `[{"id":1,"body":"first comment"}]`)
					return
				}
				if r.URL.Query().Get("page") == "2" {
					fmt.Fprint(w, `[]`)
					return
				}
				w.Header().Set("Link", `<https://api.github.com/repos/pool-issue/repo/issues?page=2>; rel="next"`)
				fmt.Fprint(w, `[{"number":1,"title":"hello","updated_at":"2026-10-12T00:00:00Z"}]`)
			})
			cache := filepath.Join(".gitrieve", "github.com", "pool-issue", "repo")
			t.Cleanup(func() { _ = os.RemoveAll(cache) })
			require.NoError(t, Sync(ctx, typedef.Repository{URL: "github.com/pool-issue/repo", UseCache: true}, nil))
			data, err := os.ReadFile(filepath.Join(cache, "issues", "#1.md"))
			require.NoError(t, err)
			require.Contains(t, string(data), "- Comment Count: 2")
			require.Equal(t, 1, strings.Count(string(data), "first comment"))
			require.Equal(t, 1, strings.Count(string(data), "second comment"))
			a, b := "Bearer "+tokens[0], "Bearer "+tokens[1]
			switch failure {
			case "none":
				require.Equal(t, []string{a, b, a, b}, auth)
			case "primary":
				require.Equal(t, []string{a, b, b, b, b}, auth)
			case "permission":
				require.Equal(t, []string{a, b, a, b, a}, auth)
			}
			require.Equal(t, "/repos/pool-issue/repo/issues?2", pages[len(pages)-1])
			require.Contains(t, pages, "/repos/pool-issue/repo/issues/1/comments?2")
		})
	}
}

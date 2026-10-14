package discussion

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/wnarutou/gitrieve/internal/githubapi"
	"github.com/wnarutou/gitrieve/internal/githubapi/githubapitest"
	"github.com/wnarutou/gitrieve/internal/typedef"
)

func TestPoolGraphQLQueriesResetPartialDataAndKeepCursors(t *testing.T) {
	ctx, tokens := githubapitest.Context(t)
	var auth, cursors []string
	calls := 0
	githubapitest.Local(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		auth = append(auth, r.Header.Get("Authorization"))
		var request struct {
			Query     string
			Variables map[string]interface{}
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		w.Header().Set("Content-Type", "application/json")
		rate := `"rateLimit":{"cost":1,"remaining":1000,"resetAt":"2099-01-01T00:00:00Z"},`
		switch {
		case strings.Contains(request.Query, "replies("):
			fmt.Fprintf(w, `{"data":{%s"repository":{"discussion":{"comments":{"nodes":[{"databaseId":10,"replies":{"nodes":[{"databaseId":11,"body":"reply body"}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}]}}}}}`, rate)
		case strings.Contains(request.Query, "comments("):
			cursor, _ := request.Variables["commentCursor"].(string)
			cursors = append(cursors, "comment:"+cursor)
			if cursor != "" {
				fmt.Fprintf(w, `{"data":{%s"repository":{"discussion":{"comments":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`, rate)
				return
			}
			fmt.Fprintf(w, `{"data":{%s"repository":{"discussion":{"comments":{"nodes":[{"databaseId":10,"body":"comment body"}],"pageInfo":{"hasNextPage":true,"endCursor":"comment-next"}}}}}}`, rate)
		default:
			cursor, _ := request.Variables["discussionCursor"].(string)
			cursors = append(cursors, "discussion:"+cursor)
			if calls == 1 {
				// githubv4 can decode partial data before surfacing the errors array.
				fmt.Fprint(w, `{"data":{"repository":{"discussions":{"nodes":[{"number":999,"title":"stale partial","updatedAt":"2026-10-12T01:00:00Z"}],"pageInfo":{"hasNextPage":true,"endCursor":"stale"}}}},"errors":[{"message":"Resource not accessible by integration"}]}`)
				return
			}
			if cursor != "" {
				fmt.Fprintf(w, `{"data":{%s"repository":{"discussions":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`, rate)
				return
			}
			fmt.Fprintf(w, `{"data":{%s"repository":{"discussions":{"nodes":[{"number":1,"title":"hello","body":"discussion body","updatedAt":"2026-10-12T01:00:00Z"}],"pageInfo":{"hasNextPage":true,"endCursor":"discussion-next"}}}}}`, rate)
		}
	})
	cache := filepath.Join(".gitrieve", "github.com", "pool-discussion", "repo")
	t.Cleanup(func() { _ = os.RemoveAll(cache) })
	require.NoError(t, Sync(ctx, typedef.Repository{URL: "github.com/pool-discussion/repo", UseCache: true}, nil))
	data, err := os.ReadFile(filepath.Join(cache, "discussion", "1.md"))
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(data), "comment body"))
	require.Equal(t, 1, strings.Count(string(data), "reply body"))
	_, err = os.Stat(filepath.Join(cache, "discussion", "999.md"))
	require.True(t, os.IsNotExist(err))
	require.Equal(t, []string{"discussion:", "discussion:", "comment:", "comment:comment-next", "discussion:discussion-next"}, cursors)
	// oauth2 authenticates each fresh GraphQL client with the selected token.
	require.Equal(t, []string{"Bearer " + tokens[0], "Bearer " + tokens[1], "Bearer " + tokens[0], "Bearer " + tokens[1], "Bearer " + tokens[0], "Bearer " + tokens[1]}, auth)
}

func TestPoolGraphQLSuccessfulQuotaStaysOnSelectedResource(t *testing.T) {
	ctx, tokens := githubapitest.Context(t)
	var auth []string
	githubapitest.Local(t, func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		remaining := 1000
		if len(auth) == 1 {
			remaining = 0
		}
		fmt.Fprintf(w, `{"data":{"rateLimit":{"cost":1,"remaining":%d,"resetAt":"2099-01-01T00:00:00Z"},"repository":{"discussions":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`, remaining)
	})
	cache := filepath.Join(".gitrieve", "github.com", "pool-quota", "repo")
	t.Cleanup(func() { _ = os.RemoveAll(cache) })
	repo := typedef.Repository{URL: "github.com/pool-quota/repo", UseCache: true}
	require.NoError(t, Sync(ctx, repo, nil))
	require.NoError(t, Sync(ctx, repo, nil))
	require.Equal(t, []string{"Bearer " + tokens[0], "Bearer " + tokens[1]}, auth)
	permit, err := githubapi.AcquireAccount(ctx, "core", []githubapi.Account{{Name: "a", Token: tokens[0]}}, nil)
	require.NoError(t, err)
	permit.Done(githubapi.Observation{})
}

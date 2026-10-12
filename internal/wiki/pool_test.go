package wiki

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/wnarutou/gitrieve/internal/githubapi/githubapitest"
	"github.com/wnarutou/gitrieve/internal/syncresult"
	"github.com/wnarutou/gitrieve/internal/typedef"
)

func TestPoolWikiPermissionFallback(t *testing.T) {
	ctx, tokens := githubapitest.Context(t)
	var auth []string
	githubapitest.Local(t, func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		if len(auth) == 1 {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message":"missing"}`)
			return
		}
		fmt.Fprint(w, `{"has_wiki":false}`)
	})
	err := Sync(ctx, typedef.Repository{URL: "github.com/pool-wiki/repo"}, nil)
	_, skipped := syncresult.SkippedReason(err)
	require.True(t, skipped)
	require.Equal(t, []string{"Bearer " + tokens[0], "Bearer " + tokens[1]}, auth)
}

package repository

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitserver "github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/stretchr/testify/require"
	"github.com/wnarutou/gitrieve/internal/config"
	"github.com/wnarutou/gitrieve/internal/githubapi"
	"github.com/wnarutou/gitrieve/internal/githubapi/githubapitest"
	"github.com/wnarutou/gitrieve/internal/typedef"
)

// The fixture serves real Git advertisements and packfiles. It catches missing
// Auth options and clone retries that reuse a partially initialized directory.
type accountGitRequests struct {
	mu     sync.Mutex
	tokens []string
}

type accountGitHTTPTransport struct{ base http.RoundTripper }

type accountGitRoundTripFunc func(*http.Request) (*http.Response, error)

func (f accountGitRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (t accountGitHTTPTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if response != nil {
		// Local routing changes the dial target, not the logical URL Git sees.
		response.Request = request
	}
	return response, err
}

func (r *accountGitRequests) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.tokens...)
}

func serveAccountGit(t *testing.T, status func(string, *http.Request) int) (*git.Repository, *accountGitRequests) {
	t.Helper()
	upstream, err := git.PlainInit(t.TempDir(), false)
	require.NoError(t, err)
	commitAccountGit(t, upstream, "first version")
	endpoint, err := transport.NewEndpoint("https://github.com/git-account-tests/source")
	require.NoError(t, err)
	backend := gitserver.NewServer(gitserver.MapLoader{endpoint.String(): upstream.Storer})
	seen := &accountGitRequests{}
	githubapitest.Local(t, func(w http.ResponseWriter, r *http.Request) {
		_, token, _ := r.BasicAuth()
		seen.mu.Lock()
		seen.tokens = append(seen.tokens, token)
		code := status(token, r)
		seen.mu.Unlock()
		if code != 0 {
			if target := r.Header.Get("X-Fixture-Redirect"); target != "" {
				w.Header().Set("Location", target)
			}
			http.Error(w, "repository access denied", code)
			return
		}
		session, err := backend.NewUploadPackSession(endpoint, nil)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		defer session.Close()
		if strings.HasSuffix(r.URL.Path, "/info/refs") {
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			encoder := pktline.NewEncoder(w)
			if err := encoder.EncodeString("# service=git-upload-pack\n"); err != nil {
				t.Error(err)
				return
			}
			if err := encoder.Flush(); err != nil {
				t.Error(err)
				return
			}
			refs, err := session.AdvertisedReferencesContext(r.Context())
			if err != nil {
				t.Error(err)
				return
			}
			if err := refs.Encode(w); err != nil {
				t.Error(err)
			}
			return
		}
		request := packp.NewUploadPackRequest()
		if err := request.Decode(r.Body); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		response, err := session.UploadPack(r.Context(), request)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		defer response.Close()
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		if err := response.Encode(w); err != nil {
			t.Error(err)
		}
	})
	// go-git captures DefaultTransport at package initialization, so replacing
	// net/http's default alone does not intercept its requests.
	original := gitclient.Protocols["https"]
	http.DefaultTransport = accountGitHTTPTransport{base: http.DefaultTransport}
	gitclient.InstallProtocol("https", &githubGitTransport{base: githttp.NewClient(&http.Client{Transport: http.DefaultTransport})})
	t.Cleanup(func() { gitclient.InstallProtocol("https", original) })
	return upstream, seen
}

func commitAccountGit(t *testing.T, repository *git.Repository, content string) {
	t.Helper()
	worktree, err := repository.Worktree()
	require.NoError(t, err)
	file, err := worktree.Filesystem.Create("content.txt")
	require.NoError(t, err)
	_, err = file.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, file.Close())
	_, err = worktree.Add("content.txt")
	require.NoError(t, err)
	_, err = worktree.Commit(content, &git.CommitOptions{Author: &object.Signature{Name: "fixture", Email: "fixture@example.test", When: time.Now()}})
	require.NoError(t, err)
}

func accountGitRepo(t *testing.T) (typedef.Repository, string) {
	t.Helper()
	name := strings.ReplaceAll(t.Name(), "/", "-")
	repo := typedef.Repository{URL: "github.com/git-account-tests/" + name, UseCache: true}
	cache := filepath.Join(".gitrieve", "github.com", "git-account-tests", name)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(cache)) })
	return repo, filepath.Join(cache, "code")
}

func TestGitAccountCloneFallbackKeepsSuccessfulCredential(t *testing.T) {
	for _, code := range []int{401, 403, 404} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			ctx, tokens := githubapitest.Context(t)
			_, seen := serveAccountGit(t, func(token string, _ *http.Request) int {
				if token != tokens[1] {
					return code
				}
				return 0
			})
			repo, directory := accountGitRepo(t)
			require.NoError(t, Sync(ctx, repo, false, nil))
			content, err := os.ReadFile(filepath.Join(directory, "content.txt"))
			require.NoError(t, err)
			require.Equal(t, "first version", string(content))
			captured := seen.snapshot()
			require.Greater(t, len(captured), 2)
			require.Equal(t, tokens[0], captured[0])
			for _, token := range captured[1:] {
				require.Equal(t, tokens[1], token, "successful credential must persist through fetch/list/pull")
			}
		})
	}
}

func TestGitAccountCachedSyncChangesCredentialWithoutRecloning(t *testing.T) {
	ctx, tokens := githubapitest.Context(t)
	allowed := tokens[0]
	upstream, seen := serveAccountGit(t, func(token string, _ *http.Request) int {
		if token != allowed {
			return 404
		}
		return 0
	})
	repo, directory := accountGitRepo(t)
	require.NoError(t, Sync(ctx, repo, false, nil))
	// An existing file would disappear if the fallback incorrectly removed the
	// previous checkout rather than retrying fetch on the same object database.
	marker := filepath.Join(directory, git.GitDirName, "preserved.marker")
	require.NoError(t, os.WriteFile(marker, []byte("keep"), 0600))
	commitAccountGit(t, upstream, "second version")
	seen.mu.Lock()
	allowed = tokens[1]
	seen.tokens = nil
	seen.mu.Unlock()
	require.NoError(t, Sync(ctx, repo, false, nil))
	content, err := os.ReadFile(filepath.Join(directory, "content.txt"))
	require.NoError(t, err)
	require.Equal(t, "second version", string(content))
	_, err = os.Stat(marker)
	require.NoError(t, err)
	captured := seen.snapshot()
	require.Equal(t, tokens[0], captured[0])
	for _, token := range captured[1:] {
		require.Equal(t, tokens[1], token)
	}
}

func TestGitAccountNonPermissionFailureDoesNotRotate(t *testing.T) {
	ctx, tokens := githubapitest.Context(t)
	_, seen := serveAccountGit(t, func(string, *http.Request) int { return 503 })
	repo, _ := accountGitRepo(t)
	require.Error(t, Sync(ctx, repo, false, nil))
	require.Equal(t, []string{tokens[0]}, seen.snapshot())
}

func TestGitAccountAllDeniedAttemptsEachTokenOnce(t *testing.T) {
	cfg := &config.Config{GitHubAccounts: []githubapi.Account{{Name: "a", Token: "first"}, {Name: "alias", Token: "first"}, {Name: "b", Token: "second"}}}
	ctx := config.WithExecutionSnapshot(context.Background(), config.NewExecutionSnapshot(cfg))
	_, seen := serveAccountGit(t, func(string, *http.Request) int { return 404 })
	repo, directory := accountGitRepo(t)
	err := Sync(ctx, repo, false, nil)
	require.ErrorIs(t, err, transport.ErrRepositoryNotFound)
	require.Equal(t, []string{"first", "second"}, seen.snapshot())
	_, err = os.Stat(directory)
	require.True(t, os.IsNotExist(err), "failed fresh clone must be cleaned up")
}

func TestGitAccountFetchListAndPullEachFallback(t *testing.T) {
	ctx, tokens := githubapitest.Context(t)
	stage := false
	advertisements := 0
	_, seen := serveAccountGit(t, func(_ string, request *http.Request) int {
		if stage && strings.HasSuffix(request.URL.Path, "/info/refs") {
			advertisements++
			switch advertisements {
			case 1:
				return 404 // fetch: A -> B
			case 3:
				return 401 // list: B -> A
			case 5:
				return 403 // pull: A -> B
			}
		}
		return 0
	})
	repo, _ := accountGitRepo(t)
	require.NoError(t, Sync(ctx, repo, false, nil))
	seen.mu.Lock()
	stage = true
	seen.tokens = nil
	seen.mu.Unlock()
	require.NoError(t, Sync(ctx, repo, false, nil))
	require.Equal(t, []string{tokens[0], tokens[1], tokens[1], tokens[0], tokens[0], tokens[1]}, seen.snapshot())
}

func TestGitAccountWikiCacheSurvivesCredentialChange(t *testing.T) {
	ctx, tokens := githubapitest.Context(t)
	allowed := tokens[0]
	_, seen := serveAccountGit(t, func(token string, _ *http.Request) int {
		if token != allowed {
			return 404
		}
		return 0
	})
	repo, directory := accountGitRepo(t)
	directory = filepath.Join(filepath.Dir(directory), "wiki")
	require.NoError(t, Sync(ctx, repo, true, nil))
	marker := filepath.Join(directory, git.GitDirName, "preserved.marker")
	require.NoError(t, os.WriteFile(marker, []byte("keep"), 0600))
	seen.mu.Lock()
	allowed = tokens[1]
	seen.tokens = nil
	seen.mu.Unlock()
	require.NoError(t, Sync(ctx, repo, true, nil))
	_, err := os.Stat(marker)
	require.NoError(t, err)
	captured := seen.snapshot()
	require.Equal(t, tokens[0], captured[0])
	require.Contains(t, captured, tokens[1])
}

func TestGitAccountPullAllDeniedFailsAndPreservesCache(t *testing.T) {
	ctx, tokens := githubapitest.Context(t)
	stage := false
	advertisements := 0
	_, seen := serveAccountGit(t, func(_ string, request *http.Request) int {
		if stage && strings.HasSuffix(request.URL.Path, "/info/refs") {
			advertisements++
			if advertisements >= 3 {
				return 403 // Fetch and list succeed; both pull credentials fail.
			}
		}
		return 0
	})
	repo, directory := accountGitRepo(t)
	repo.AllBranches = true
	require.NoError(t, Sync(ctx, repo, false, nil))
	marker := filepath.Join(directory, git.GitDirName, "preserved.marker")
	require.NoError(t, os.WriteFile(marker, []byte("keep"), 0600))
	seen.mu.Lock()
	stage = true
	seen.tokens = nil
	seen.mu.Unlock()
	err := Sync(ctx, repo, false, nil)
	require.ErrorIs(t, err, transport.ErrAuthorizationFailed)
	require.Equal(t, []string{tokens[0], tokens[0], tokens[0], tokens[1]}, seen.snapshot())
	_, err = os.Stat(marker)
	require.NoError(t, err)
}

func TestGitAccountClonePackFailureRestartsWithFixedCredential(t *testing.T) {
	ctx, tokens := githubapitest.Context(t)
	_, seen := serveAccountGit(t, func(token string, request *http.Request) int {
		if token == tokens[0] && request.Method == http.MethodPost {
			return 401
		}
		return 0
	})
	repo, directory := accountGitRepo(t)
	require.NoError(t, Sync(ctx, repo, false, nil))
	content, err := os.ReadFile(filepath.Join(directory, "content.txt"))
	require.NoError(t, err)
	require.Equal(t, "first version", string(content))
	captured := seen.snapshot()
	require.GreaterOrEqual(t, len(captured), 4)
	require.Equal(t, []string{tokens[0], tokens[0]}, captured[:2], "one clone keeps its auth across advertisement and pack request")
	for _, token := range captured[2:] {
		require.Equal(t, tokens[1], token)
	}
}

func TestGitAccountLegacyAnonymousAndOtherHosts(t *testing.T) {
	for _, test := range []struct {
		name string
		cfg  *config.Config
		host string
		want string
	}{
		{"anonymous", &config.Config{}, "github.com", ""},
		{"legacy", &config.Config{GitHubToken: "legacy-token"}, "github.com", "legacy-token"},
		{"other-host", &config.Config{GitHubAccounts: []githubapi.Account{{Name: "a", Token: "github-only"}}}, "gitlab.com", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := config.WithExecutionSnapshot(context.Background(), config.NewExecutionSnapshot(test.cfg))
			_, seen := serveAccountGit(t, func(token string, _ *http.Request) int {
				if token != test.want {
					return 401
				}
				return 0
			})
			repo, _ := accountGitRepo(t)
			if test.host != "github.com" {
				repo.URL = strings.Replace(repo.URL, "github.com", test.host, 1)
				cache := filepath.Join(".gitrieve", test.host, "git-account-tests", filepath.Base(repo.URL))
				t.Cleanup(func() { require.NoError(t, os.RemoveAll(cache)) })
			}
			require.NoError(t, Sync(ctx, repo, false, nil))
			captured := seen.snapshot()
			require.NotEmpty(t, captured)
			for _, token := range captured {
				require.Equal(t, test.want, token)
			}
		})
	}
}

func TestGitAccountCancellationStopsBeforeNextCredential(t *testing.T) {
	ctx, tokens := githubapitest.Context(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	_, seen := serveAccountGit(t, func(string, *http.Request) int {
		cancel()
		return 401
	})
	repo, _ := accountGitRepo(t)
	require.ErrorIs(t, Sync(ctx, repo, false, nil), context.Canceled)
	require.Equal(t, []string{tokens[0]}, seen.snapshot())
}

func TestGitAccountCachedRemoteOtherHostReceivesNoCredential(t *testing.T) {
	ctx, _ := githubapitest.Context(t)
	stage := false
	_, seen := serveAccountGit(t, func(token string, _ *http.Request) int {
		if stage && token != "" {
			return 401
		}
		return 0
	})
	repo, directory := accountGitRepo(t)
	require.NoError(t, Sync(ctx, repo, false, nil))
	cached, err := git.PlainOpen(directory)
	require.NoError(t, err)
	require.NoError(t, cached.DeleteRemote("origin"))
	_, err = cached.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{"https://gitlab.com/git-account-tests/source"}})
	require.NoError(t, err)
	seen.mu.Lock()
	stage = true
	seen.tokens = nil
	seen.mu.Unlock()
	require.NoError(t, Sync(ctx, repo, false, nil))
	require.NotEmpty(t, seen.snapshot())
	for _, token := range seen.snapshot() {
		require.Empty(t, token, "cached origin URL must also constrain credential use")
	}
}

func TestGitAccountRedirectCannotSendCredentialOutsideHTTPSGitHub(t *testing.T) {
	for _, target := range []string{"https://gitlab.com", "http://github.com"} {
		t.Run(strings.NewReplacer("/", "-", ":", "-").Replace(target), func(t *testing.T) {
			ctx, tokens := githubapitest.Context(t)
			_, seen := serveAccountGit(t, func(_ string, request *http.Request) int {
				if !strings.HasPrefix(request.URL.Path, "/redirected/") {
					request.Header.Set("X-Fixture-Redirect", target+"/redirected/source/info/refs?service=git-upload-pack")
					return 302
				}
				return 0
			})
			repo, _ := accountGitRepo(t)
			require.ErrorIs(t, Sync(ctx, repo, false, nil), transport.ErrAuthorizationFailed)
			require.Equal(t, []string{tokens[0], tokens[1]}, seen.snapshot(), "only the two trusted initial requests may reach the server")
		})
	}
}

func TestGitAccountRedirectWithinHTTPSGitHubKeepsCredential(t *testing.T) {
	ctx, tokens := githubapitest.Context(t)
	_, seen := serveAccountGit(t, func(_ string, request *http.Request) int {
		if !strings.HasPrefix(request.URL.Path, "/redirected/") {
			request.Header.Set("X-Fixture-Redirect", "https://github.com/redirected/source/info/refs?service=git-upload-pack")
			return 302
		}
		return 0
	})
	repo, directory := accountGitRepo(t)
	require.NoError(t, Sync(ctx, repo, false, nil))
	content, err := os.ReadFile(filepath.Join(directory, "content.txt"))
	require.NoError(t, err)
	require.Equal(t, "first version", string(content))
	captured := seen.snapshot()
	require.GreaterOrEqual(t, len(captured), 3, "redirect and subsequent pack request must both complete")
	for _, token := range captured {
		require.Equal(t, tokens[0], token)
	}
}

func TestGitAccountEOFKeepsCredentialBeforePermissionFallback(t *testing.T) {
	ctx, tokens := githubapitest.Context(t)
	_, _ = serveAccountGit(t, func(token string, _ *http.Request) int {
		if token == tokens[0] {
			return http.StatusUnauthorized
		}
		return 0
	})
	base := http.DefaultTransport
	var captured []string
	http.DefaultTransport = accountGitRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		_, token, _ := request.BasicAuth()
		captured = append(captured, token)
		if len(captured) == 1 {
			return nil, io.ErrUnexpectedEOF
		}
		return base.RoundTrip(request)
	})
	repo, directory := accountGitRepo(t)
	require.NoError(t, Sync(ctx, repo, false, nil))
	require.GreaterOrEqual(t, len(captured), 4)
	require.Equal(t, []string{tokens[0], tokens[0], tokens[1]}, captured[:3])
	for _, token := range captured[3:] {
		require.Equal(t, tokens[1], token)
	}
	content, err := os.ReadFile(filepath.Join(directory, "content.txt"))
	require.NoError(t, err)
	require.Equal(t, "first version", string(content))
}

func TestGitAccountLocalRecoveryFailureStopsFallback(t *testing.T) {
	for _, failure := range []error{errCloneCleanup, errPackCleanup, errManagedIndexRecovery, errManagedWorktreeRecovery} {
		t.Run(failure.Error(), func(t *testing.T) {
			accounts := &gitAccounts{tokens: []string{"first", "second"}}
			calls := 0
			joined := errors.Join(transport.ErrAuthenticationRequired, failure)
			err := accounts.do(context.Background(), func(transport.AuthMethod) error {
				calls++
				return joined
			})
			require.ErrorIs(t, err, failure)
			require.Equal(t, 1, calls, "unsafe local recovery must stop before the next credential")
		})
	}
}

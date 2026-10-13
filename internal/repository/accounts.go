package repository

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/wnarutou/gitrieve/internal/config"
)

// gitAccounts belongs to one sync. Each network operation fixes its credential
// for its entire lifetime, and later operations prefer the last successful one.
// Git transfers use repository-task concurrency, not the short API permits.
type gitAccounts struct {
	tokens    []string
	preferred int
}

func newGitAccounts(ctx context.Context, host string) *gitAccounts {
	accounts := &gitAccounts{}
	// Never send GitHub credentials to another repository host.
	if gitHubHTTPSURL("https://" + host) {
		seen := make(map[string]bool)
		for _, account := range config.GetGitHubAccountsContext(ctx) {
			if !seen[account.Token] {
				accounts.tokens = append(accounts.tokens, account.Token)
				seen[account.Token] = true
			}
		}
	}
	if len(accounts.tokens) == 0 {
		accounts.tokens = []string{""}
	}
	return accounts
}

func gitHubHTTPSURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	return err == nil && parsed.Scheme == "https" && strings.EqualFold(parsed.Hostname(), "github.com")
}

// A cached checkout can have a different origin URL than the configured repo.
// Its actual URLs must be trusted before passing any GitHub credential to Git.
func (a *gitAccounts) restrictRemote(urls []string) {
	for _, remoteURL := range urls {
		if !gitHubHTTPSURL(remoteURL) {
			a.tokens = []string{""}
			a.preferred = 0
			return
		}
	}
}

func (a *gitAccounts) do(ctx context.Context, operation func(transport.AuthMethod) error) error {
	var firstErr error
	for offset := 0; offset < len(a.tokens); offset++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		index := (a.preferred + offset) % len(a.tokens)
		var auth transport.AuthMethod
		if token := a.tokens[index]; token != "" {
			auth = &githubGitAuth{BasicAuth: &githttp.BasicAuth{Username: "x-access-token", Password: token}}
		}
		err := operation(auth)
		if err == nil || errors.Is(err, git.NoErrAlreadyUpToDate) {
			a.preferred = index
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A joined permission error cannot make retrying safe when a local
		// packfile, clone directory, or managed worktree failed to recover.
		if errors.Is(err, errCloneCleanup) || errors.Is(err, errPackCleanup) ||
			errors.Is(err, errManagedIndexRecovery) || errors.Is(err, errManagedWorktreeRecovery) ||
			!gitAccountError(err) {
			return err
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func gitAccountError(err error) bool {
	return errors.Is(err, transport.ErrRepositoryNotFound) ||
		errors.Is(err, transport.ErrAuthenticationRequired) ||
		errors.Is(err, transport.ErrAuthorizationFailed)
}

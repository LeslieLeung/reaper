package wiki

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing/transport"
	gh "github.com/google/go-github/v56/github"
	"github.com/wnarutou/gitrieve/internal/config"
	"github.com/wnarutou/gitrieve/internal/githubapi"
	"github.com/wnarutou/gitrieve/internal/repository"
	"github.com/wnarutou/gitrieve/internal/retry"
	"github.com/wnarutou/gitrieve/internal/scm"
	"github.com/wnarutou/gitrieve/internal/syncresult"
	"github.com/wnarutou/gitrieve/internal/typedef"
	"github.com/wnarutou/gitrieve/internal/ui"
)

var syncRepository = repository.Sync

func wikiAvailability(repoURL string, hasWiki bool) error {
	if hasWiki {
		return nil
	}
	return syncresult.Skip(fmt.Sprintf("repository %s has no wiki", repoURL))
}

func uninitializedPublicWiki(hasWiki, private bool, err error) bool {
	if !hasWiki || private {
		return false
	}
	// Only accept the transport error and its ordinary wrappers. Joined errors
	// can also contain local cleanup failures that must still fail the job.
	for err != nil {
		if err == transport.ErrAuthenticationRequired || err == transport.ErrRepositoryNotFound {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
}

func Sync(ctx context.Context, repo typedef.Repository, storages []typedef.MultiStorage) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// get the repo name from the URL
	r, err := scm.NewRepository(repo.URL)
	if err != nil {
		return err
	}
	repoName := r.Name
	if repoName == "." || repoName == "/" {
		ui.Errorf("Invalid repository name")
		return err
	}

	accounts := config.GetGitHubAccountsContext(ctx)
	baseClient := gh.NewClient(nil)

	var gitrepo *gh.Repository
	err = retry.Do(ctx, config.GetRetryConfigContext(ctx), func() error {
		return githubapi.Do(ctx, "core", accounts, func(token string) (githubapi.Observation, error) {
			client := githubapi.NewREST(baseClient, token)
			var response *gh.Response
			var apiErr error
			gitrepo, response, apiErr = client.Repositories.Get(ctx, r.Owner, r.Name)
			return githubapi.ObserveREST(response, apiErr), apiErr
		})
	})
	if err != nil {
		ui.Errorf("Get repository %s fail", repo.URL)
		return err
	}

	if err := wikiAvailability(repo.URL, gitrepo.GetHasWiki()); err != nil {
		return err
	}

	ui.Printf("Running %s's wiki", repo.Name)
	if err := syncRepository(ctx, repo, true, storages); err != nil {
		if uninitializedPublicWiki(gitrepo.GetHasWiki(), gitrepo.GetPrivate(), err) {
			ui.Printf("Wiki for %s is enabled but has no pages", repo.URL)
			return nil
		}
		if ctx.Err() == nil {
			ui.Errorf("Error running %s's wiki, %s", repo.Name, err)
		}
		return err
	}
	return nil
}

package github

import (
	"context"
	"io"
	"net/http"
	"net/url"

	"github.com/google/go-github/v56/github"
	"github.com/wnarutou/gitrieve/internal/config"
	"github.com/wnarutou/gitrieve/internal/githubapi"
	"github.com/wnarutou/gitrieve/internal/retry"
	"github.com/wnarutou/gitrieve/internal/typedef"
)

type Client struct {
	c        *github.Client
	accounts []githubapi.Account
}

func New() (*Client, error) {
	return NewWithContext(context.Background())
}

func NewWithContext(ctx context.Context) (*Client, error) {
	c := github.NewClient(nil)
	return &Client{c: c, accounts: config.GetGitHubAccountsContext(ctx)}, nil
}

func (c *Client) GetRepos(name string, accountType string) ([]string, error) {
	return c.GetReposContext(context.Background(), name, accountType)
}

func (c *Client) GetReposContext(ctx context.Context, name string, accountType string) ([]string, error) {
	var (
		list []*github.Repository
		err  error
	)
	err = retry.Do(ctx, config.GetRetryConfigContext(ctx), func() error {
		return githubapi.Do(ctx, "core", c.accounts, func(token string) (githubapi.Observation, error) {
			client := githubapi.NewREST(c.c, token)
			var response *github.Response
			var apiErr error
			if accountType == typedef.TypeOrg {
				list, response, apiErr = client.Repositories.ListByOrg(ctx, name, nil)
			} else {
				list, response, apiErr = client.Repositories.List(ctx, name, nil)
			}
			return githubapi.ObserveREST(response, apiErr), apiErr
		})
	})
	if err != nil {
		return nil, err
	}
	repos := make([]string, 0)
	for _, repo := range list {
		htmlURL := repo.GetHTMLURL()
		URL, err := url.Parse(htmlURL)
		if err != nil {
			return nil, err
		}
		repos = append(repos, URL.Hostname()+URL.Path)
	}
	return repos, nil
}

func (c *Client) GetReleases(ctx context.Context, owner, repo string) ([]*github.RepositoryRelease, error) {
	var (
		list []*github.RepositoryRelease
		err  error
	)
	err = retry.Do(ctx, config.GetRetryConfigContext(ctx), func() error {
		return githubapi.Do(ctx, "core", c.accounts, func(token string) (githubapi.Observation, error) {
			client := githubapi.NewREST(c.c, token)
			var apiErr error
			var resp *github.Response
			list, resp, apiErr = client.Repositories.ListReleases(ctx, owner, repo, nil)
			return githubapi.ObserveREST(resp, apiErr), apiErr
		})
	})
	if err != nil {
		return nil, err
	}
	return list, nil
}

func (c *Client) GetReleaseAssets(ctx context.Context, owner, repo string, id int64) ([]*github.ReleaseAsset, error) {
	var (
		list []*github.ReleaseAsset
		err  error
	)
	err = retry.Do(ctx, config.GetRetryConfigContext(ctx), func() error {
		return githubapi.Do(ctx, "core", c.accounts, func(token string) (githubapi.Observation, error) {
			client := githubapi.NewREST(c.c, token)
			var apiErr error
			var resp *github.Response
			list, resp, apiErr = client.Repositories.ListReleaseAssets(ctx, owner, repo, id, nil)
			return githubapi.ObserveREST(resp, apiErr), apiErr
		})
	})
	if err != nil {
		return nil, err
	}
	return list, nil
}

func (c *Client) DownloadAsset(ctx context.Context, owner, repo string, id int64) (io.ReadCloser, error) {
	var rc io.ReadCloser
	err := retry.Do(ctx, config.GetRetryConfigContext(ctx), func() error {
		var redirectURL string
		err := githubapi.Do(ctx, "core", c.accounts, func(token string) (githubapi.Observation, error) {
			client := githubapi.NewREST(c.c, token)
			var apiErr error
			rc, redirectURL, apiErr = client.Repositories.DownloadReleaseAsset(ctx, owner, repo, id, nil)
			return githubapi.ObserveError(apiErr), apiErr
		})
		if err != nil || redirectURL == "" {
			return err
		}
		// API authorization has completed. The asset body transfer does not
		// occupy an account/global API permit or forward the GitHub credential.
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, redirectURL, nil)
		if err != nil {
			return err
		}
		request.Header.Set("Accept", "*/*")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return err
		}
		if err := github.CheckResponse(response); err != nil {
			response.Body.Close()
			return err
		}
		rc = response.Body
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rc, nil
}

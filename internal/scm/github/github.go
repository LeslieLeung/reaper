package github

import (
	"context"
	"github.com/google/go-github/v56/github"
	"github.com/leslieleung/reaper/internal/config"
	"github.com/leslieleung/reaper/internal/typedef"
	"io"
	"net/http"
	"net/url"
	"sync"
)

type Client struct {
	c *github.Client
}

var once sync.Once
var client *Client

func New() (*Client, error) {
	once.Do(func() {
		cfg := config.GetIns()
		client = &Client{
			c: github.NewClient(nil).WithAuthToken(cfg.GitHubToken),
		}
		if cfg.GitHubToken != "" {
			client.c = client.c.WithAuthToken(cfg.GitHubToken)
		}
	})
	return client, nil
}

func (c *Client) GetRepos(name string, accountType string) ([]string, error) {
	var (
		list []*github.Repository
		err  error
	)
	if accountType == typedef.TypeOrg {
		list, _, err = c.c.Repositories.ListByOrg(context.Background(), name, nil)
	} else {
		list, _, err = c.c.Repositories.List(context.Background(), name, nil)
	}
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

func (c *Client) GetReleases(owner, repo string) ([]*github.RepositoryRelease, error) {
	var (
		list []*github.RepositoryRelease
		err  error
	)
	list, _, err = c.c.Repositories.ListReleases(context.Background(), owner, repo, nil)
	if err != nil {
		return nil, err
	}
	return list, nil
}

func (c *Client) GetReleaseAssets(owner, repo string, id int64) ([]*github.ReleaseAsset, error) {
	var (
		list []*github.ReleaseAsset
		err  error
	)
	list, _, err = c.c.Repositories.ListReleaseAssets(context.Background(), owner, repo, id, nil)
	if err != nil {
		return nil, err
	}
	return list, nil
}

func (c *Client) DownloadAsset(owner, repo string, id int64) (io.ReadCloser, error) {
	rc, _, err := c.c.Repositories.DownloadReleaseAsset(context.Background(), owner, repo, id, http.DefaultClient)
	if err != nil {
		return nil, err
	}
	return rc, nil
}

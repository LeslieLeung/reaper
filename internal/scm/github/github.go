package github

import (
	"context"
	"github.com/google/go-github/v56/github"
	"github.com/leslieleung/reaper/internal/config"
	"github.com/leslieleung/reaper/internal/typedef"
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

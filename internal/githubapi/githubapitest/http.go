// Package githubapitest supplies isolated local HTTP fixtures for API consumers.
package githubapitest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/wnarutou/gitrieve/internal/config"
	"github.com/wnarutou/gitrieve/internal/githubapi"
)

type transport func(*http.Request) (*http.Response, error)

func (fn transport) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

// Local routes all default HTTP traffic to a local server, so fixtures never
// contact GitHub even if a consumer uses a hardcoded production endpoint.
func Local(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	target, _ := url.Parse(server.URL)
	original := http.DefaultTransport
	http.DefaultTransport = transport(func(req *http.Request) (*http.Response, error) {
		copy := req.Clone(req.Context())
		copy.URL.Scheme, copy.URL.Host = target.Scheme, target.Host
		return server.Client().Transport.RoundTrip(copy)
	})
	t.Cleanup(func() { http.DefaultTransport = original })
}

func Context(t *testing.T) (context.Context, []string) {
	t.Helper()
	tokens := []string{t.Name() + "-a", t.Name() + "-b"}
	cfg := &config.Config{GitHubToken: "old-legacy", GitHubAccounts: []githubapi.Account{{Name: "a", Token: tokens[0]}, {Name: "b", Token: tokens[1]}}, GitHubAPIConcurrency: 2, GitHubAccountConcurrency: 1}
	githubapi.Configure(config.GitHubAPIConfigFrom(cfg))
	ctx := config.WithExecutionSnapshot(context.Background(), config.NewExecutionSnapshot(cfg))
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	t.Cleanup(cancel)
	return ctx, tokens
}

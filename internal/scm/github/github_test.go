package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/wnarutou/gitrieve/internal/config"
	"github.com/wnarutou/gitrieve/internal/githubapi"
	"github.com/wnarutou/gitrieve/internal/typedef"
)

func TestNewObservesReloadedToken(t *testing.T) {
	var auth []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer s.Close()
	base, err := url.Parse(s.URL + "/")
	require.NoError(t, err)

	for _, token := range []string{"first", "second"} {
		config.SetIns(&config.Config{GitHubToken: token, GitHubAPIConcurrency: 1})
		client, err := New()
		require.NoError(t, err)
		client.c.BaseURL = base
		_, err = client.GetReleases(context.Background(), "o", "r")
		require.NoError(t, err)
	}
	require.Equal(t, []string{"Bearer first", "Bearer second"}, auth)
}

func githubExecutionContext(parent context.Context, cfg *config.Config) context.Context {
	ctx := config.WithExecutionSnapshot(parent, config.NewExecutionSnapshot(cfg))
	return githubapi.WithScope(ctx, githubapi.NewScope(config.GitHubAPIConfigFrom(cfg)))
}

func TestGetReposContextCancellationInterruptsScopedPermitWait(t *testing.T) {
	githubapi.Configure(githubapi.Config{Concurrency: 1})
	held, err := githubapi.Acquire(context.Background(), "core")
	require.NoError(t, err)
	var releaseHeld sync.Once
	t.Cleanup(func() { releaseHeld.Do(func() { held.Done(githubapi.Observation{}) }) })

	requestSeen := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestSeen <- struct{}{}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer server.Close()
	base, err := url.Parse(server.URL + "/")
	require.NoError(t, err)
	cfg := &config.Config{RetryMaxCount: 0}
	ctx, cancel := context.WithCancel(githubExecutionContext(context.Background(), cfg))
	client, err := NewWithContext(ctx)
	require.NoError(t, err)
	client.c.BaseURL = base

	done := make(chan error, 1)
	go func() {
		_, listErr := client.GetReposContext(ctx, "acme", typedef.TypeOrg)
		done <- listErr
	}()
	select {
	case <-requestSeen:
		t.Fatal("repository listing reached HTTP while the scoped permit was held")
	case err := <-done:
		t.Fatalf("repository listing returned before cancellation: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt repository listing permit wait")
	}
	select {
	case <-requestSeen:
		t.Fatal("cancelled repository listing reached HTTP")
	default:
	}
}

func TestGetReposContextRetriesFromSnapshotAndObservesREST(t *testing.T) {
	githubapi.Configure(githubapi.Config{Concurrency: 1, LowRemainingThreshold: 1})
	previousConfig := config.GetIns()
	config.SetIns(&config.Config{RetryMaxCount: 0})
	t.Cleanup(func() { config.SetIns(previousConfig) })

	var calls int
	// Use a per-test resource so the process-wide coordinator's deliberately
	// persistent quota state cannot leak between repeated test runs. Leave a
	// generous reset window because GitHub reset timestamps have one-second
	// precision and Unix truncation can otherwise make a one-second pause expire
	// before the assertion acquires it.
	resource := fmt.Sprintf("test-core-%d", time.Now().UnixNano())
	reset := time.Now().Add(time.Minute).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"temporary"}`))
			return
		}
		w.Header().Set("X-RateLimit-Resource", resource)
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", reset))
		_, _ = w.Write([]byte(`[{"html_url":"https://github.com/acme/one"}]`))
	}))
	defer server.Close()
	base, err := url.Parse(server.URL + "/")
	require.NoError(t, err)
	cfg := &config.Config{RetryMaxCount: 1, RetryBaseDelay: time.Millisecond}
	ctx := githubExecutionContext(context.Background(), cfg)
	client, err := NewWithContext(ctx)
	require.NoError(t, err)
	client.c.BaseURL = base

	repositories, err := client.GetReposContext(ctx, "acme", typedef.TypeOrg)
	require.NoError(t, err)
	require.Equal(t, []string{"github.com/acme/one"}, repositories)
	require.Equal(t, 2, calls, "repository listing must use the job snapshot retry count")

	otherCtx, otherCancel := context.WithTimeout(context.Background(), time.Second)
	defer otherCancel()
	permit, err := githubapi.Acquire(otherCtx, "graphql")
	require.NoError(t, err, "REST observation did not release the process permit")
	permit.Done(githubapi.Observation{})
	coreCtx, coreCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer coreCancel()
	permit, err = githubapi.Acquire(coreCtx, resource)
	if permit != nil {
		permit.Done(githubapi.Observation{})
	}
	require.ErrorIs(t, err, context.DeadlineExceeded, "repository listing response was not observed by the quota arbiter")
}

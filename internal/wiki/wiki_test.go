package wiki

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	gitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/stretchr/testify/require"
	"github.com/wnarutou/gitrieve/internal/config"
	"github.com/wnarutou/gitrieve/internal/githubapi"
	"github.com/wnarutou/gitrieve/internal/syncresult"
	"github.com/wnarutou/gitrieve/internal/typedef"
	"github.com/wnarutou/gitrieve/internal/ui"
)

type wikiRoundTripper func(*http.Request) (*http.Response, error)

func (fn wikiRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type wikiLogSink struct {
	logs []string
}

func (s *wikiLogSink) Log(_, _, level, message string) error {
	s.logs = append(s.logs, level+":"+message)
	return nil
}

func TestWikiAvailabilitySkipsRepositoryWithoutWiki(t *testing.T) {
	err := wikiAvailability("github.com/test/repo", false)
	reason, ok := syncresult.SkippedReason(err)

	require.True(t, ok)
	require.Equal(t, "repository github.com/test/repo has no wiki", reason)
	require.NoError(t, wikiAvailability("github.com/test/repo", true))
}

func TestSyncSkipsUnavailableWikiWithoutPresentationOrRepositorySync(t *testing.T) {
	previousConfig := config.GetIns()
	config.SetIns(&config.Config{})
	if previousConfig != nil {
		t.Cleanup(func() { config.SetIns(previousConfig) })
	}

	previousTransport := http.DefaultTransport
	http.DefaultTransport = wikiRoundTripper(func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "/repos/test/repo", req.URL.Path)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"has_wiki":false}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	sink := &wikiLogSink{}
	ui.SetSink(sink)
	t.Cleanup(func() { ui.SetSink(nil) })
	unbind := ui.Bind("execution-1", "wiki")
	defer unbind()

	err := Sync(context.Background(), typedef.Repository{Name: "repo", URL: "github.com/test/repo"}, nil)
	reason, ok := syncresult.SkippedReason(err)

	require.True(t, ok)
	require.Equal(t, "repository github.com/test/repo has no wiki", reason)
	require.Empty(t, sink.logs)
}

func TestSyncCompletesWhenPublicWikiIsEnabledButHasNoPages(t *testing.T) {
	previousConfig := config.GetIns()
	config.SetIns(&config.Config{})
	if previousConfig != nil {
		t.Cleanup(func() { config.SetIns(previousConfig) })
	}

	previousTransport := http.DefaultTransport
	http.DefaultTransport = wikiRoundTripper(func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "/repos/test/repo", req.URL.Path)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"has_wiki":true,"private":false}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	previousSyncRepository := syncRepository
	syncRepository = func(context.Context, typedef.Repository, bool, []typedef.MultiStorage) error {
		return fmt.Errorf("clone wiki: %w: Repository not found", transport.ErrAuthenticationRequired)
	}
	t.Cleanup(func() { syncRepository = previousSyncRepository })

	sink := &wikiLogSink{}
	ui.SetSink(sink)
	t.Cleanup(func() { ui.SetSink(nil) })
	unbind := ui.Bind("execution-1", "wiki")
	defer unbind()

	err := Sync(context.Background(), typedef.Repository{Name: "repo", URL: "github.com/test/repo"}, nil)

	require.NoError(t, err)
	require.Contains(t, sink.logs, "info:Wiki for github.com/test/repo is enabled but has no pages")
}

func TestSyncHandlesMissingWikiFromGitHTTP(t *testing.T) {
	for _, tc := range []struct {
		name    string
		private bool
		status  int
		wantErr error
	}{
		{name: "public missing wiki", status: http.StatusNotFound},
		{name: "public uninitialized wiki", status: http.StatusUnauthorized},
		{name: "private missing wiki", private: true, status: http.StatusNotFound, wantErr: transport.ErrRepositoryNotFound},
		{name: "private authentication failure", private: true, status: http.StatusUnauthorized, wantErr: transport.ErrAuthenticationRequired},
		{name: "public permission failure", status: http.StatusForbidden, wantErr: transport.ErrAuthorizationFailed},
		{name: "server failure", status: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previousDir, err := os.Getwd()
			require.NoError(t, err)
			require.NoError(t, os.Chdir(t.TempDir()))
			t.Cleanup(func() { require.NoError(t, os.Chdir(previousDir)) })

			roundTripper := wikiRoundTripper(func(req *http.Request) (*http.Response, error) {
				status := http.StatusOK
				body := fmt.Sprintf(`{"has_wiki":true,"private":%t}`, tc.private)
				switch req.URL.Host {
				case "api.github.com":
					require.Equal(t, "/repos/test/repo", req.URL.Path)
				case "github.com":
					require.Equal(t, "/test/repo.wiki/info/refs", req.URL.Path)
					status = tc.status
					body = "Repository not found"
				default:
					t.Fatalf("unexpected request: %s", req.URL)
				}
				return &http.Response{
					StatusCode: status,
					Body:       io.NopCloser(strings.NewReader(body)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			})
			previousTransport := http.DefaultTransport
			http.DefaultTransport = roundTripper
			t.Cleanup(func() { http.DefaultTransport = previousTransport })
			previousGitTransport := gitclient.Protocols["https"]
			gitclient.InstallProtocol("https", githttp.NewClient(&http.Client{Transport: roundTripper}))
			t.Cleanup(func() { gitclient.InstallProtocol("https", previousGitTransport) })

			sink := &wikiLogSink{}
			ui.SetSink(sink)
			t.Cleanup(func() { ui.SetSink(nil) })
			unbind := ui.Bind("execution-1", "wiki")
			defer unbind()

			err = Sync(wikiExecutionContext(context.Background(), &config.Config{}), typedef.Repository{
				Name: "repo", URL: "github.com/test/repo", UseCache: true,
			}, nil)
			if tc.status == http.StatusInternalServerError {
				require.ErrorContains(t, err, "status code: 500")
				return
			}
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Contains(t, sink.logs, "info:Wiki for github.com/test/repo is enabled but has no pages")
			for _, message := range sink.logs {
				require.NotContains(t, message, "error:")
			}
		})
	}
}

func TestUninitializedWikiDoesNotHidePrivateRepositoryAuthenticationFailure(t *testing.T) {
	err := fmt.Errorf("clone wiki: %w: Repository not found", transport.ErrAuthenticationRequired)

	require.False(t, uninitializedPublicWiki(true, true, err))
	require.True(t, uninitializedPublicWiki(true, false, err))
	require.False(t, uninitializedPublicWiki(false, false, err))
	require.False(t, uninitializedPublicWiki(true, false, errors.New("network down")))
}

func TestUninitializedWikiDoesNotHideJoinedCleanupFailure(t *testing.T) {
	for _, transportErr := range []error{transport.ErrRepositoryNotFound, transport.ErrAuthenticationRequired} {
		t.Run(transportErr.Error(), func(t *testing.T) {
			err := fmt.Errorf("clone wiki: %w", errors.Join(transportErr, errors.New("cleanup failed")))
			require.False(t, uninitializedPublicWiki(true, false, err))
		})
	}
}

func TestSyncCancelledContextReturnsImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := Sync(ctx, typedef.Repository{URL: "github.com/test/repo"}, nil)
	require.ErrorIs(t, err, context.Canceled)
}

func wikiExecutionContext(parent context.Context, cfg *config.Config) context.Context {
	ctx := config.WithExecutionSnapshot(parent, config.NewExecutionSnapshot(cfg))
	return githubapi.WithScope(ctx, githubapi.NewScope(config.GitHubAPIConfigFrom(cfg)))
}

func TestWikiPreflightWaitsForScopedPermitAndReleasesItAfterObservation(t *testing.T) {
	githubapi.Configure(githubapi.Config{Concurrency: 1})
	held, err := githubapi.Acquire(context.Background(), "core")
	require.NoError(t, err)
	var releaseHeld sync.Once
	t.Cleanup(func() { releaseHeld.Do(func() { held.Done(githubapi.Observation{}) }) })

	requestSeen := make(chan struct{}, 1)
	previousTransport := http.DefaultTransport
	http.DefaultTransport = wikiRoundTripper(func(req *http.Request) (*http.Response, error) {
		requestSeen <- struct{}{}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"has_wiki":false}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	cfg := &config.Config{RetryMaxCount: 0}
	done := make(chan error, 1)
	go func() {
		done <- Sync(wikiExecutionContext(context.Background(), cfg), typedef.Repository{Name: "repo", URL: "github.com/test/repo"}, nil)
	}()
	select {
	case <-requestSeen:
		t.Fatal("wiki preflight reached HTTP while the process-wide permit was held")
	case err := <-done:
		t.Fatalf("wiki preflight returned before the held permit was released: %v", err)
	case <-time.After(30 * time.Millisecond):
	}

	releaseHeld.Do(func() { held.Done(githubapi.Observation{}) })
	select {
	case err := <-done:
		_, skipped := syncresult.SkippedReason(err)
		require.True(t, skipped)
	case <-time.After(time.Second):
		t.Fatal("wiki preflight did not resume after the permit was released")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	permit, err := githubapi.Acquire(ctx, "core")
	require.NoError(t, err, "wiki observation did not release its permit")
	permit.Done(githubapi.Observation{})
}

func TestWikiPreflightCancellationInterruptsPermitWait(t *testing.T) {
	githubapi.Configure(githubapi.Config{Concurrency: 1})
	held, err := githubapi.Acquire(context.Background(), "core")
	require.NoError(t, err)
	defer held.Done(githubapi.Observation{})

	requestSeen := make(chan struct{}, 1)
	previousTransport := http.DefaultTransport
	http.DefaultTransport = wikiRoundTripper(func(req *http.Request) (*http.Response, error) {
		requestSeen <- struct{}{}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"has_wiki":false}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	ctx, cancel := context.WithCancel(wikiExecutionContext(context.Background(), &config.Config{}))
	done := make(chan error, 1)
	go func() {
		done <- Sync(ctx, typedef.Repository{Name: "repo", URL: "github.com/test/repo"}, nil)
	}()
	select {
	case <-requestSeen:
		t.Fatal("wiki preflight bypassed the held permit")
	case err := <-done:
		t.Fatalf("wiki preflight returned before cancellation: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt wiki preflight permit wait")
	}
	select {
	case <-requestSeen:
		t.Fatal("cancelled wiki preflight reached HTTP")
	default:
	}
}

func TestWikiPreflightRetriesFromJobSnapshot(t *testing.T) {
	githubapi.Configure(githubapi.Config{Concurrency: 1})
	previousConfig := config.GetIns()
	config.SetIns(&config.Config{RetryMaxCount: 0})
	t.Cleanup(func() { config.SetIns(previousConfig) })

	var calls int
	previousTransport := http.DefaultTransport
	http.DefaultTransport = wikiRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		status := http.StatusInternalServerError
		body := `{"message":"temporary"}`
		if calls == 2 {
			status = http.StatusOK
			body = `{"has_wiki":false}`
		}
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Request:    req,
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	cfg := &config.Config{RetryMaxCount: 1, RetryBaseDelay: time.Millisecond}
	err := Sync(wikiExecutionContext(context.Background(), cfg), typedef.Repository{Name: "repo", URL: "github.com/test/repo"}, nil)
	_, skipped := syncresult.SkippedReason(err)
	require.True(t, skipped)
	require.Equal(t, 2, calls, "wiki preflight must use the job snapshot retry count")
}

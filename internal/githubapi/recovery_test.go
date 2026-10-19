package githubapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-github/v56/github"
	"github.com/stretchr/testify/require"
	"github.com/wnarutou/gitrieve/internal/retry"
)

func TestAnonymousGraphQL401PreservesREST(t *testing.T) {
	installCoordinatorForTest(t, Config{Concurrency: 2})
	original := errors.New("non-200 OK status code: 401 Unauthorized body: Bad credentials")
	require.Same(t, original, Do(context.Background(), "graphql", nil, func(string) (Observation, error) { return Observation{}, original }))
	called := false
	require.NoError(t, Do(context.Background(), "core", nil, func(token string) (Observation, error) {
		require.Empty(t, token)
		called = true
		return Observation{}, nil
	}))
	require.True(t, called)
	p, err := Acquire(context.Background(), "core")
	require.NoError(t, err)
	p.Done(Observation{})
}

func TestPoolExhaustionExposesEarliestPrimaryReset(t *testing.T) {
	installCoordinatorForTest(t, Config{Concurrency: 2})
	first := errors.New("API rate limit exceeded: first")
	earliest := time.Now().Add(10 * time.Second)
	err := Do(context.Background(), "graphql", poolAccounts, func(token string) (Observation, error) {
		if token == "a" {
			return Observation{Reset: time.Now().Add(time.Hour)}, first
		}
		return Observation{Reset: earliest}, errors.New("API rate limit exceeded: second")
	})
	require.Equal(t, first.Error(), err.Error())
	require.ErrorIs(t, err, first)
	var deadline interface{ RetryAt() time.Time }
	require.ErrorAs(t, err, &deadline)
	require.Equal(t, earliest, deadline.RetryAt())
	requirePoolBlocked(t, "graphql", poolAccounts)
	acquirePool(t, "core", poolAccounts, nil).Done(Observation{})
}

func TestPoolPermissionThenPrimaryRecoversWithOuterRetry(t *testing.T) {
	for _, resource := range []string{"core", "graphql"} {
		t.Run(resource, func(t *testing.T) {
			installCoordinatorForTest(t, Config{Concurrency: 2})
			first := restAccountError(403)
			reset := time.Now().Add(30 * time.Millisecond)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			outer := 0
			var attempts []string
			err := retry.Do(ctx, retry.Config{MaxRetries: 1, BaseDelay: time.Hour}, func() error {
				outer++
				return Do(ctx, resource, poolAccounts, func(token string) (Observation, error) {
					attempts = append(attempts, token)
					if token == "a" {
						return Observation{}, first
					}
					if outer == 1 {
						return Observation{Reset: reset}, errors.New("API rate limit exceeded")
					}
					require.False(t, time.Now().Before(reset), "retry did not wait for reported reset")
					return Observation{}, nil
				})
			})
			require.NoError(t, err)
			require.Equal(t, 2, outer)
			require.Equal(t, []string{"a", "b", "a", "b"}, attempts)
		})
	}
}

func TestPoolTwoPrimaryFailuresRetryEarliestReset(t *testing.T) {
	installCoordinatorForTest(t, Config{Concurrency: 2})
	longReset, shortReset := time.Now().Add(time.Hour), time.Now().Add(30*time.Millisecond)
	first := &github.RateLimitError{Response: accountHTTPResponse(403), Rate: github.Rate{Reset: github.Timestamp{Time: longReset}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var attempts []string
	err := retry.Do(ctx, retry.Config{MaxRetries: 1, BaseDelay: time.Hour}, func() error {
		return Do(ctx, "core", poolAccounts, func(token string) (Observation, error) {
			attempts = append(attempts, token)
			if token == "a" {
				return Observation{}, first
			}
			if len(attempts) == 2 {
				return Observation{}, &github.RateLimitError{Response: accountHTTPResponse(403), Rate: github.Rate{Reset: github.Timestamp{Time: shortReset}}}
			}
			require.False(t, time.Now().Before(shortReset))
			return Observation{}, nil
		})
	})
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b", "b"}, attempts)
	requirePoolBlocked(t, "core", poolAccounts[:1])
	acquirePool(t, "graphql", poolAccounts[:1], nil).Done(Observation{})
}

// These callbacks are the network boundary; no requests leave the process.
func TestPoolFallbackAlreadyPausedAccountUsesEarlierRecovery(t *testing.T) {
	installCoordinatorForTest(t, Config{Concurrency: 2, AccountConcurrency: 1})
	held := acquirePool(t, "core", poolAccounts[1:], nil)
	held.Done(Observation{Resource: "core", Reset: time.Now().Add(time.Hour)})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	first := errors.New("API rate limit exceeded: first")
	reset := time.Now().Add(20 * time.Millisecond)
	outer, calls := 0, 0
	err := retry.Do(ctx, retry.Config{MaxRetries: 1, BaseDelay: time.Hour}, func() error {
		outer++
		return Do(ctx, "core", poolAccounts, func(token string) (Observation, error) {
			calls++
			require.Equal(t, "a", token, "long-paused B must not be requested")
			if calls == 1 {
				return Observation{Reset: reset}, first
			}
			require.False(t, time.Now().Before(reset))
			return Observation{}, nil
		})
	})
	require.NoError(t, err)
	require.Equal(t, 2, outer)
	require.Equal(t, 2, calls, "one distinct credential per outer attempt")
}

func TestPoolFallbackAccountBecomesPausedWhileWaiting(t *testing.T) {
	installCoordinatorForTest(t, Config{Concurrency: 2, AccountConcurrency: 1})
	held := acquirePool(t, "core", poolAccounts[1:], nil)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	first := errors.New("API rate limit exceeded: first")
	reset := time.Now().Add(80 * time.Millisecond)
	firstSeen := make(chan struct{})
	done := make(chan error, 1)
	calls, outer := 0, 0
	go func() {
		done <- retry.Do(ctx, retry.Config{MaxRetries: 1, BaseDelay: time.Hour}, func() error {
			outer++
			return Do(ctx, "core", poolAccounts, func(token string) (Observation, error) {
				calls++
				if token != "a" {
					return Observation{}, errors.New("requested long-paused B")
				}
				if calls == 1 {
					close(firstSeen)
					return Observation{Reset: reset}, first
				}
				if time.Now().Before(reset) {
					return Observation{}, errors.New("retried before A reset")
				}
				return Observation{}, nil
			})
		})
	}()
	<-firstSeen
	select {
	case err := <-done:
		t.Fatalf("fallback returned before B changed state: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	// Another request completes with a primary observation while Do is waiting
	// for its busy credential. The change signal must reconsider A's deadline.
	held.Done(Observation{Resource: "core", Reset: time.Now().Add(time.Hour)})
	require.NoError(t, <-done)
	require.Equal(t, 2, outer)
	require.Equal(t, 2, calls)
}

func TestPoolFallbackSharedPrimaryResetPreservesFirstPermissionError(t *testing.T) {
	installCoordinatorForTest(t, Config{Concurrency: 2})
	reset := time.Now().Add(time.Hour)
	acquirePool(t, "core", poolAccounts[1:], nil).Done(Observation{Resource: "core", Reset: reset})
	first := restAccountError(403)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	calls := 0
	err := Do(ctx, "core", poolAccounts, func(token string) (Observation, error) {
		calls++
		require.Equal(t, "a", token)
		return Observation{}, first
	})
	require.ErrorIs(t, err, first)
	require.Equal(t, first.Error(), err.Error())
	var deadline interface{ RetryAt() time.Time }
	require.ErrorAs(t, err, &deadline)
	require.Equal(t, reset, deadline.RetryAt())
	require.Equal(t, 1, calls)
	acquirePool(t, "core", poolAccounts[:1], nil).Done(Observation{})
}

func TestPoolFallbackBusyAccountReleasedBeforeRecoveryIsAttempted(t *testing.T) {
	installCoordinatorForTest(t, Config{Concurrency: 2, AccountConcurrency: 1})
	held := acquirePool(t, "core", poolAccounts[1:], nil)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	firstSeen := make(chan struct{})
	done := make(chan error, 1)
	var attempts []string
	go func() {
		done <- Do(ctx, "core", poolAccounts, func(token string) (Observation, error) {
			attempts = append(attempts, token)
			if token == "a" {
				close(firstSeen)
				return Observation{Reset: time.Now().Add(time.Hour)}, errors.New("API rate limit exceeded")
			}
			return Observation{}, nil
		})
	}()
	<-firstSeen
	select {
	case err := <-done:
		t.Fatalf("fallback did not wait for busy B: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	held.Done(Observation{})
	require.NoError(t, <-done)
	require.Equal(t, []string{"a", "b"}, attempts)
}

func TestPoolFallbackBusyAccountCannotDelayExpiredRecovery(t *testing.T) {
	installCoordinatorForTest(t, Config{Concurrency: 2, AccountConcurrency: 1})
	held := acquirePool(t, "core", poolAccounts[1:], nil)
	defer held.Done(Observation{})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	reset := time.Now().Add(20 * time.Millisecond)
	calls, outer := 0, 0
	err := retry.Do(ctx, retry.Config{MaxRetries: 1, BaseDelay: time.Hour}, func() error {
		outer++
		return Do(ctx, "core", poolAccounts, func(token string) (Observation, error) {
			calls++
			if token != "a" {
				return Observation{}, errors.New("requested busy B")
			}
			if calls == 1 {
				return Observation{Reset: reset}, errors.New("API rate limit exceeded")
			}
			return Observation{}, nil
		})
	})
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.Equal(t, 2, outer)
}

func TestPoolFallbackRecoveryStillHonorsSharedSecondaryPause(t *testing.T) {
	installCoordinatorForTest(t, Config{Concurrency: 2, AccountConcurrency: 1})
	held := acquirePool(t, "core", poolAccounts[1:], nil)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	firstSeen := make(chan struct{})
	done := make(chan error, 1)
	calls := 0
	go func() {
		done <- retry.Do(ctx, retry.Config{MaxRetries: 1, BaseDelay: time.Millisecond}, func() error {
			return Do(ctx, "core", poolAccounts, func(string) (Observation, error) {
				calls++
				if calls == 1 {
					close(firstSeen)
				}
				return Observation{Reset: time.Now().Add(30 * time.Millisecond)}, errors.New("API rate limit exceeded")
			})
		})
	}()
	<-firstSeen
	held.Done(Observation{Secondary: true, RetryAfter: time.Hour})
	require.ErrorIs(t, <-done, context.DeadlineExceeded)
	require.Equal(t, 1, calls, "primary recovery must never bypass the global secondary pause")
}

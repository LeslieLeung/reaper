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

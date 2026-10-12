package githubapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/go-github/v56/github"
	"github.com/stretchr/testify/require"
)

var poolAccounts = []Account{{Name: "first", Token: "a"}, {Name: "second", Token: "b"}}

func acquirePool(t *testing.T, resource string, accounts []Account, excluded map[string]bool) AccountPermit {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	p, err := AcquireAccount(ctx, resource, accounts, excluded)
	require.NoError(t, err)
	t.Cleanup(func() { p.Done(Observation{}) })
	return p
}

func requirePoolBlocked(t *testing.T, resource string, accounts []Account) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	p, err := AcquireAccount(ctx, resource, accounts, nil)
	if p != nil {
		p.Done(Observation{})
	}
	require.Nil(t, p)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// These tests catch quota state being shared between distinct credentials and
// slot reservation ignoring the credential cap.
func TestAccountQuotaExhaustionSelectsAnotherCredential(t *testing.T) {
	installCoordinatorForTest(t, Config{Concurrency: 2, AccountConcurrency: 1})
	p := acquirePool(t, "core", poolAccounts, nil)
	require.Equal(t, "a", p.Token())
	p.Done(Observation{Resource: "core", Remaining: 0, Reset: time.Now().Add(time.Hour)})
	p = acquirePool(t, "core", poolAccounts, nil)
	require.Equal(t, "b", p.Token())
}

func TestAccountSlotsAreReservedAtomically(t *testing.T) {
	installCoordinatorForTest(t, Config{Concurrency: 3, AccountConcurrency: 1})
	first := acquirePool(t, "core", poolAccounts, nil)
	second := acquirePool(t, "graphql", poolAccounts, nil)
	require.Equal(t, "a", first.Token())
	require.Equal(t, "b", second.Token())
	requirePoolBlocked(t, "core", poolAccounts)
	first.Done(Observation{})
	first.Done(Observation{})
	third := acquirePool(t, "core", poolAccounts, nil)
	require.Equal(t, "a", third.Token())
	requirePoolBlocked(t, "core", poolAccounts)
}

func TestAccountSelectionIsFairAndHonorsExclusions(t *testing.T) {
	installCoordinatorForTest(t, Config{Concurrency: 2})
	for _, want := range []string{"a", "b", "a", "b"} {
		p := acquirePool(t, "core", poolAccounts, nil)
		require.Equal(t, want, p.Token())
		p.Done(Observation{})
	}
	p := acquirePool(t, "core", poolAccounts, map[string]bool{"a": true})
	require.Equal(t, "b", p.Token())
}

func TestDuplicateTokensShareAccountSlots(t *testing.T) {
	installCoordinatorForTest(t, Config{Concurrency: 3, AccountConcurrency: 1})
	accounts := []Account{{Name: "one", Token: "a"}, {Name: "duplicate", Token: "a"}}
	acquirePool(t, "core", accounts, nil)
	requirePoolBlocked(t, "graphql", accounts)
}

func TestAccountAndLegacyCallsShareGlobalCap(t *testing.T) {
	installCoordinatorForTest(t, Config{Concurrency: 1, AccountConcurrency: 2})
	p, err := Acquire(context.Background(), "core")
	require.NoError(t, err)
	t.Cleanup(func() { p.Done(Observation{}) })
	requirePoolBlocked(t, "core", poolAccounts)
}

func TestUnnamedLegacyAccountsKeepGlobalConcurrency(t *testing.T) {
	for _, accounts := range [][]Account{nil, {{Token: "a"}}} {
		t.Run("token="+func() string {
			if len(accounts) == 0 {
				return ""
			}
			return accounts[0].Token
		}(), func(t *testing.T) {
			installCoordinatorForTest(t, Config{Concurrency: 3, AccountConcurrency: 1})
			for i := 0; i < 3; i++ {
				acquirePool(t, "core", accounts, nil)
			}
			requirePoolBlocked(t, "core", accounts)
		})
	}
}

func TestAccountPacingAndPrimaryQuotaAreCredentialLocal(t *testing.T) {
	installCoordinatorForTest(t, Config{Concurrency: 3, MinRequestInterval: time.Hour})
	p := acquirePool(t, "core", poolAccounts, nil)
	p.Done(Observation{})
	p = acquirePool(t, "graphql", poolAccounts, nil)
	require.Equal(t, "b", p.Token())
	p.Done(Observation{})
	requirePoolBlocked(t, "graphql", poolAccounts)

	Configure(Config{Concurrency: 3})
	// Reload cannot erase previously reserved pacing.
	requirePoolBlocked(t, "core", poolAccounts)
}

func TestAccountPrimaryQuotaIsResourceLocalAndSurvivesReload(t *testing.T) {
	installCoordinatorForTest(t, Config{Concurrency: 3})
	p := acquirePool(t, "core", poolAccounts[:1], nil)
	p.Done(Observation{Resource: "core", Reset: time.Now().Add(time.Hour)})
	Configure(Config{Concurrency: 3})
	require.Same(t, p.(*permit).c, current.Load())
	requirePoolBlocked(t, "core", poolAccounts[:1])
	p = acquirePool(t, "graphql", poolAccounts[:1], nil)
	p.Done(Observation{})
}

func TestAccountSecondaryPauseIsProcessWide(t *testing.T) {
	installCoordinatorForTest(t, Config{Concurrency: 3})
	p := acquirePool(t, "core", poolAccounts[:1], nil)
	p.Done(Observation{Secondary: true, RetryAfter: time.Hour})
	Configure(Config{Concurrency: 3})
	requirePoolBlocked(t, "graphql", poolAccounts[1:])
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := Acquire(ctx, "core")
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestCanceledAccountAcquisitionNeverReservesASlot(t *testing.T) {
	installCoordinatorForTest(t, Config{Concurrency: 1})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p, err := AcquireAccount(ctx, "core", poolAccounts, nil)
	require.Nil(t, p)
	require.ErrorIs(t, err, context.Canceled)
	acquirePool(t, "core", poolAccounts, nil)
}

func restAccountError(status int) error {
	return &github.ErrorResponse{Response: accountHTTPResponse(status), Message: http.StatusText(status)}
}

func accountHTTPResponse(status int) *http.Response {
	request, _ := http.NewRequest(http.MethodGet, "https://example.test/repository", nil)
	return &http.Response{StatusCode: status, Request: request, Header: make(http.Header)}
}

// The callback is the actual network boundary: these tests catch wrong selected
// credentials, unbounded replay, and errors accidentally treated as success.
func TestDoFallsBackForCredentialAndPermissionErrors(t *testing.T) {
	for _, status := range []int{401, 403, 404} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			installCoordinatorForTest(t, Config{Concurrency: 2})
			firstErr := restAccountError(status)
			var attempts []string
			var downloaded string
			err := Do(context.Background(), "core", poolAccounts, func(token string) (Observation, error) {
				attempts = append(attempts, token)
				if token == "a" {
					return Observation{}, fmt.Errorf("request failed: %w", firstErr)
				}
				downloaded = "data from b"
				return Observation{}, nil
			})
			require.NoError(t, err)
			require.Equal(t, []string{"a", "b"}, attempts)
			require.Equal(t, "data from b", downloaded)
			if status == 401 {
				Configure(Config{Concurrency: 2})
				_, err = AcquireAccount(context.Background(), "core", poolAccounts[:1], nil)
				require.ErrorIs(t, err, ErrNoAccountsAvailable)
				p := acquirePool(t, "core", []Account{{Name: "first", Token: "new-a"}}, nil)
				require.Equal(t, "new-a", p.Token())
			} else {
				p := acquirePool(t, "core", poolAccounts[:1], nil)
				require.Equal(t, "a", p.Token(), "permission failures must not disable credentials")
			}
		})
	}
}

func TestDoBoundsFallbackAndReturnsOriginalError(t *testing.T) {
	for _, firstErr := range []error{restAccountError(401), restAccountError(403), restAccountError(404),
		&github.RateLimitError{Response: accountHTTPResponse(403), Rate: github.Rate{Reset: github.Timestamp{Time: time.Now().Add(time.Hour)}}},
		errors.New("API rate limit exceeded"), errors.New("Resource not accessible by integration")} {
		t.Run(firstErr.Error(), func(t *testing.T) {
			installCoordinatorForTest(t, Config{Concurrency: 2})
			var attempts []string
			err := Do(context.Background(), "core", append(poolAccounts, Account{Name: "alias", Token: "a"}), func(token string) (Observation, error) {
				attempts = append(attempts, token)
				if token == "a" {
					return Observation{}, firstErr
				}
				return Observation{}, restAccountError(404)
			})
			require.Same(t, firstErr, err)
			require.Equal(t, []string{"a", "b"}, attempts)
		})
	}
}

func TestDoPrimaryExhaustionSkipsResetSleep(t *testing.T) {
	installCoordinatorForTest(t, Config{Concurrency: 2})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var attempts []string
	err := Do(ctx, "core", poolAccounts, func(token string) (Observation, error) {
		attempts = append(attempts, token)
		if token == "a" {
			return Observation{}, &github.RateLimitError{Response: accountHTTPResponse(403), Rate: github.Rate{Reset: github.Timestamp{Time: time.Now().Add(time.Hour)}}}
		}
		return Observation{}, nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, attempts)
	requirePoolBlocked(t, "core", poolAccounts[:1])
	acquirePool(t, "graphql", poolAccounts[:1], nil)
}

func TestDoSuccessfulQuotaObservationBelongsToSelectedAccount(t *testing.T) {
	installCoordinatorForTest(t, Config{Concurrency: 2})
	err := Do(context.Background(), "graphql", poolAccounts, func(token string) (Observation, error) {
		require.Equal(t, "a", token)
		return Observation{Resource: "graphql", Remaining: 0, Reset: time.Now().Add(time.Hour)}, nil
	})
	require.NoError(t, err)
	requirePoolBlocked(t, "graphql", poolAccounts[:1])
	p := acquirePool(t, "graphql", poolAccounts, nil)
	require.Equal(t, "b", p.Token())
}

func TestDoSecondaryAndTransientErrorsNeverRotate(t *testing.T) {
	for _, fixture := range []struct {
		name      string
		err       error
		secondary bool
	}{
		{"typed-secondary", &github.AbuseRateLimitError{Response: accountHTTPResponse(403)}, true},
		{"graphql-secondary", errors.New(`non-200 OK status code: 403 Forbidden body: {"errors":[{"message":"You have exceeded a secondary rate limit.","extensions":{"retryAfterSeconds":60}}]}`), true},
		{"graphql-429", errors.New("non-200 OK status code: 429 Too Many Requests body: throttled"), true},
		{"transient", restAccountError(503), false},
		{"generic", errors.New("connection reset"), false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			installCoordinatorForTest(t, Config{Concurrency: 2})
			var attempts []string
			err := Do(context.Background(), "graphql", poolAccounts, func(token string) (Observation, error) {
				attempts = append(attempts, token)
				return Observation{}, fixture.err
			})
			require.Same(t, fixture.err, err)
			require.Equal(t, []string{"a"}, attempts)
			if fixture.secondary {
				requirePoolBlocked(t, "core", poolAccounts[1:])
			}
		})
	}
}

func TestDoGraphQLKnownErrorsAndConservativeCooldown(t *testing.T) {
	for _, message := range []string{"API rate limit exceeded", "non-200 OK status code: 401 Unauthorized body: Bad credentials", "Bad credentials", "non-200 OK status code: 403 Forbidden body: forbidden", "non-200 OK status code: 404 Not Found body: missing", "Resource not accessible by personal access token", "Could not resolve to a Repository with the name 'private/repo'."} {
		t.Run(message, func(t *testing.T) {
			installCoordinatorForTest(t, Config{Concurrency: 2})
			var attempts []string
			err := Do(context.Background(), "graphql", poolAccounts, func(token string) (Observation, error) {
				attempts = append(attempts, token)
				if token == "a" {
					return Observation{}, errors.New(message)
				}
				return Observation{}, nil
			})
			require.NoError(t, err)
			require.Equal(t, []string{"a", "b"}, attempts)
			if message == "API rate limit exceeded" {
				requirePoolBlocked(t, "graphql", poolAccounts[:1])
				acquirePool(t, "core", poolAccounts[:1], nil)
			}
		})
	}
}

func TestDoCanceledContextNeverCallsNetworkCallback(t *testing.T) {
	installCoordinatorForTest(t, Config{Concurrency: 2})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Do(ctx, "core", poolAccounts, func(string) (Observation, error) {
		t.Fatal("canceled context sent a network request")
		return Observation{}, nil
	})
	require.ErrorIs(t, err, context.Canceled)
}

func TestNamedTokenCapAppliesToOldLegacySnapshots(t *testing.T) {
	installCoordinatorForTest(t, Config{Concurrency: 3, AccountConcurrency: 1})
	legacy := []Account{{Token: "a"}}
	first := acquirePool(t, "core", legacy, nil)
	requirePoolBlocked(t, "core", poolAccounts[:1])
	requirePoolBlocked(t, "graphql", legacy)
	first.Done(Observation{})
	acquirePool(t, "core", legacy, nil)
	requirePoolBlocked(t, "core", poolAccounts[:1])
}

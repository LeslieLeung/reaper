package githubapi

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v56/github"
)

// Account is an immutable execution-snapshot credential. An unnamed account
// represents legacy authentication and has no additional account concurrency cap
// unless the same token has also been used by a named pool account.
type Account struct {
	Name  string `yaml:"name" mapstructure:"name" json:"name"`
	Token string `yaml:"token" mapstructure:"token" json:"token"`
}

type AccountPermit interface {
	Permit
	Token() string
}

// ErrNoAccountsAvailable means every supplied credential is excluded or disabled.
// Busy, paced, or quota-paused credentials instead wait until available or canceled.
var ErrNoAccountsAvailable = errors.New("no GitHub accounts available")

type credentialState struct {
	active        uint
	nextStart     time.Time
	resourcePause map[string]time.Time
	disabled      bool
	named         bool
	lastSelected  uint64
}

func (p *permit) Token() string { return p.token }

// AcquireAccount atomically selects and reserves a credential and a global slot.
// excluded is keyed by exact token strings. An empty accounts slice uses anonymous
// REST authentication. Identical tokens always share pacing, quota, and slots.
func AcquireAccount(ctx context.Context, resource string, accounts []Account, excluded map[string]bool) (AccountPermit, error) {
	return loadCoordinator().acquireAccount(ctx, resource, accounts, excluded)
}

// Do wraps one network attempt, with bounded credential fallback for primary
// exhaustion, invalid authentication, and permission errors. Each unique token
// is tried at most once. Secondary and transient errors return to the caller's
// existing retry policy. Exhaustion returns the first request error.
func Do(ctx context.Context, resource string, accounts []Account, fn func(string) (Observation, error)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c := loadCoordinator()
	accounts = uniqueAccounts(accounts)
	excluded := make(map[string]bool, len(accounts))
	var firstErr error
	for attempt := 0; attempt < len(accounts); attempt++ {
		p, err := c.acquireAccount(ctx, resource, accounts, excluded)
		if err != nil {
			if errors.Is(err, ErrNoAccountsAvailable) && firstErr != nil {
				return firstErr
			}
			return err
		}
		failure, err := attemptAccount(ctx, p.(*permit), fn)
		if err == nil {
			return nil
		}
		if firstErr == nil {
			firstErr = err
		}
		if failure == noFallback {
			return err
		}
		excluded[p.Token()] = true
	}
	return firstErr
}

type accountFailure uint8

const (
	noFallback accountFailure = iota
	permissionFailure
	invalidCredential
	primaryExhaustion
)

func attemptAccount(ctx context.Context, p *permit, fn func(string) (Observation, error)) (accountFailure, error) {
	var obs Observation
	// A callback panic must not permanently occupy a process/account slot.
	defer func() { p.Done(obs) }()
	if err := ctx.Err(); err != nil {
		return noFallback, err
	}
	var err error
	obs, err = fn(p.Token())
	failure, observed := classifyAccountError(err, p.resource, obs)
	obs = observed
	err = normalizeRESTRateError(err, failure, obs)
	if failure == invalidCredential {
		p.c.mu.Lock()
		p.credential.disabled = true
		p.c.signalLocked()
		p.c.mu.Unlock()
	}
	return failure, err
}

func classifyAccountError(err error, resource string, obs Observation) (accountFailure, Observation) {
	if err == nil {
		return noFallback, obs
	}
	var abuse *github.AbuseRateLimitError
	if errors.As(err, &abuse) {
		obs.Secondary, obs.RetryAfter = true, time.Minute
		if abuse.RetryAfter != nil {
			obs.RetryAfter = *abuse.RetryAfter
		}
		return noFallback, obs
	}
	var limited *github.RateLimitError
	if errors.As(err, &limited) {
		if obs.Secondary || secondaryMessage(limited.Message) {
			return noFallback, secondaryRESTObservation(limited.Response, obs)
		}
		if obs.Reset.IsZero() {
			obs.Reset = limited.Rate.Reset.Time
		}
		return primaryObservation(resource, obs)
	}
	// v56 recognizes rate errors only for selected 403 forms. Use explicit
	// secondary evidence and primary quota headers for both 403 and 429.
	var response *github.ErrorResponse
	if errors.As(err, &response) {
		if response.Response == nil {
			return noFallback, obs
		}
		switch response.Response.StatusCode {
		case http.StatusUnauthorized:
			return invalidCredential, obs
		case http.StatusForbidden, http.StatusTooManyRequests:
			headers := response.Response.Header
			if obs.Secondary || secondaryMessage(response.Message) || (headers.Get("Retry-After") != "" && headers.Get("X-RateLimit-Remaining") != "0") {
				return noFallback, secondaryRESTObservation(response.Response, obs)
			}
			if headers.Get("X-RateLimit-Remaining") == "0" || (obs.Limit > 0 && obs.Remaining == 0 && !obs.Reset.IsZero()) {
				if obs.Resource == "" {
					obs.Resource = headers.Get("X-RateLimit-Resource")
				}
				if obs.Reset.IsZero() {
					if seconds, parseErr := strconv.ParseInt(headers.Get("X-RateLimit-Reset"), 10, 64); parseErr == nil {
						obs.Reset = time.Unix(seconds, 0)
					}
				}
				if obs.Limit == 0 {
					obs.Limit, _ = strconv.Atoi(headers.Get("X-RateLimit-Limit"))
				}
				return primaryObservation(resource, obs)
			}
			if response.Response.StatusCode == http.StatusTooManyRequests {
				return noFallback, secondaryRESTObservation(response.Response, obs)
			}
			return permissionFailure, obs
		case http.StatusNotFound:
			return permissionFailure, obs
		}
		return noFallback, obs
	}
	if obs.Secondary {
		return noFallback, obs
	}
	// githubv4 exposes either the first GraphQL error message or a formatted
	// non-200 HTTP status/body string, not an exported typed error.
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "secondary rate limit"), strings.Contains(message, "abuse detection"),
		strings.Contains(message, "non-200 ok status code: 429"), strings.Contains(message, "retryafterseconds"):
		obs.Secondary = true
		obs.RetryAfter = graphQLRetryAfter(message)
		return noFallback, obs
	case strings.Contains(message, "api rate limit exceeded"), strings.Contains(message, "rate limit exceeded"):
		return primaryObservation(resource, obs)
	case strings.Contains(message, "non-200 ok status code: 401"), strings.TrimSpace(message) == "bad credentials":
		return invalidCredential, obs
	case strings.Contains(message, "non-200 ok status code: 403"), strings.Contains(message, "non-200 ok status code: 404"),
		strings.Contains(message, "resource not accessible by integration"),
		strings.Contains(message, "resource not accessible by personal access token"),
		strings.Contains(message, "could not resolve to a repository"):
		return permissionFailure, obs
	default:
		return noFallback, obs
	}
}

func secondaryMessage(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "secondary rate limit") || strings.Contains(message, "abuse detection")
}

func secondaryRESTObservation(response *http.Response, obs Observation) Observation {
	obs.Secondary = true
	if response != nil && response.Header.Get("Retry-After") != "" {
		obs.RetryAfter = retryAfter(response.Header.Get("Retry-After"))
	} else if obs.RetryAfter <= 0 {
		obs.RetryAfter = time.Minute
	}
	return obs
}

// normalizedRateError preserves the original diagnostic/identity while exposing
// only the corrected rate kind to retry.classify. In particular, unwrap must
// not reveal a misclassified primary error underneath a secondary error.
type normalizedRateError struct{ original, rate error }

func (e *normalizedRateError) Error() string        { return e.original.Error() }
func (e *normalizedRateError) Unwrap() error        { return e.rate }
func (e *normalizedRateError) Is(target error) bool { return errors.Is(e.original, target) }
func (e *normalizedRateError) As(target any) bool {
	switch target.(type) {
	case **github.RateLimitError, **github.AbuseRateLimitError:
		return errors.As(e.rate, target)
	default:
		return errors.As(e.original, target)
	}
}

func normalizeRESTRateError(err error, failure accountFailure, obs Observation) error {
	if err == nil {
		return nil
	}
	var response *http.Response
	var message string
	var limited *github.RateLimitError
	var generic *github.ErrorResponse
	if errors.As(err, &limited) {
		if !obs.Secondary {
			return err
		}
		response, message = limited.Response, limited.Message
	} else if errors.As(err, &generic) && generic.Response != nil && (generic.Response.StatusCode == 403 || generic.Response.StatusCode == 429) {
		response, message = generic.Response, generic.Message
	} else {
		return err
	}
	if obs.Secondary {
		return &normalizedRateError{original: err, rate: &github.AbuseRateLimitError{Response: response, Message: message, RetryAfter: &obs.RetryAfter}}
	}
	if failure == primaryExhaustion {
		return &normalizedRateError{original: err, rate: &github.RateLimitError{Response: response, Message: message, Rate: github.Rate{Limit: obs.Limit, Remaining: 0, Reset: github.Timestamp{Time: obs.Reset}}}}
	}
	return err
}

func primaryObservation(resource string, obs Observation) (accountFailure, Observation) {
	if obs.Resource == "" {
		obs.Resource = resource
	}
	obs.Remaining = 0
	if obs.Reset.IsZero() {
		obs.Reset = time.Now().Add(time.Minute)
	}
	return primaryExhaustion, obs
}

func retryAfter(value string) time.Duration {
	if seconds, err := strconv.ParseUint(value, 10, 32); err == nil {
		return time.Duration(seconds) * time.Second
	}
	if until, err := http.ParseTime(value); err == nil {
		return time.Until(until)
	}
	return time.Minute
}

var retryAfterSecondsPattern = regexp.MustCompile(`"retryafterseconds"\s*:\s*(\d+)`)

func graphQLRetryAfter(message string) time.Duration {
	message = strings.ReplaceAll(message, `\"`, `"`)
	match := retryAfterSecondsPattern.FindStringSubmatch(message)
	if len(match) == 2 {
		return retryAfter(match[1])
	}
	return time.Minute
}

func uniqueAccounts(accounts []Account) []Account {
	if len(accounts) == 0 {
		return []Account{{}}
	}
	result := make([]Account, 0, len(accounts))
	seen := make(map[string]int, len(accounts))
	for _, account := range accounts {
		if index, ok := seen[account.Token]; ok {
			// A duplicate named entry cannot remove the account cap.
			if account.Name != "" {
				result[index].Name = account.Name
			}
			continue
		}
		seen[account.Token] = len(result)
		result = append(result, account)
	}
	return result
}

func (c *coordinator) acquireAccount(ctx context.Context, resource string, accounts []Account, excluded map[string]bool) (AccountPermit, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	accounts = uniqueAccounts(accounts)
	for {
		c.mu.Lock()
		if err := ctx.Err(); err != nil {
			c.mu.Unlock()
			return nil, err
		}
		now := time.Now()
		var selected *credentialState
		var token string
		var earliest time.Time
		eligible := false
		for _, account := range accounts {
			if excluded[account.Token] {
				continue
			}
			state := c.credentials[account.Token]
			if state == nil {
				state = &credentialState{resourcePause: make(map[string]time.Time)}
				c.credentials[account.Token] = state
			}
			if state.disabled {
				continue
			}
			if account.Name != "" {
				state.named = true
			}
			eligible = true
			if c.active >= c.cfg.Concurrency || (state.named && state.active >= c.cfg.AccountConcurrency) {
				continue
			}
			until := c.secondaryPause
			if state.nextStart.After(until) {
				until = state.nextStart
			}
			if pause := state.resourcePause[resource]; pause.After(until) {
				until = pause
			}
			if until.After(now) {
				if earliest.IsZero() || until.Before(earliest) {
					earliest = until
				}
				continue
			}
			if selected == nil || state.lastSelected < selected.lastSelected {
				selected, token = state, account.Token
			}
		}
		if selected != nil {
			c.active++
			selected.active++
			selected.nextStart = now.Add(c.cfg.MinRequestInterval)
			c.selection++
			selected.lastSelected = c.selection
			c.mu.Unlock()
			return &permit{c: c, credential: selected, token: token, resource: resource}, nil
		}
		changed := c.changed
		c.mu.Unlock()
		if !eligible {
			return nil, ErrNoAccountsAvailable
		}
		if err := waitAccount(ctx, changed, earliest); err != nil {
			return nil, err
		}
	}
}

func waitAccount(ctx context.Context, changed <-chan struct{}, until time.Time) error {
	var deadline <-chan time.Time
	if !until.IsZero() {
		timer := time.NewTimer(time.Until(until))
		defer timer.Stop()
		deadline = timer.C
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-changed:
		return nil
	case <-deadline:
		return nil
	}
}

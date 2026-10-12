// Package retry provides rate-limit- and transient-error-aware retry for
// GitHub API calls (REST via go-github, GraphQL via githubv4).
package retry

import (
	"context"
	"errors"
	"math/rand"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v56/github"
)

// Config controls per-call retry behavior.
type Config struct {
	MaxRetries int           // retries after the first attempt; 0 = no retry
	BaseDelay  time.Duration // base for exponential backoff (doubles per retry)
}

// maxBackoff caps the exponential-backoff fallback wait.
const maxBackoff = 2 * time.Minute

// classify reports whether err is retryable and, when GitHub reported an exact
// reset/retry time, how long to wait. A returned wait <= 0 means "use
// exponential backoff".
func classify(err error) (retryable bool, wait time.Duration) {
	// Pool exhaustion may preserve a permission diagnostic while carrying quota
	// evidence from another credential. Prefer its earliest reset.
	var deadline interface{ RetryAt() time.Time }
	if errors.As(err, &deadline) {
		return true, time.Until(deadline.RetryAt())
	}

	// REST primary rate limit (403 with rate-limit headers).
	var rateLimitErr *github.RateLimitError
	if errors.As(err, &rateLimitErr) {
		return true, time.Until(rateLimitErr.Rate.Reset.Time)
	}

	// REST secondary rate limit (403 abuse / secondary-limit response).
	var abuseErr *github.AbuseRateLimitError
	if errors.As(err, &abuseErr) {
		if abuseErr.RetryAfter != nil {
			return true, *abuseErr.RetryAfter
		}
		return true, 0
	}

	// Other REST error responses: 429 and 5xx are transient, rest are not.
	var respErr *github.ErrorResponse
	if errors.As(err, &respErr) {
		if respErr.Response == nil {
			return false, 0
		}
		switch respErr.Response.StatusCode {
		case 429:
			return true, retryAfterHeader(respErr.Response.Header.Get("Retry-After"))
		case 500, 502, 503, 504:
			return true, 0
		default:
			return false, 0
		}
	}

	// Network-level errors (connection reset, timeout, ...).
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true, 0
	}

	// GraphQL (githubv4) surfaces errors only as strings: either the embedded
	// "non-200 OK status code: <code> body: <body>" error, or the errors array
	// whose Error() is the first GraphQL error message.
	msg := err.Error()
	switch {
	case strings.Contains(msg, "retryAfterSeconds"):
		return true, parseRetryAfterSeconds(msg)
	case strings.Contains(msg, "API rate limit exceeded"),
		strings.Contains(msg, "secondary rate limit"),
		strings.Contains(msg, "rate limit"):
		return true, 0
	case strings.Contains(msg, "non-200 OK status code: 429"),
		strings.Contains(msg, "non-200 OK status code: 500"),
		strings.Contains(msg, "non-200 OK status code: 502"),
		strings.Contains(msg, "non-200 OK status code: 503"),
		strings.Contains(msg, "non-200 OK status code: 504"):
		return true, 0
	default:
		return false, 0
	}
}

// retryAfterHeader parses a Retry-After header value (seconds) into a duration.
// An empty or unparseable value yields 0, meaning "use backoff".
func retryAfterHeader(v string) time.Duration {
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// parseRetryAfterSeconds extracts retryAfterSeconds from a GraphQL error body
// embedded in the error string. Returns 0 when absent/unparseable.
func parseRetryAfterSeconds(msg string) time.Duration {
	const key = "retryAfterSeconds"
	i := strings.Index(msg, key)
	if i < 0 {
		return 0
	}
	rest := msg[i+len(key):]
	start := -1
	for j := 0; j < len(rest); j++ {
		if rest[j] >= '0' && rest[j] <= '9' {
			start = j
			break
		}
	}
	if start < 0 {
		return 0
	}
	end := start
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	secs, err := strconv.Atoi(rest[start:end])
	if err != nil {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// Do runs fn, retrying when the error is classified as retryable (GitHub rate
// limit or transient 5xx/network error). The wait between retries honors the
// reset time GitHub reports when available, else exponential backoff from
// BaseDelay. Waits are interrupted when ctx is done. Returns the last error
// after cfg.MaxRetries retries, or ctx.Err() if cancelled.
func Do(ctx context.Context, cfg Config, fn func() error) error {
	return do(ctx, cfg, rand.Int63n, fn)
}

func do(ctx context.Context, cfg Config, randN func(int64) int64, fn func() error) error {
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	}
	var lastErr error
	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		retryable, wait := classify(lastErr)
		if !retryable || attempt == cfg.MaxRetries {
			return lastErr
		}
		if wait <= 0 {
			wait = jitter(backoff(cfg.BaseDelay, attempt), randN)
		}
		if err := sleep(ctx, wait); err != nil {
			return err
		}
	}
	return lastErr
}

func jitter(d time.Duration, randN func(int64) int64) time.Duration {
	if d <= 0 || d >= maxBackoff {
		return minDuration(d, maxBackoff)
	}
	span := d/2 + 1
	result := d + time.Duration(randN(int64(span)))
	return minDuration(result, maxBackoff)
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// backoff returns BaseDelay * 2^attempt, capped at maxBackoff. A zero BaseDelay
// falls back to 1s so backoff never sleeps zero.
func backoff(base time.Duration, attempt int) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if attempt > 60 {
		return maxBackoff
	}
	d := base << uint(attempt)
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}

// sleep waits for d, returning ctx.Err() if ctx is cancelled first.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

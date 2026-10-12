package retry

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/go-github/v56/github"
	"github.com/stretchr/testify/require"
)

type poolDeadlineFixture struct {
	cause error
	reset time.Time
}

func (e poolDeadlineFixture) Error() string      { return e.cause.Error() }
func (e poolDeadlineFixture) Unwrap() error      { return e.cause }
func (e poolDeadlineFixture) RetryAt() time.Time { return e.reset }

func TestClassifyPoolDeadlineBeforeIndividualError(t *testing.T) {
	for _, first := range []error{
		&github.ErrorResponse{Response: &http.Response{StatusCode: 403}},
		&github.RateLimitError{Rate: github.Rate{Reset: github.Timestamp{Time: time.Now().Add(time.Hour)}}},
		errors.New("GraphQL permission denied"),
	} {
		reset := time.Now().Add(20 * time.Millisecond)
		err := fmt.Errorf("request: %w", poolDeadlineFixture{cause: first, reset: reset})
		retryable, wait := classify(err)
		require.True(t, retryable)
		require.Greater(t, wait, time.Duration(0))
		require.LessOrEqual(t, wait, 20*time.Millisecond)
		require.ErrorIs(t, err, first)
	}
}

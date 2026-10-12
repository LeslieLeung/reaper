package repository

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/wnarutou/gitrieve/internal/ui"
)

const gitEOFRetries = 3

var errCloneCleanup = errors.New("failed clone cleanup")

func isGitEOF(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func retryGitEOF(ctx context.Context, operation string, run func() error) error {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := run()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Joined errors may include an EOF alongside a local recovery failure.
		// Retrying is unsafe until that local failure has been resolved.
		if !isGitEOF(err) || attempt == gitEOFRetries ||
			errors.Is(err, errCloneCleanup) || errors.Is(err, errManagedIndexRecovery) ||
			errors.Is(err, errManagedWorktreeRecovery) || errors.Is(err, errPackCleanup) ||
			errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, transport.ErrAuthenticationRequired) || errors.Is(err, transport.ErrAuthorizationFailed) {
			return err
		}
		delay := time.Second << attempt
		// Do not include the raw error/URL, which could contain credentials.
		ui.Printf("Git %s interrupted by EOF; retry %d/%d in %s", operation, attempt+1, gitEOFRetries, delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

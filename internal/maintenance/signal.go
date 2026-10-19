// Package maintenance carries cooperative stop requests separately from hard
// context cancellation, allowing components to finish publishing current work.
package maintenance

import (
	"context"
	"errors"
	"sync"
)

var ErrRequested = errors.New("maintenance requested")

type Signal struct {
	once sync.Once
	done chan struct{}
}

func NewSignal() *Signal                { return &Signal{done: make(chan struct{})} }
func (s *Signal) Request()              { s.once.Do(func() { close(s.done) }) }
func (s *Signal) Done() <-chan struct{} { return s.done }

type signalKey struct{}

func WithSignal(ctx context.Context, signal *Signal) context.Context {
	return context.WithValue(ctx, signalKey{}, signal)
}

func Requested(ctx context.Context) bool {
	signal, _ := ctx.Value(signalKey{}).(*Signal)
	if signal == nil {
		return false
	}
	select {
	case <-signal.Done():
		return true
	default:
		return false
	}
}

// InterruptContext derives a context for work that may be interrupted by a soft
// stop, such as Git network calls. Archive publication must keep using ctx.
func InterruptContext(ctx context.Context) (context.Context, context.CancelFunc) {
	interrupted, cancel := context.WithCancel(ctx)
	signal, _ := ctx.Value(signalKey{}).(*Signal)
	if signal == nil {
		return interrupted, cancel
	}
	if Requested(ctx) {
		cancel()
		return interrupted, cancel
	}
	go func() {
		select {
		case <-signal.Done():
			cancel()
		case <-interrupted.Done():
		}
	}()
	return interrupted, cancel
}

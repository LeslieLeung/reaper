package maintenance

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestSignalRequestsSoftStopWithoutCancellingContext(t *testing.T) {
	signal := NewSignal()
	ctx := WithSignal(context.Background(), signal)
	if Requested(ctx) || Requested(context.Background()) {
		t.Fatal("new signal is already requested")
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); signal.Request() }()
	}
	wg.Wait()
	select {
	case <-signal.Done():
	default:
		t.Fatal("request did not close signal")
	}
	if !Requested(ctx) || ctx.Err() != nil {
		t.Fatal("soft stop must leave context active")
	}
}

func TestInterruptContextStopsNetworkButKeepsParentActive(t *testing.T) {
	signal := NewSignal()
	parent := WithSignal(context.Background(), signal)
	ctx, cancel := InterruptContext(parent)
	defer cancel()
	signal.Request()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("network context did not stop")
	}
	if parent.Err() != nil {
		t.Fatal("network interruption cancelled archive context")
	}
}

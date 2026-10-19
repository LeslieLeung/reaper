package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/wnarutou/gitrieve/internal/ui"
)

// serve keeps status/API access available while the worker drain completes.
// Docker's stop grace is the external deadline; operators should enter
// maintenance and wait for ready before stopping a container with large jobs.
func (s *Server) serve(ctx context.Context, listener net.Listener) error {
	server := &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	ui.Printf("Stopping admission and waiting for maintenance boundaries")
	s.executor.BeginShutdown()
	drainErr := s.executor.WaitForMaintenance(context.Background())
	if drainErr != nil {
		ui.Errorf("Maintenance state could not be saved: %v", drainErr)
	}
	ui.Printf("Maintenance drain complete; closing HTTP server")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		// Long-lived SSE connections must not hold up database shutdown forever.
		_ = server.Close()
	}
	err := <-result
	if errors.Is(err, http.ErrServerClosed) {
		return drainErr
	}
	return errors.Join(drainErr, err)
}

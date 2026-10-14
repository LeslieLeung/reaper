package server

import (
	"context"
	"time"

	internalserver "github.com/wnarutou/gitrieve/internal/server"
	"github.com/wnarutou/gitrieve/internal/ui"
)

// The cleanup lifecycle is independent of repository scheduling, so refreshing
// cron configuration cannot remove it or queue it behind repository jobs.
func (s *Server) startLogCleanup(interval time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	s.logCleanupCancel = cancel
	s.logCleanupDone = make(chan struct{})
	go func() {
		defer close(s.logCleanupDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			months := internalserver.GetServerConfig().LogRetentionMonths
			deleted, err := s.database.PruneExecutionLogs(ctx, time.Now(), months)
			if err != nil && ctx.Err() == nil {
				ui.Errorf("Failed to clean expired execution logs: %s", err)
			} else if deleted > 0 {
				ui.Printf("Cleaned %d execution log lines (retention: %d months)", deleted, months)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

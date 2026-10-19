package executor

import (
	"context"
	"errors"
	"strings"

	"github.com/wnarutou/gitrieve/internal/maintenance"
	"github.com/wnarutou/gitrieve/internal/ui"
)

var ErrMaintenance = errors.New("server is in maintenance mode")
var ErrMaintenanceDraining = errors.New("maintenance is still waiting for active work to finish")

// MaintenanceStatus is process-local: restarting restores normal scheduling.
type MaintenanceStatus struct {
	Drained      bool   `json:"drained"`
	Error        string `json:"error,omitempty"`
	ShuttingDown bool   `json:"shutting_down"`
	Enabled      bool   `json:"enabled"`
	Ready        bool   `json:"ready"`
	Running      int    `json:"running"`
	Pending      int    `json:"pending"`
	Preparing    int    `json:"preparing"`
}

func (e *Executor) maintenanceStatusLocked() MaintenanceStatus {
	drained := e.maintenance && e.running == 0 && e.settling == 0 && e.inflight == 0 && len(e.preparations) == 0
	message := ""
	if e.maintenanceErr != nil {
		message = e.maintenanceErr.Error()
	}
	return MaintenanceStatus{
		Drained: drained, Error: message,
		ShuttingDown: e.shuttingDown,
		Enabled:      e.maintenance,
		Ready:        drained && e.maintenanceErr == nil,
		Running:      e.running, Pending: len(e.pending) + e.settling, Preparing: len(e.preparations),
	}
}

func (e *Executor) MaintenanceStatus() MaintenanceStatus {
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	return e.maintenanceStatusLocked()
}

// EnterMaintenance closes admission before signalling workers. Pending work is
// cancelled, not saved for replay; the next cron/manual invocation starts anew.
func (e *Executor) EnterMaintenance() MaintenanceStatus {
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	if e.maintenance || e.closed {
		return e.maintenanceStatusLocked()
	}
	e.maintenance = true
	for _, cancel := range e.preparations {
		cancel()
	}
	for _, job := range e.jobs {
		job.maintenance.Request()
	}
	pending := e.pending
	e.pending = nil
	e.settling += len(pending)
	e.workers.Add(len(pending))
	for _, job := range pending {
		go func(job *queuedJob) {
			defer e.workers.Done()
			unbind := ui.Bind(job.id, job.repo.Name)
			e.finishCancellation(job.id, job.plan, 0, maintenance.ErrRequested)
			unbind()
			e.queueMu.Lock()
			if jobCtx := e.jobs[job.id]; jobCtx != nil {
				jobCtx.CancelFunc()
				close(jobCtx.done)
				delete(e.jobs, job.id)
			}
			if e.activeByRepo[job.repo.Key()] == job.id {
				delete(e.activeByRepo, job.repo.Key())
			}
			e.settling--
			e.queueCond.Broadcast()
			e.queueMu.Unlock()
		}(job)
	}
	e.queueCond.Broadcast()
	return e.maintenanceStatusLocked()
}

func (e *Executor) ExitMaintenance() error {
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	if e.closed || e.shuttingDown {
		return ErrExecutorClosed
	}
	if !e.maintenance {
		return nil
	}
	if !e.maintenanceStatusLocked().Drained {
		return ErrMaintenanceDraining
	}
	if e.maintenanceErr != nil {
		return e.maintenanceErr
	}
	e.maintenance = false
	e.queueCond.Broadcast()
	return nil
}

func (e *Executor) WaitForMaintenance(ctx context.Context) error {
	wake := context.AfterFunc(ctx, func() {
		e.queueMu.Lock()
		e.queueCond.Broadcast()
		e.queueMu.Unlock()
	})
	defer wake()
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	for !e.maintenanceStatusLocked().Drained {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !e.maintenance {
			return errors.New("maintenance mode is not enabled")
		}
		e.queueCond.Wait()
	}
	return e.maintenanceErr
}

// Track expansion as well as DB admission, so ready cannot race an org lookup.
func (e *Executor) beginPreparation(parent context.Context) (context.Context, func(), error) {
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	if e.closed {
		return nil, nil, ErrExecutorClosed
	}
	if e.maintenance {
		return nil, nil, ErrMaintenance
	}
	ctx, cancel := context.WithCancel(parent)
	e.nextPreparation++
	id := e.nextPreparation
	if e.preparations == nil {
		e.preparations = make(map[uint64]context.CancelFunc)
	}
	e.preparations[id] = cancel
	return ctx, func() {
		cancel()
		e.queueMu.Lock()
		delete(e.preparations, id)
		e.queueCond.Broadcast()
		e.queueMu.Unlock()
	}, nil
}

func (e *Executor) finishMaintenance(jobID string, plan []component, first int, failures []string, allComponentsTerminal bool) {
	if len(failures) == 0 && allComponentsTerminal {
		e.finishCancellation(jobID, plan, first, maintenance.ErrRequested)
		return
	}
	more, terminal := e.cancelComponents(jobID, plan, first, maintenance.ErrRequested)
	failures = append(failures, more...)
	if terminal && allComponentsTerminal {
		e.finishExecution(jobID, StatusFailed, strings.Join(failures, "; "))
	} else {
		e.recordMaintenanceError(errors.New("cannot finalize maintenance: " + strings.Join(failures, "; ")))
	}
}

// BeginShutdown makes maintenance irreversible for this process.
func (e *Executor) BeginShutdown() {
	e.queueMu.Lock()
	e.shuttingDown = true
	e.queueMu.Unlock()
	e.EnterMaintenance()
}

func (e *Executor) recordMaintenanceError(err error) {
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	if e.maintenance {
		e.maintenanceErr = errors.Join(e.maintenanceErr, err)
		e.queueCond.Broadcast()
	}
}

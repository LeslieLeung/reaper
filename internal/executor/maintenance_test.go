package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/wnarutou/gitrieve/internal/config"
	"github.com/wnarutou/gitrieve/internal/db"
	"github.com/wnarutou/gitrieve/internal/maintenance"
	"github.com/wnarutou/gitrieve/internal/typedef"
)

func TestMaintenanceReportsUnfinishedComponentPersistence(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	runners := noOpRunners()
	runners.Issue = func(ctx context.Context, _ typedef.Repository, _ []typedef.MultiStorage) error {
		close(entered)
		select {
		case <-release:
			return maintenance.ErrRequested
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	exec, database := newTestExecutorForRepo(t, typedef.Repository{URL: "github.com/test/repo", DownloadIssues: true}, runners)
	_, err := database.Exec(`CREATE TRIGGER prevent_code_finish BEFORE UPDATE OF status ON execution_components
 WHEN OLD.component = 'code' AND NEW.status IN ('completed','failed')
 BEGIN SELECT RAISE(FAIL, 'terminal storage failure'); END;`)
	require.NoError(t, err)
	ids, err := exec.ExecuteJob("github.com/test/repo")
	require.NoError(t, err)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("issue did not start")
	}
	exec.EnterMaintenance()
	close(release)
	waitForJob(t, exec, ids[0])
	requireExecution(t, database, ids[0], StatusRunning, "")
	require.False(t, exec.MaintenanceStatus().Ready, "failed persistence must not be reported as safe")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = exec.WaitForMaintenance(ctx)
	require.Error(t, err)
	require.NotErrorIs(t, err, context.DeadlineExceeded, "drain failures should be surfaced promptly")
	require.Error(t, exec.ExitMaintenance())
}

func TestMaintenanceDrainsWithoutCancellingRunningContext(t *testing.T) {
	started := make(chan context.Context, 2)
	release := make(chan struct{})
	runners := noOpRunners()
	runners.Code = func(ctx context.Context, _ typedef.Repository, _ []typedef.MultiStorage) error {
		started <- ctx
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	runners.Issue = func(context.Context, typedef.Repository, []typedef.MultiStorage) error {
		t.Error("later component started during maintenance")
		return nil
	}
	cfg := &config.Config{ConcurrencyNum: 1, Repository: []typedef.Repository{
		{URL: "github.com/test/one", DownloadIssues: true}, {URL: "github.com/test/two"},
	}}
	exec, database := newTestExecutorForConfig(t, cfg, runners)
	one, err := exec.ExecuteJob(cfg.Repository[0].Key())
	require.NoError(t, err)
	var runningCtx context.Context
	select {
	case runningCtx = <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not start")
	}
	two, err := exec.ExecuteJob(cfg.Repository[1].Key())
	require.NoError(t, err)
	status := exec.EnterMaintenance()
	require.True(t, status.Enabled)
	require.False(t, status.Ready)
	require.NoError(t, runningCtx.Err(), "soft maintenance must allow the current page/file to finish")
	_, err = exec.ExecuteJob(cfg.Repository[1].Key())
	require.ErrorIs(t, err, ErrMaintenance)
	require.ErrorIs(t, exec.ExitMaintenance(), ErrMaintenanceDraining)
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, exec.WaitForMaintenance(ctx))
	require.True(t, exec.MaintenanceStatus().Ready)
	for _, id := range []string{one[0], two[0]} {
		var got, message string
		require.NoError(t, database.QueryRow("SELECT status, COALESCE(error_message, '') FROM executions WHERE id = ?", id).Scan(&got, &message))
		require.Equal(t, "cancelled", got)
		require.Contains(t, message, "maintenance")
	}
	requireComponents(t, database, one[0], expectedComponent{db.ComponentCode, db.ComponentCompleted, ""}, expectedComponent{db.ComponentIssue, db.ComponentCancelled, "maintenance requested"})
	require.NoError(t, exec.ExitMaintenance())
	select {
	case <-started:
		t.Fatal("maintenance exit restarted a cancelled job")
	default:
	}
	next, err := exec.ExecuteJob(cfg.Repository[1].Key())
	require.NoError(t, err)
	waitForJob(t, exec, next[0])
	require.False(t, exec.MaintenanceStatus().Enabled)
}

func TestMaintenanceWaitsForAdmissionAndDiscardsUnpublishedJobs(t *testing.T) {
	exec, database := newTestExecutor(t)
	entered, release := make(chan struct{}), make(chan struct{})
	exec.db = &blockingExecutionStore{delegate: database, componentsEntered: entered, componentsRelease: release}
	result := make(chan error, 1)
	go func() { _, err := exec.ExecuteJob("github.com/test/repo"); result <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("admission did not start")
	}
	require.False(t, exec.EnterMaintenance().Ready)
	close(release)
	select {
	case err := <-result:
		require.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("admission did not exit")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, exec.WaitForMaintenance(ctx))
	var count int
	require.NoError(t, database.QueryRow("SELECT COUNT(*) FROM executions").Scan(&count))
	require.Zero(t, count)
}

func TestMaintenanceDoesNotHideArchiveFailure(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	runners := noOpRunners()
	runners.Issue = func(ctx context.Context, _ typedef.Repository, _ []typedef.MultiStorage) error {
		close(entered)
		select {
		case <-release:
			return errors.New("archive storage unavailable")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	exec, database := newTestExecutorForRepo(t, typedef.Repository{URL: "github.com/test/repo", DownloadIssues: true, DownloadDiscussion: true}, runners)
	ids, err := exec.ExecuteJob("github.com/test/repo")
	require.NoError(t, err)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("issue did not start")
	}
	exec.EnterMaintenance()
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, exec.WaitForMaintenance(ctx))
	var status, message string
	require.NoError(t, database.QueryRow("SELECT status, error_message FROM executions WHERE id = ?", ids[0]).Scan(&status, &message))
	require.Equal(t, "failed", status)
	require.Contains(t, message, "archive storage unavailable")
	requireComponents(t, database, ids[0], expectedComponent{db.ComponentCode, db.ComponentCompleted, ""}, expectedComponent{db.ComponentIssue, db.ComponentFailed, "archive storage unavailable"}, expectedComponent{db.ComponentDiscussion, db.ComponentCancelled, "maintenance requested"})
}

package executor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wnarutou/gitrieve/internal/config"
	"github.com/wnarutou/gitrieve/internal/db"
	"github.com/wnarutou/gitrieve/internal/githubapi"
	"github.com/wnarutou/gitrieve/internal/logger"
	"github.com/wnarutou/gitrieve/internal/syncresult"
	"github.com/wnarutou/gitrieve/internal/typedef"
)

func newTestExecutor(t *testing.T) (*Executor, *db.DB) {
	t.Helper()
	return newTestExecutorForRepo(t, typedef.Repository{Name: "test-repo", URL: "github.com/test/repo"}, noOpRunners())
}

func newTestExecutorForRepo(t *testing.T, repo typedef.Repository, runners Runners) (*Executor, *db.DB) {
	t.Helper()
	return newTestExecutorForConfig(t, &config.Config{Repository: []typedef.Repository{repo}}, runners)
}

func newTestExecutorForConfig(t *testing.T, cfg *config.Config, runners Runners) (*Executor, *db.DB) {
	t.Helper()
	testDB, err := db.Initialize(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testDB.Close()) })

	log := logger.NewLogger(testDB)
	exec := NewExecutorWithRunners(log, testDB, cfg, runners)
	t.Cleanup(func() { require.NoError(t, exec.Close()) })
	return exec, testDB
}

func noOpRunners() Runners {
	run := func(context.Context, typedef.Repository, []typedef.MultiStorage) error { return nil }
	return Runners{Code: run, Release: run, Issue: run, Wiki: run, Discussion: run}
}

type runnerRecorder struct {
	mu     sync.Mutex
	calls  []string
	errors map[string]error
}

func (r *runnerRecorder) runner(name string) SyncFunc {
	return func(context.Context, typedef.Repository, []typedef.MultiStorage) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls = append(r.calls, name)
		return r.errors[name]
	}
}

func (r *runnerRecorder) runners() Runners {
	return Runners{
		Code:       r.runner("code"),
		Release:    r.runner("release"),
		Issue:      r.runner("issues"),
		Wiki:       r.runner("wiki"),
		Discussion: r.runner("discussion"),
	}
}

func (r *runnerRecorder) recordedCalls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

type blockingRunner struct {
	entered    chan string
	release    chan struct{}
	running    atomic.Int32
	maxRunning atomic.Int32
}

type blockingExecutionStore struct {
	delegate *db.DB

	activeKey     string
	activeEntered chan struct{}
	activeRelease chan struct{}
	activeOnce    sync.Once

	execEntered chan struct{}
	execRelease chan struct{}
	execOnce    sync.Once

	componentsEntered chan struct{}
	componentsRelease chan struct{}
	componentsOnce    sync.Once

	finishExecutionID string
	finishEntered     chan struct{}
	finishRelease     chan struct{}
	finishOnce        sync.Once
}

type discardFailingStore struct {
	*blockingExecutionStore
	err error
}

func (s *discardFailingStore) DiscardPendingExecutions(context.Context, []string) error {
	return s.err
}

func (s *blockingExecutionStore) Exec(query string, args ...interface{}) (sql.Result, error) {
	if s.execEntered != nil {
		s.execOnce.Do(func() { close(s.execEntered) })
		<-s.execRelease
	}
	return s.delegate.Exec(query, args...)
}

func (s *blockingExecutionStore) ActiveExecutionExists(ctx context.Context, repoKey string) (bool, error) {
	if repoKey == s.activeKey {
		s.activeOnce.Do(func() { close(s.activeEntered) })
		<-s.activeRelease
	}
	return s.delegate.ActiveExecutionExists(ctx, repoKey)
}

func (s *blockingExecutionStore) CreatePendingExecutions(ctx context.Context, executions []db.PendingExecution) error {
	err := s.delegate.CreatePendingExecutions(ctx, executions)
	if err == nil && s.componentsEntered != nil {
		s.componentsOnce.Do(func() { close(s.componentsEntered) })
		<-s.componentsRelease
	}
	return err
}

func (s *blockingExecutionStore) DiscardPendingExecutions(ctx context.Context, executionIDs []string) error {
	return s.delegate.DiscardPendingExecutions(ctx, executionIDs)
}

func (s *blockingExecutionStore) StartComponent(ctx context.Context, executionID string, component db.ComponentName, startedAt time.Time) error {
	return s.delegate.StartComponent(ctx, executionID, component, startedAt)
}

func (s *blockingExecutionStore) FinishComponent(ctx context.Context, executionID string, component db.ComponentName, status db.ComponentStatus, finishedAt time.Time, errorMessage string) error {
	err := s.delegate.FinishComponent(ctx, executionID, component, status, finishedAt, errorMessage)
	if err == nil && executionID == s.finishExecutionID {
		s.finishOnce.Do(func() { close(s.finishEntered) })
		<-s.finishRelease
	}
	return err
}

func newBlockingRunner(capacity int) *blockingRunner {
	return &blockingRunner{
		entered: make(chan string, capacity),
		release: make(chan struct{}),
	}
}

func (r *blockingRunner) run(ctx context.Context, repo typedef.Repository, _ []typedef.MultiStorage) error {
	running := r.running.Add(1)
	defer r.running.Add(-1)
	for {
		maxRunning := r.maxRunning.Load()
		if running <= maxRunning || r.maxRunning.CompareAndSwap(maxRunning, running) {
			break
		}
	}
	r.entered <- repo.Key()
	select {
	case <-r.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *blockingRunner) runners() Runners {
	runners := noOpRunners()
	runners.Code = r.run
	return runners
}

func requireRunnerEntry(t *testing.T, entered <-chan string) string {
	t.Helper()
	select {
	case repoKey := <-entered:
		return repoKey
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not start")
		return ""
	}
}

func executorClosedSignal(exec *Executor) <-chan struct{} {
	closed := make(chan struct{})
	go func() {
		exec.queueMu.Lock()
		for !exec.closed {
			exec.queueCond.Wait()
		}
		exec.queueMu.Unlock()
		close(closed)
	}()
	return closed
}

func executorCloseWaiterSignal(exec *Executor) <-chan struct{} {
	waiting := make(chan struct{})
	go func() {
		exec.queueMu.Lock()
		for exec.closeWaiters == 0 {
			exec.queueCond.Wait()
		}
		exec.queueMu.Unlock()
		close(waiting)
	}()
	return waiting
}

func repositoryConfig(limit uint, count int) *config.Config {
	repos := make([]typedef.Repository, count)
	for i := range repos {
		repos[i] = typedef.Repository{
			Name: fmt.Sprintf("repo-%d", i),
			URL:  fmt.Sprintf("github.com/test/repo-%d", i),
		}
	}
	return &config.Config{ConcurrencyNum: limit, Repository: repos}
}

func executionStatusCounts(t *testing.T, testDB *db.DB) map[string]int {
	t.Helper()
	rows, err := testDB.Query(`SELECT status, COUNT(*) FROM executions GROUP BY status`)
	require.NoError(t, err)
	defer rows.Close()
	counts := make(map[string]int)
	for rows.Next() {
		var status string
		var count int
		require.NoError(t, rows.Scan(&status, &count))
		counts[status] = count
	}
	require.NoError(t, rows.Err())
	return counts
}

func waitForJob(t *testing.T, exec *Executor, jobID string) {
	t.Helper()
	exec.queueMu.Lock()
	jobCtx := exec.jobs[jobID]
	exec.queueMu.Unlock()
	if jobCtx == nil {
		return
	}
	select {
	case <-jobCtx.done:
	case <-time.After(3 * time.Second):
		t.Fatalf("job %s did not finish", jobID)
	}
}

func requireExecution(t *testing.T, testDB *db.DB, jobID string, wantStatus ExecutionStatus, wantError string) {
	t.Helper()
	var status, errorMessage string
	require.NoError(t, testDB.QueryRow(
		"SELECT status, COALESCE(error_message, '') FROM executions WHERE id = ?", jobID,
	).Scan(&status, &errorMessage))
	require.Equal(t, string(wantStatus), status)
	require.Equal(t, wantError, errorMessage)
}

type expectedComponent struct {
	name         db.ComponentName
	status       db.ComponentStatus
	errorMessage string
}

func requireComponents(t *testing.T, testDB *db.DB, jobID string, want ...expectedComponent) {
	t.Helper()
	components, err := testDB.ListComponents(context.Background(), jobID)
	require.NoError(t, err)
	require.Len(t, components, len(want))
	for i := range want {
		require.Equal(t, want[i].name, components[i].Component)
		require.Equal(t, want[i].status, components[i].Status)
		require.Equal(t, want[i].errorMessage, components[i].ErrorMessage)
	}
}

func TestExecuteJobWritesBoundLogs(t *testing.T) {
	exec, testDB := newTestExecutor(t)

	jobIDs, err := exec.ExecuteJob("github.com/test/repo")
	require.NoError(t, err)
	require.Len(t, jobIDs, 1)
	waitForJob(t, exec, jobIDs[0])

	var count int
	err = testDB.QueryRow(
		"SELECT COUNT(*) FROM logs WHERE execution_id = ? AND message = 'Starting job execution'", jobIDs[0],
	).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestExecuteJobRunsConfiguredComponents(t *testing.T) {
	recorder := &runnerRecorder{errors: map[string]error{}}
	exec, _ := newTestExecutorForRepo(t, typedef.Repository{
		Name:               "test-repo",
		URL:                "github.com/test/repo",
		DownloadReleases:   true,
		DownloadIssues:     true,
		DownloadWiki:       true,
		DownloadDiscussion: true,
	}, recorder.runners())

	jobIDs, err := exec.ExecuteJob("github.com/test/repo")
	require.NoError(t, err)
	require.Len(t, jobIDs, 1)
	waitForJob(t, exec, jobIDs[0])
	require.Equal(t, []string{"code", "release", "issues", "wiki", "discussion"}, recorder.recordedCalls())
}

func TestExecuteJobComponentsArePlannedBeforeTheFirstRunner(t *testing.T) {
	codeStarted := make(chan struct{})
	releaseCode := make(chan struct{})
	runners := noOpRunners()
	runners.Code = func(context.Context, typedef.Repository, []typedef.MultiStorage) error {
		close(codeStarted)
		<-releaseCode
		return nil
	}
	exec, testDB := newTestExecutorForRepo(t, typedef.Repository{
		Name:               "test-repo",
		URL:                "github.com/test/repo",
		DownloadReleases:   true,
		DownloadIssues:     true,
		DownloadWiki:       true,
		DownloadDiscussion: true,
	}, runners)

	jobIDs, err := exec.ExecuteJob("github.com/test/repo")
	require.NoError(t, err)
	<-codeStarted
	requireComponents(t, testDB, jobIDs[0],
		expectedComponent{db.ComponentCode, db.ComponentRunning, ""},
		expectedComponent{db.ComponentRelease, db.ComponentPending, ""},
		expectedComponent{db.ComponentIssue, db.ComponentPending, ""},
		expectedComponent{db.ComponentWiki, db.ComponentPending, ""},
		expectedComponent{db.ComponentDiscussion, db.ComponentPending, ""},
	)

	close(releaseCode)
	waitForJob(t, exec, jobIDs[0])
}

func TestExecuteJobComponentFailureRunsLaterComponentsAndFailsOverall(t *testing.T) {
	recorder := &runnerRecorder{errors: map[string]error{"issues": errors.New("rate limit")}}
	exec, testDB := newTestExecutorForRepo(t, typedef.Repository{
		Name:           "test-repo",
		URL:            "github.com/test/repo",
		DownloadIssues: true,
		DownloadWiki:   true,
	}, recorder.runners())

	jobIDs, err := exec.ExecuteJob("github.com/test/repo")
	require.NoError(t, err)
	waitForJob(t, exec, jobIDs[0])

	require.Equal(t, []string{"code", "issues", "wiki"}, recorder.recordedCalls())
	requireExecution(t, testDB, jobIDs[0], StatusFailed, "issue: rate limit")
	requireComponents(t, testDB, jobIDs[0],
		expectedComponent{db.ComponentCode, db.ComponentCompleted, ""},
		expectedComponent{db.ComponentIssue, db.ComponentFailed, "rate limit"},
		expectedComponent{db.ComponentWiki, db.ComponentCompleted, ""},
	)
}

func TestExecuteJobOverallFailureContinuesAfterCodeFailure(t *testing.T) {
	recorder := &runnerRecorder{errors: map[string]error{"code": errors.New("clone failed")}}
	exec, testDB := newTestExecutorForRepo(t, typedef.Repository{
		Name:           "test-repo",
		URL:            "github.com/test/repo",
		DownloadIssues: true,
	}, recorder.runners())

	jobIDs, err := exec.ExecuteJob("github.com/test/repo")
	require.NoError(t, err)
	waitForJob(t, exec, jobIDs[0])

	require.Equal(t, []string{"code", "issues"}, recorder.recordedCalls())
	requireExecution(t, testDB, jobIDs[0], StatusFailed, "code: clone failed")
	requireComponents(t, testDB, jobIDs[0],
		expectedComponent{db.ComponentCode, db.ComponentFailed, "clone failed"},
		expectedComponent{db.ComponentIssue, db.ComponentCompleted, ""},
	)
}

func TestExecuteJobComponentContextErrorWithoutJobCancellationIsFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "runner cancelled", err: context.Canceled},
		{name: "runner deadline exceeded", err: context.DeadlineExceeded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &runnerRecorder{errors: map[string]error{"code": tt.err}}
			exec, testDB := newTestExecutorForRepo(t, typedef.Repository{
				Name:           "test-repo",
				URL:            "github.com/test/repo",
				DownloadIssues: true,
			}, recorder.runners())

			jobIDs, err := exec.ExecuteJob("github.com/test/repo")
			require.NoError(t, err)
			waitForJob(t, exec, jobIDs[0])

			require.Equal(t, []string{"code", "issues"}, recorder.recordedCalls())
			requireExecution(t, testDB, jobIDs[0], StatusFailed, "code: "+tt.err.Error())
			requireComponents(t, testDB, jobIDs[0],
				expectedComponent{db.ComponentCode, db.ComponentFailed, tt.err.Error()},
				expectedComponent{db.ComponentIssue, db.ComponentCompleted, ""},
			)
		})
	}
}

func TestExecuteJobComponentFailuresUseSemicolonSeparatedSummary(t *testing.T) {
	recorder := &runnerRecorder{errors: map[string]error{
		"issues": errors.New("rate limit"),
		"wiki":   errors.New("permission denied"),
	}}
	exec, testDB := newTestExecutorForRepo(t, typedef.Repository{
		Name:           "test-repo",
		URL:            "github.com/test/repo",
		DownloadIssues: true,
		DownloadWiki:   true,
	}, recorder.runners())

	jobIDs, err := exec.ExecuteJob("github.com/test/repo")
	require.NoError(t, err)
	waitForJob(t, exec, jobIDs[0])

	requireExecution(t, testDB, jobIDs[0], StatusFailed, "issue: rate limit; wiki: permission denied")
	requireComponents(t, testDB, jobIDs[0],
		expectedComponent{db.ComponentCode, db.ComponentCompleted, ""},
		expectedComponent{db.ComponentIssue, db.ComponentFailed, "rate limit"},
		expectedComponent{db.ComponentWiki, db.ComponentFailed, "permission denied"},
	)
}

func TestExecuteJobSkipCompletesOverall(t *testing.T) {
	recorder := &runnerRecorder{errors: map[string]error{
		"wiki": fmt.Errorf("wiki preflight: %w", syncresult.Skip("repository has no wiki")),
	}}
	exec, testDB := newTestExecutorForRepo(t, typedef.Repository{
		Name:         "test-repo",
		URL:          "github.com/test/repo",
		DownloadWiki: true,
	}, recorder.runners())

	jobIDs, err := exec.ExecuteJob("github.com/test/repo")
	require.NoError(t, err)
	waitForJob(t, exec, jobIDs[0])

	requireExecution(t, testDB, jobIDs[0], StatusCompleted, "")
	requireComponents(t, testDB, jobIDs[0],
		expectedComponent{db.ComponentCode, db.ComponentCompleted, ""},
		expectedComponent{db.ComponentWiki, db.ComponentSkipped, "repository has no wiki"},
	)
}

func TestExecuteJobCancelTerminalizesCurrentAndRemainingComponents(t *testing.T) {
	issueStarted := make(chan struct{})
	runners := noOpRunners()
	runners.Issue = func(ctx context.Context, _ typedef.Repository, _ []typedef.MultiStorage) error {
		close(issueStarted)
		<-ctx.Done()
		return ctx.Err()
	}
	exec, testDB := newTestExecutorForRepo(t, typedef.Repository{
		Name:           "test-repo",
		URL:            "github.com/test/repo",
		DownloadIssues: true,
		DownloadWiki:   true,
	}, runners)

	jobIDs, err := exec.ExecuteJob("github.com/test/repo")
	require.NoError(t, err)
	<-issueStarted
	require.NoError(t, exec.CancelJob(jobIDs[0]))
	waitForJob(t, exec, jobIDs[0])

	requireExecution(t, testDB, jobIDs[0], StatusCancelled, "")
	requireComponents(t, testDB, jobIDs[0],
		expectedComponent{db.ComponentCode, db.ComponentCompleted, ""},
		expectedComponent{db.ComponentIssue, db.ComponentCancelled, "context canceled"},
		expectedComponent{db.ComponentWiki, db.ComponentCancelled, "context canceled"},
	)
}

func TestExecuteJobCancelStoreFailureFallsBackToFailedComponentAndOverall(t *testing.T) {
	issueStarted := make(chan struct{})
	runners := noOpRunners()
	runners.Issue = func(ctx context.Context, _ typedef.Repository, _ []typedef.MultiStorage) error {
		close(issueStarted)
		<-ctx.Done()
		return ctx.Err()
	}
	exec, testDB := newTestExecutorForRepo(t, typedef.Repository{
		Name:           "test-repo",
		URL:            "github.com/test/repo",
		DownloadIssues: true,
		DownloadWiki:   true,
	}, runners)
	_, err := testDB.Exec(`
		CREATE TRIGGER fail_issue_cancellation
		BEFORE UPDATE OF status ON execution_components
		WHEN OLD.component = 'issues' AND NEW.status = 'cancelled'
		BEGIN
			SELECT RAISE(FAIL, 'forced cancellation terminal update failure');
		END;
	`)
	require.NoError(t, err)

	jobIDs, err := exec.ExecuteJob("github.com/test/repo")
	require.NoError(t, err)
	<-issueStarted
	require.NoError(t, exec.CancelJob(jobIDs[0]))
	waitForJob(t, exec, jobIDs[0])

	persistenceMessage := fmt.Sprintf(
		`persist cancelled state: finish component "issues" for execution %q: constraint failed: forced cancellation terminal update failure (1811)`,
		jobIDs[0],
	)
	requireExecution(t, testDB, jobIDs[0], StatusFailed, "issue: "+persistenceMessage)
	requireComponents(t, testDB, jobIDs[0],
		expectedComponent{db.ComponentCode, db.ComponentCompleted, ""},
		expectedComponent{db.ComponentIssue, db.ComponentFailed, persistenceMessage},
		expectedComponent{db.ComponentWiki, db.ComponentCancelled, "context canceled"},
	)
}

func TestExecuteJobCancelFailedFallbackRejectionLeavesOverallActive(t *testing.T) {
	issueStarted := make(chan struct{})
	runners := noOpRunners()
	runners.Issue = func(ctx context.Context, _ typedef.Repository, _ []typedef.MultiStorage) error {
		close(issueStarted)
		<-ctx.Done()
		return ctx.Err()
	}
	exec, testDB := newTestExecutorForRepo(t, typedef.Repository{
		Name:           "test-repo",
		URL:            "github.com/test/repo",
		DownloadIssues: true,
		DownloadWiki:   true,
	}, runners)
	_, err := testDB.Exec(`
		CREATE TRIGGER fail_issue_cancellation_and_fallback
		BEFORE UPDATE OF status ON execution_components
		WHEN OLD.component = 'issues' AND NEW.status IN ('cancelled', 'failed')
		BEGIN
			SELECT RAISE(FAIL, 'forced cancellation and fallback failure');
		END;
	`)
	require.NoError(t, err)

	jobIDs, err := exec.ExecuteJob("github.com/test/repo")
	require.NoError(t, err)
	<-issueStarted
	require.NoError(t, exec.CancelJob(jobIDs[0]))
	waitForJob(t, exec, jobIDs[0])

	require.False(t, exec.IsJobRunning(jobIDs[0]))
	requireExecution(t, testDB, jobIDs[0], StatusRunning, "")
	requireComponents(t, testDB, jobIDs[0],
		expectedComponent{db.ComponentCode, db.ComponentCompleted, ""},
		expectedComponent{db.ComponentIssue, db.ComponentRunning, ""},
		expectedComponent{db.ComponentWiki, db.ComponentCancelled, "context canceled"},
	)

	wantFallbackLog := fmt.Sprintf(
		`Failed to record issues cancellation store failure: finish component "issues" for execution %q: constraint failed: forced cancellation and fallback failure (1811)`,
		jobIDs[0],
	)
	var fallbackLogCount int
	require.NoError(t, testDB.QueryRow(`
		SELECT COUNT(*) FROM logs
		WHERE execution_id = ? AND level = 'error' AND message = ?`,
		jobIDs[0], wantFallbackLog,
	).Scan(&fallbackLogCount))
	require.Equal(t, 1, fallbackLogCount)
}

func TestExecuteJobCancelledOverallStoreFailureFallsBackToFailed(t *testing.T) {
	issueStarted := make(chan struct{})
	runners := noOpRunners()
	runners.Issue = func(ctx context.Context, _ typedef.Repository, _ []typedef.MultiStorage) error {
		close(issueStarted)
		<-ctx.Done()
		return ctx.Err()
	}
	exec, testDB := newTestExecutorForRepo(t, typedef.Repository{
		Name:           "test-repo",
		URL:            "github.com/test/repo",
		DownloadIssues: true,
		DownloadWiki:   true,
	}, runners)
	_, err := testDB.Exec(`
		CREATE TRIGGER fail_execution_cancellation
		BEFORE UPDATE OF status ON executions
		WHEN NEW.status = 'cancelled'
		BEGIN
			SELECT RAISE(FAIL, 'forced overall cancellation failure');
		END;
	`)
	require.NoError(t, err)

	jobIDs, err := exec.ExecuteJob("github.com/test/repo")
	require.NoError(t, err)
	<-issueStarted
	require.NoError(t, exec.CancelJob(jobIDs[0]))
	waitForJob(t, exec, jobIDs[0])

	persistenceMessage := fmt.Sprintf(
		`persist cancelled execution state: update execution %q to "cancelled": constraint failed: forced overall cancellation failure (1811)`,
		jobIDs[0],
	)
	requireExecution(t, testDB, jobIDs[0], StatusFailed, persistenceMessage)
	requireComponents(t, testDB, jobIDs[0],
		expectedComponent{db.ComponentCode, db.ComponentCompleted, ""},
		expectedComponent{db.ComponentIssue, db.ComponentCancelled, "context canceled"},
		expectedComponent{db.ComponentWiki, db.ComponentCancelled, "context canceled"},
	)
}

func TestExecuteJobComponentStoreFailurePreventsCompletedOverall(t *testing.T) {
	exec, testDB := newTestExecutorForRepo(t, typedef.Repository{
		Name:         "test-repo",
		URL:          "github.com/test/repo",
		DownloadWiki: true,
	}, noOpRunners())
	_, err := testDB.Exec(`
		CREATE TRIGGER fail_wiki_completion
		BEFORE UPDATE OF status ON execution_components
		WHEN OLD.component = 'wiki' AND NEW.status = 'completed'
		BEGIN
			SELECT RAISE(FAIL, 'forced component terminal update failure');
		END;
	`)
	require.NoError(t, err)

	jobIDs, err := exec.ExecuteJob("github.com/test/repo")
	require.NoError(t, err)
	waitForJob(t, exec, jobIDs[0])

	storeMessage := fmt.Sprintf(
		`persist completed state: finish component "wiki" for execution %q: constraint failed: forced component terminal update failure (1811)`,
		jobIDs[0],
	)
	requireExecution(t, testDB, jobIDs[0], StatusFailed, "wiki: "+storeMessage)
	requireComponents(t, testDB, jobIDs[0],
		expectedComponent{db.ComponentCode, db.ComponentCompleted, ""},
		expectedComponent{db.ComponentWiki, db.ComponentFailed, storeMessage},
	)
}

func TestExecuteJobOverallTerminalStoreFailureFallsBackToFailed(t *testing.T) {
	exec, testDB := newTestExecutor(t)
	_, err := testDB.Exec(`
		CREATE TRIGGER fail_execution_completion
		BEFORE UPDATE OF status ON executions
		WHEN NEW.status = 'completed'
		BEGIN
			SELECT RAISE(FAIL, 'forced overall completion failure');
		END;
	`)
	require.NoError(t, err)

	jobIDs, err := exec.ExecuteJob("github.com/test/repo")
	require.NoError(t, err)
	waitForJob(t, exec, jobIDs[0])

	persistenceMessage := fmt.Sprintf(
		`persist completed execution state: update execution %q to "completed": constraint failed: forced overall completion failure (1811)`,
		jobIDs[0],
	)
	requireExecution(t, testDB, jobIDs[0], StatusFailed, persistenceMessage)
	requireComponents(t, testDB, jobIDs[0],
		expectedComponent{db.ComponentCode, db.ComponentCompleted, ""},
	)
}

func TestExecuteJobFailedOverallFallbackRejectionIsDurablyLogged(t *testing.T) {
	exec, testDB := newTestExecutor(t)
	_, err := testDB.Exec(`
		CREATE TRIGGER fail_execution_completion_before_fallback
		BEFORE UPDATE OF status ON executions
		WHEN NEW.status = 'completed'
		BEGIN
			SELECT RAISE(FAIL, 'forced overall completion failure');
		END;

		CREATE TRIGGER fail_execution_failed_fallback
		BEFORE UPDATE OF status ON executions
		WHEN NEW.status = 'failed'
		BEGIN
			SELECT RAISE(FAIL, 'forced overall failed fallback failure');
		END;
	`)
	require.NoError(t, err)

	jobIDs, err := exec.ExecuteJob("github.com/test/repo")
	require.NoError(t, err)
	waitForJob(t, exec, jobIDs[0])

	require.False(t, exec.IsJobRunning(jobIDs[0]))
	requireExecution(t, testDB, jobIDs[0], StatusRunning, "")
	requireComponents(t, testDB, jobIDs[0],
		expectedComponent{db.ComponentCode, db.ComponentCompleted, ""},
	)

	wantLog := fmt.Sprintf(
		`Failed to record overall execution store failure: update execution %q to "failed": constraint failed: forced overall failed fallback failure (1811)`,
		jobIDs[0],
	)
	var count int
	require.NoError(t, testDB.QueryRow(`
		SELECT COUNT(*) FROM logs
		WHERE execution_id = ? AND level = 'error' AND message = ?`,
		jobIDs[0], wantLog,
	).Scan(&count))
	require.Equal(t, 1, count)
}

func TestExecuteJobCreatesRecord(t *testing.T) {
	codeStarted := make(chan struct{})
	runners := noOpRunners()
	runners.Code = func(ctx context.Context, _ typedef.Repository, _ []typedef.MultiStorage) error {
		close(codeStarted)
		<-ctx.Done()
		return ctx.Err()
	}
	exec, testDB := newTestExecutorForRepo(t, typedef.Repository{Name: "test-repo", URL: "github.com/test/repo"}, runners)

	jobIDs, err := exec.ExecuteJob("github.com/test/repo")
	assert.NoError(t, err)
	require.Len(t, jobIDs, 1)
	assert.NotEmpty(t, jobIDs[0])
	<-codeStarted

	var status string
	err = testDB.QueryRow("SELECT status FROM executions WHERE id = ?", jobIDs[0]).Scan(&status)
	assert.NoError(t, err)
	assert.Equal(t, "running", status)
	assert.True(t, exec.IsJobRunning(jobIDs[0]))

	require.NoError(t, exec.CancelJob(jobIDs[0]))
	waitForJob(t, exec, jobIDs[0])
}

func TestExecuteJobUnknownRepository(t *testing.T) {
	exec, testDB := newTestExecutor(t)

	_, err := exec.ExecuteJob("does-not-exist")
	require.ErrorIs(t, err, ErrRepositoryNotFound)
	require.Empty(t, executionStatusCounts(t, testDB))
}

func TestCancelNonRunningJob(t *testing.T) {
	exec, _ := newTestExecutor(t)

	err := exec.CancelJob("never-started")
	require.EqualError(t, err, `update execution "never-started" to "cancelled": expected one row, updated 0`)
}

func TestExecuteJobWritesRepoKey(t *testing.T) {
	exec, testDB := newTestExecutor(t)

	jobIDs, err := exec.ExecuteJob("https://github.com/test/repo")
	require.NoError(t, err)
	require.Len(t, jobIDs, 1)

	var repoKey string
	err = testDB.QueryRow("SELECT repo_key FROM executions WHERE id = ?", jobIDs[0]).Scan(&repoKey)
	assert.NoError(t, err)
	assert.Equal(t, "github.com/test/repo", repoKey)
	waitForJob(t, exec, jobIDs[0])
}

func TestExecuteJobExpandsOrgIntoMultipleJobs(t *testing.T) {
	exec, testDB := newTestExecutor(t)
	exec.RefreshConfig(&config.Config{Repository: []typedef.Repository{
		{Name: "acme", URL: "https://github.com/acme", Type: typedef.TypeOrg, OrgName: "acme"},
	}})

	old := expandRepos
	t.Cleanup(func() { expandRepos = old })
	expandRepos = func(_ context.Context, repo typedef.Repository) ([]typedef.Repository, error) {
		return []typedef.Repository{
			{Name: "alpha", URL: "github.com/acme/alpha"},
			{Name: "beta", URL: "github.com/acme/beta"},
		}, nil
	}

	jobIDs, err := exec.ExecuteJob("github.com/acme")
	require.NoError(t, err)
	require.Len(t, jobIDs, 2)

	keys := map[string]bool{}
	for _, id := range jobIDs {
		var repoKey string
		require.NoError(t, testDB.QueryRow("SELECT repo_key FROM executions WHERE id = ?", id).Scan(&repoKey))
		keys[repoKey] = true
		waitForJob(t, exec, id)
	}
	assert.True(t, keys["github.com/acme/alpha"])
	assert.True(t, keys["github.com/acme/beta"])
}

func TestExecuteJobPropagatesProductionExpansionFailure(t *testing.T) {
	expansionErr := errors.New("GitHub repository listing failed")
	old := expandRepos
	t.Cleanup(func() { expandRepos = old })
	expandRepos = func(context.Context, typedef.Repository) ([]typedef.Repository, error) {
		return nil, expansionErr
	}
	exec, testDB := newTestExecutorForConfig(t, &config.Config{Repository: []typedef.Repository{{
		Name: "acme", URL: "github.com/acme", Type: typedef.TypeOrg, OrgName: "acme",
	}}}, noOpRunners())

	jobIDs, err := exec.ExecuteJob("github.com/acme")
	require.ErrorIs(t, err, expansionErr)
	require.Empty(t, jobIDs)
	require.Empty(t, executionStatusCounts(t, testDB))
}

func TestExecuteJobRejectsEmptyExpansionAsTypedEnqueueFailure(t *testing.T) {
	cfg := &config.Config{Repository: []typedef.Repository{{
		Name: "empty", URL: "github.com/empty", Type: typedef.TypeUser, OrgName: "empty",
	}}}
	testDB, err := db.Initialize(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testDB.Close()) })
	exec := NewExecutorWithRunners(logger.NewLogger(testDB), testDB, cfg, noOpRunners(),
		func(typedef.Repository) []typedef.Repository { return nil },
	)
	t.Cleanup(func() { require.NoError(t, exec.Close()) })

	jobIDs, err := exec.ExecuteJob("github.com/empty")
	var empty *RepositoryExpansionEmptyError
	require.ErrorAs(t, err, &empty)
	require.Equal(t, "github.com/empty", empty.RepositoryKey)
	require.Empty(t, jobIDs)
	require.Empty(t, executionStatusCounts(t, testDB))
}

func TestRefreshConfigRepointsExecutor(t *testing.T) {
	exec, _ := newTestExecutor(t)

	exec.RefreshConfig(&config.Config{Repository: []typedef.Repository{
		{Name: "other", URL: "github.com/other/repo"},
	}})

	_, err := exec.ExecuteJob("github.com/test/repo")
	require.Error(t, err)

	jobIDs, err := exec.ExecuteJob("github.com/other/repo")
	require.NoError(t, err)
	require.Len(t, jobIDs, 1)
	waitForJob(t, exec, jobIDs[0])
}

func TestRuntimeConfigSnapshotDefensivelyCopiesAndIndexesRepositories(t *testing.T) {
	cfg := &config.Config{SyncOverdueGrace: time.Minute, SyncStuckThreshold: time.Hour, Repository: []typedef.Repository{{
		Name:    "original",
		URL:     "https://github.com/acme/original.git",
		Storage: []string{"archive"},
	}}}
	exec, _ := newTestExecutorForConfig(t, cfg, noOpRunners())

	snapshot := exec.RuntimeConfigSnapshot()
	cfg.Repository[0].URL = "github.com/acme/changed"
	cfg.Repository[0].Storage[0] = "changed"
	cfg.SyncOverdueGrace = 2 * time.Minute
	cfg.SyncStuckThreshold = 2 * time.Hour

	repository, found := snapshot.Repository("github.com/acme/original")
	require.True(t, found)
	require.Equal(t, "https://github.com/acme/original.git", repository.URL)
	require.Equal(t, []string{"archive"}, repository.Storage)
	repository.Storage[0] = "caller mutation"
	repositories := snapshot.Repositories()
	repositories[0].URL = "caller mutation"

	repository, found = exec.RuntimeConfigSnapshot().Repository("https://github.com/acme/original")
	require.True(t, found)
	require.Equal(t, "https://github.com/acme/original.git", repository.URL)
	require.Equal(t, []string{"archive"}, repository.Storage)
	overdueGrace, stuckThreshold := exec.RuntimeConfigSnapshot().SyncHealthThresholds()
	require.Equal(t, time.Minute, overdueGrace)
	require.Equal(t, time.Hour, stuckThreshold)
	_, found = exec.RuntimeConfigSnapshot().Repository("github.com/acme/changed")
	require.False(t, found)

	jobIDs, err := exec.ExecuteJob("github.com/acme/original")
	require.NoError(t, err)
	waitForJob(t, exec, jobIDs[0])
}

func TestExecuteJobAtGenerationRejectsConfigPublishedAfterRecheck(t *testing.T) {
	exec, testDB := newTestExecutorForRepo(t, typedef.Repository{
		Name: "old",
		URL:  "github.com/acme/old",
	}, noOpRunners())
	checked := exec.RuntimeConfigSnapshot()
	exec.RefreshConfig(&config.Config{Repository: []typedef.Repository{{
		Name: "new",
		URL:  "github.com/acme/new",
	}}})

	jobIDs, err := exec.ExecuteJobAtGeneration("github.com/acme/old", checked.Generation())
	require.ErrorIs(t, err, ErrConfigGenerationChanged)
	require.Empty(t, jobIDs)
	require.Empty(t, executionStatusCounts(t, testDB))

	current := exec.RuntimeConfigSnapshot()
	require.Greater(t, current.Generation(), checked.Generation())
	jobIDs, err = exec.ExecuteJobAtGeneration("github.com/acme/new", current.Generation())
	require.NoError(t, err)
	require.Len(t, jobIDs, 1)
	waitForJob(t, exec, jobIDs[0])
}

func TestExecuteJobAtGenerationRejectsRefreshBetweenLookupAndReservation(t *testing.T) {
	expansionEntered := make(chan struct{})
	expansionRelease := make(chan struct{})
	testDB, err := db.Initialize(":memory:")
	require.NoError(t, err)
	exec := NewExecutorWithRunners(
		logger.NewLogger(testDB),
		testDB,
		&config.Config{Repository: []typedef.Repository{{Name: "old", URL: "github.com/acme/old"}}},
		noOpRunners(),
		func(repo typedef.Repository) []typedef.Repository {
			close(expansionEntered)
			<-expansionRelease
			return []typedef.Repository{repo}
		},
	)
	t.Cleanup(func() {
		require.NoError(t, exec.Close())
		require.NoError(t, testDB.Close())
	})

	checked := exec.RuntimeConfigSnapshot()
	type result struct {
		ids []string
		err error
	}
	resultReady := make(chan result, 1)
	go func() {
		ids, executeErr := exec.ExecuteJobAtGeneration("github.com/acme/old", checked.Generation())
		resultReady <- result{ids: ids, err: executeErr}
	}()

	<-expansionEntered
	exec.RefreshConfig(&config.Config{Repository: []typedef.Repository{{Name: "new", URL: "github.com/acme/new"}}})
	close(expansionRelease)

	got := <-resultReady
	require.ErrorIs(t, got.err, ErrConfigGenerationChanged)
	require.Empty(t, got.ids)
	require.Empty(t, executionStatusCounts(t, testDB))
}

func TestQueuedJobUsesStorageFromAcceptedRuntimeGeneration(t *testing.T) {
	storagesSeen := make(chan []typedef.MultiStorage, 1)
	runners := noOpRunners()
	runners.Code = func(_ context.Context, _ typedef.Repository, storages []typedef.MultiStorage) error {
		storagesSeen <- storages
		return nil
	}
	cfg := &config.Config{
		Repository: []typedef.Repository{{Name: "repo", URL: "github.com/acme/repo", Storage: []string{"archive"}}},
		Storage:    []typedef.MultiStorage{{Storage: typedef.Storage{Name: "archive", Type: "file", Path: "old"}}},
	}
	exec, testDB := newTestExecutorForConfig(t, cfg, runners)
	execEntered := make(chan struct{})
	execRelease := make(chan struct{})
	exec.db = &blockingExecutionStore{
		delegate:    testDB,
		execEntered: execEntered,
		execRelease: execRelease,
	}

	jobIDs, err := exec.ExecuteJob("github.com/acme/repo")
	require.NoError(t, err)
	require.Len(t, jobIDs, 1)
	<-execEntered
	exec.RefreshConfig(&config.Config{
		Repository: []typedef.Repository{{Name: "repo", URL: "github.com/acme/repo", Storage: []string{"archive"}}},
		Storage:    []typedef.MultiStorage{{Storage: typedef.Storage{Name: "archive", Type: "file", Path: "new"}}},
	})
	close(execRelease)

	storages := <-storagesSeen
	require.Len(t, storages, 1)
	require.Equal(t, "old", storages[0].Path)
	waitForJob(t, exec, jobIDs[0])
}

func TestQueuedJobUsesOneCompleteAcceptedConfigGeneration(t *testing.T) {
	type observation struct {
		component string
		cfg       *config.Config
		api       githubapi.Config
		storages  []typedef.MultiStorage
	}

	blockerEntered := make(chan struct{})
	blockerRelease := make(chan struct{})
	observed := make(chan observation, 5)
	record := func(component string) SyncFunc {
		return func(ctx context.Context, repo typedef.Repository, storages []typedef.MultiStorage) error {
			if component == "code" && repo.Name == "blocker" {
				close(blockerEntered)
				<-blockerRelease
				return nil
			}
			apiConfig, ok := githubapi.ConfigFromContext(ctx)
			require.True(t, ok, "%s runner has no frozen GitHub API coordinator", component)
			observed <- observation{
				component: component,
				cfg:       config.GetExecutionConfig(ctx),
				api:       apiConfig,
				storages:  append([]typedef.MultiStorage(nil), storages...),
			}
			return nil
		}
	}
	runners := Runners{
		Code:       record("code"),
		Release:    record("release"),
		Issue:      record("issue"),
		Wiki:       record("wiki"),
		Discussion: record("discussion"),
	}
	oldConfig := &config.Config{
		Repository: []typedef.Repository{
			{Name: "blocker", URL: "github.com/acme/blocker"},
			{
				Name:               "target",
				URL:                "github.com/acme/target",
				Storage:            []string{"archive"},
				DownloadReleases:   true,
				DownloadIssues:     true,
				DownloadWiki:       true,
				DownloadDiscussion: true,
			},
		},
		Storage: []typedef.MultiStorage{{
			Storage: typedef.Storage{Name: "archive", Type: "file", Path: "old-path"},
		}},
		GitHubToken:                 "old-token",
		ConcurrencyNum:              1,
		ReleaseSizeLimit:            101,
		ReleaseNumLimit:             102,
		RetryMaxCount:               103,
		RetryBaseDelay:              104 * time.Millisecond,
		SyncOverdueGrace:            105 * time.Minute,
		SyncStuckThreshold:          106 * time.Hour,
		GitHubAPIConcurrency:        107,
		GitHubMinRequestInterval:    108 * time.Millisecond,
		GitHubLowRemainingThreshold: 109,
		GitHubScheduleJitter:        110 * time.Millisecond,
	}
	previousGlobal := config.GetIns()
	config.SetIns(oldConfig)
	t.Cleanup(func() { config.SetIns(previousGlobal) })
	exec, _ := newTestExecutorForConfig(t, oldConfig, runners)

	blockerIDs, err := exec.ExecuteJob("github.com/acme/blocker")
	require.NoError(t, err)
	<-blockerEntered
	targetIDs, err := exec.ExecuteJob("github.com/acme/target")
	require.NoError(t, err)

	newConfig := config.Clone(oldConfig)
	newConfig.GitHubToken = "new-token"
	newConfig.ConcurrencyNum = 2
	newConfig.ReleaseSizeLimit = 201
	newConfig.ReleaseNumLimit = 202
	newConfig.RetryMaxCount = 203
	newConfig.RetryBaseDelay = 204 * time.Millisecond
	newConfig.SyncOverdueGrace = 205 * time.Minute
	newConfig.SyncStuckThreshold = 206 * time.Hour
	newConfig.GitHubAPIConcurrency = 207
	newConfig.GitHubMinRequestInterval = 208 * time.Millisecond
	newConfig.GitHubLowRemainingThreshold = 209
	newConfig.GitHubScheduleJitter = 210 * time.Millisecond
	newConfig.Storage[0].Path = "new-path"
	exec.RefreshConfig(newConfig)
	config.SetIns(newConfig)
	close(blockerRelease)

	wantStorage := oldConfig.Storage[0]
	seen := make(map[string]bool)
	for range 5 {
		got := <-observed
		seen[got.component] = true
		require.Equal(t, oldConfig, got.cfg, "%s runner mixed configuration generations", got.component)
		require.Equal(t, githubapi.Config{
			Concurrency:           oldConfig.GitHubAPIConcurrency,
			MinRequestInterval:    oldConfig.GitHubMinRequestInterval,
			LowRemainingThreshold: oldConfig.GitHubLowRemainingThreshold,
		}, got.api, "%s runner selected the wrong GitHub coordinator", got.component)
		require.Equal(t, []typedef.MultiStorage{wantStorage}, got.storages)
	}
	require.Equal(t, map[string]bool{
		"code": true, "release": true, "issue": true, "wiki": true, "discussion": true,
	}, seen)
	waitForJob(t, exec, blockerIDs[0])
	waitForJob(t, exec, targetIDs[0])
}

func TestExecuteJobAtGenerationContextDiscardsCommitWhenCancelledBeforePublish(t *testing.T) {
	var runnerCalls atomic.Int32
	runners := noOpRunners()
	runners.Code = func(context.Context, typedef.Repository, []typedef.MultiStorage) error {
		runnerCalls.Add(1)
		return nil
	}
	exec, testDB := newTestExecutorForRepo(t, typedef.Repository{
		Name: "repo",
		URL:  "github.com/acme/repo",
	}, runners)
	componentsEntered := make(chan struct{})
	componentsRelease := make(chan struct{})
	exec.db = &blockingExecutionStore{
		delegate:          testDB,
		componentsEntered: componentsEntered,
		componentsRelease: componentsRelease,
	}

	ctx, cancel := context.WithCancel(context.Background())
	generation := exec.RuntimeConfigSnapshot().Generation()
	type result struct {
		ids []string
		err error
	}
	resultReady := make(chan result, 1)
	go func() {
		ids, executeErr := exec.ExecuteJobAtGenerationContext(ctx, "github.com/acme/repo", generation)
		resultReady <- result{ids: ids, err: executeErr}
	}()

	<-componentsEntered
	cancel()
	close(componentsRelease)
	got := <-resultReady
	require.ErrorIs(t, got.err, context.Canceled)
	require.Empty(t, got.ids)
	require.Empty(t, executionStatusCounts(t, testDB))
	require.Zero(t, runnerCalls.Load())

	jobIDs, err := exec.ExecuteJob("github.com/acme/repo")
	require.NoError(t, err, "cancelled persistence must release repository ownership")
	require.Len(t, jobIDs, 1)
	waitForJob(t, exec, jobIDs[0])
	require.Equal(t, int32(1), runnerCalls.Load())
}

func TestReservedEligibilityRejectionIsTypedAndReleasesAdmission(t *testing.T) {
	exec, testDB := newTestExecutorForRepo(t, typedef.Repository{
		Name: "repo",
		URL:  "github.com/acme/repo",
	}, noOpRunners())
	runtime := exec.RuntimeConfigSnapshot()

	jobIDs, err := exec.ExecuteJobAtGenerationIfEligibleContext(
		context.Background(),
		"github.com/acme/repo",
		runtime.Generation(),
		func(_ context.Context, admitted *RuntimeConfigSnapshot, repo typedef.Repository) (bool, error) {
			require.Equal(t, runtime.Generation(), admitted.Generation())
			require.Equal(t, "github.com/acme/repo", repo.Key())
			return false, nil
		},
	)
	var noLongerEligible *RepositoryNoLongerEligibleError
	require.ErrorAs(t, err, &noLongerEligible)
	require.Equal(t, "github.com/acme/repo", noLongerEligible.RepositoryKey)
	require.Empty(t, jobIDs)
	require.Empty(t, executionStatusCounts(t, testDB))

	jobIDs, err = exec.ExecuteJob("github.com/acme/repo")
	require.NoError(t, err, "eligibility rejection must release the repository reservation")
	require.Len(t, jobIDs, 1)
	waitForJob(t, exec, jobIDs[0])
}

func TestReservedEligibilityCancellationReleasesAdmissionWithoutPersistence(t *testing.T) {
	exec, testDB := newTestExecutorForRepo(t, typedef.Repository{
		Name: "repo",
		URL:  "github.com/acme/repo",
	}, noOpRunners())
	runtime := exec.RuntimeConfigSnapshot()
	ctx, cancel := context.WithCancel(context.Background())

	jobIDs, err := exec.ExecuteJobAtGenerationIfEligibleContext(
		ctx,
		"github.com/acme/repo",
		runtime.Generation(),
		func(context.Context, *RuntimeConfigSnapshot, typedef.Repository) (bool, error) {
			cancel()
			return false, context.Canceled
		},
	)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, jobIDs)
	require.Empty(t, executionStatusCounts(t, testDB))

	jobIDs, err = exec.ExecuteJob("github.com/acme/repo")
	require.NoError(t, err, "cancelled eligibility must release the repository reservation")
	require.Len(t, jobIDs, 1)
	waitForJob(t, exec, jobIDs[0])
}

func TestExecuteJobLimitsActualRunnerConcurrencyAndKeepsOverflowPending(t *testing.T) {
	blocker := newBlockingRunner(6)
	exec, testDB := newTestExecutorForConfig(t, repositoryConfig(2, 6), blocker.runners())

	jobIDs := make([]string, 0, 6)
	for i := 0; i < 6; i++ {
		ids, err := exec.ExecuteJob(fmt.Sprintf("github.com/test/repo-%d", i))
		require.NoError(t, err)
		require.Len(t, ids, 1)
		jobIDs = append(jobIDs, ids[0])
	}

	requireRunnerEntry(t, blocker.entered)
	requireRunnerEntry(t, blocker.entered)
	require.Equal(t, map[string]int{"pending": 4, "running": 2}, executionStatusCounts(t, testDB))
	require.Equal(t, int32(2), blocker.running.Load())
	require.Equal(t, int32(2), blocker.maxRunning.Load())

	close(blocker.release)
	for _, jobID := range jobIDs {
		waitForJob(t, exec, jobID)
	}
	require.Equal(t, int32(2), blocker.maxRunning.Load())
}

func TestExecuteJobDefaultsZeroConcurrencyToThree(t *testing.T) {
	blocker := newBlockingRunner(4)
	exec, testDB := newTestExecutorForConfig(t, repositoryConfig(0, 4), blocker.runners())
	for i := 0; i < 4; i++ {
		_, err := exec.ExecuteJob(fmt.Sprintf("github.com/test/repo-%d", i))
		require.NoError(t, err)
	}

	requireRunnerEntry(t, blocker.entered)
	requireRunnerEntry(t, blocker.entered)
	requireRunnerEntry(t, blocker.entered)
	require.Equal(t, map[string]int{"pending": 1, "running": 3}, executionStatusCounts(t, testDB))
	require.Equal(t, int32(3), blocker.maxRunning.Load())
}

func TestExecuteJobRejectsDuplicateRepositoryReservedInThisProcess(t *testing.T) {
	blocker := newBlockingRunner(1)
	exec, testDB := newTestExecutorForConfig(t, repositoryConfig(1, 1), blocker.runners())

	_, err := exec.ExecuteJob("https://github.com/test/repo-0")
	require.NoError(t, err)
	requireRunnerEntry(t, blocker.entered)

	_, err = exec.ExecuteJob("github.com/test/repo-0")
	require.ErrorIs(t, err, ErrRepositoryActive)
	var count int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM executions`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestExecuteJobAtomicallyReservesRepositoryAcrossConcurrentRequests(t *testing.T) {
	blocker := newBlockingRunner(1)
	exec, testDB := newTestExecutorForConfig(t, repositoryConfig(1, 1), blocker.runners())
	start := make(chan struct{})
	results := make(chan error, 8)
	var callers sync.WaitGroup
	for i := 0; i < cap(results); i++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			<-start
			_, err := exec.ExecuteJob("https://github.com/test/repo-0")
			results <- err
		}()
	}
	close(start)
	callers.Wait()
	close(results)

	succeeded := 0
	active := 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrRepositoryActive):
			active++
		default:
			t.Fatalf("unexpected enqueue error: %v", err)
		}
	}
	require.Equal(t, 1, succeeded)
	require.Equal(t, 7, active)
	var count int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM executions`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestExecuteJobRejectsDuplicateRepositoryAlreadyActiveInSQLite(t *testing.T) {
	exec, testDB := newTestExecutorForConfig(t, repositoryConfig(1, 1), noOpRunners())
	_, err := testDB.Exec(`
		INSERT INTO executions (id, job_name, repo_key, start_time, status)
		VALUES (?, ?, ?, ?, ?)`, "existing", "repo-0", "github.com/test/repo-0", time.Now(), StatusPending)
	require.NoError(t, err)

	_, err = exec.ExecuteJob("github.com/test/repo-0")
	require.ErrorIs(t, err, ErrRepositoryActive)
	var count int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM executions`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestExecuteJobReleasesRepositoryReservationAfterExecutionInsertFails(t *testing.T) {
	exec, testDB := newTestExecutorForConfig(t, repositoryConfig(1, 1), noOpRunners())
	_, err := testDB.Exec(`
		CREATE TRIGGER reject_execution_insert
		BEFORE INSERT ON executions
		BEGIN
			SELECT RAISE(FAIL, 'forced execution insert failure');
		END;`)
	require.NoError(t, err)

	_, err = exec.ExecuteJob("github.com/test/repo-0")
	require.ErrorContains(t, err, "forced execution insert failure")
	_, err = testDB.Exec(`DROP TRIGGER reject_execution_insert`)
	require.NoError(t, err)

	jobIDs, err := exec.ExecuteJob("github.com/test/repo-0")
	require.NoError(t, err)
	require.Len(t, jobIDs, 1)
	waitForJob(t, exec, jobIDs[0])
}

func TestExecuteJobReleasesRepositoryReservationAfterComponentInsertFails(t *testing.T) {
	exec, testDB := newTestExecutorForConfig(t, repositoryConfig(1, 1), noOpRunners())
	_, err := testDB.Exec(`
		CREATE TRIGGER reject_component_insert
		BEFORE INSERT ON execution_components
		BEGIN
			SELECT RAISE(FAIL, 'forced component insert failure');
		END;`)
	require.NoError(t, err)

	_, err = exec.ExecuteJob("github.com/test/repo-0")
	require.ErrorContains(t, err, "forced component insert failure")
	_, err = testDB.Exec(`DROP TRIGGER reject_component_insert`)
	require.NoError(t, err)

	jobIDs, err := exec.ExecuteJob("github.com/test/repo-0")
	require.NoError(t, err)
	require.Len(t, jobIDs, 1)
	waitForJob(t, exec, jobIDs[0])
}

func TestCancelJobRemovesQueuedWorkWithoutCallingRunner(t *testing.T) {
	blocker := newBlockingRunner(2)
	exec, testDB := newTestExecutorForConfig(t, repositoryConfig(1, 2), blocker.runners())

	firstIDs, err := exec.ExecuteJob("github.com/test/repo-0")
	require.NoError(t, err)
	requireRunnerEntry(t, blocker.entered)
	queuedIDs, err := exec.ExecuteJob("github.com/test/repo-1")
	require.NoError(t, err)

	require.NoError(t, exec.CancelJob(queuedIDs[0]))
	requireExecution(t, testDB, queuedIDs[0], StatusCancelled, "")
	requireComponents(t, testDB, queuedIDs[0],
		expectedComponent{db.ComponentCode, db.ComponentCancelled, "context canceled"},
	)
	require.Equal(t, int32(1), blocker.running.Load())
	select {
	case repoKey := <-blocker.entered:
		t.Fatalf("queued repository entered runner: %s", repoKey)
	default:
	}

	close(blocker.release)
	waitForJob(t, exec, firstIDs[0])
}

func TestRefreshConfigRaisesLiveAdmissionLimit(t *testing.T) {
	blocker := newBlockingRunner(3)
	exec, testDB := newTestExecutorForConfig(t, repositoryConfig(1, 3), blocker.runners())
	for i := 0; i < 3; i++ {
		_, err := exec.ExecuteJob(fmt.Sprintf("github.com/test/repo-%d", i))
		require.NoError(t, err)
	}
	requireRunnerEntry(t, blocker.entered)
	require.Equal(t, map[string]int{"pending": 2, "running": 1}, executionStatusCounts(t, testDB))

	exec.RefreshConfig(repositoryConfig(2, 3))
	requireRunnerEntry(t, blocker.entered)
	require.Equal(t, map[string]int{"pending": 1, "running": 2}, executionStatusCounts(t, testDB))
	require.Equal(t, int32(2), blocker.maxRunning.Load())
}

func TestCloseCancelsQueuedAndRunningWorkAndIsIdempotent(t *testing.T) {
	blocker := newBlockingRunner(3)
	exec, testDB := newTestExecutorForConfig(t, repositoryConfig(1, 3), blocker.runners())
	jobIDs := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		ids, err := exec.ExecuteJob(fmt.Sprintf("github.com/test/repo-%d", i))
		require.NoError(t, err)
		jobIDs = append(jobIDs, ids[0])
	}
	requireRunnerEntry(t, blocker.entered)

	require.NoError(t, exec.Close())
	require.NoError(t, exec.Close())
	require.Equal(t, map[string]int{"cancelled": 3}, executionStatusCounts(t, testDB))
	require.Equal(t, int32(0), blocker.running.Load())
	select {
	case repoKey := <-blocker.entered:
		t.Fatalf("queued repository entered runner during close: %s", repoKey)
	default:
	}
	for _, jobID := range jobIDs {
		require.False(t, exec.IsJobRunning(jobID))
	}

	_, err := exec.ExecuteJob("github.com/test/repo-0")
	require.ErrorIs(t, err, ErrExecutorClosed)
}

func TestCloseBeforeFirstEnqueueIsIdempotent(t *testing.T) {
	exec, _ := newTestExecutorForConfig(t, nil, noOpRunners())
	require.NoError(t, exec.Close())
	require.NoError(t, exec.Close())
}

func TestBlockedSubmissionDoesNotPreventCloseFromCancellingRunningWork(t *testing.T) {
	runnerEntered := make(chan struct{})
	runnerCancelled := make(chan struct{})
	runners := noOpRunners()
	runners.Code = func(ctx context.Context, _ typedef.Repository, _ []typedef.MultiStorage) error {
		close(runnerEntered)
		<-ctx.Done()
		close(runnerCancelled)
		return ctx.Err()
	}
	exec, testDB := newTestExecutorForConfig(t, repositoryConfig(1, 2), runners)
	_, err := exec.ExecuteJob("github.com/test/repo-0")
	require.NoError(t, err)
	<-runnerEntered

	activeRelease := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(activeRelease) }) })
	store := &blockingExecutionStore{
		delegate:      testDB,
		activeKey:     "github.com/test/repo-1",
		activeEntered: make(chan struct{}),
		activeRelease: activeRelease,
	}
	exec.db = store
	submitResult := make(chan error, 1)
	go func() {
		_, submitErr := exec.ExecuteJob("github.com/test/repo-1")
		submitResult <- submitErr
	}()
	<-store.activeEntered

	closed := executorClosedSignal(exec)
	closeResult := make(chan error, 1)
	go func() { closeResult <- exec.Close() }()
	<-closed
	<-runnerCancelled
	select {
	case err := <-closeResult:
		t.Fatalf("Close returned before the in-flight submission completed: %v", err)
	default:
	}

	releaseOnce.Do(func() { close(activeRelease) })
	require.ErrorIs(t, <-submitResult, ErrExecutorClosed)
	require.NoError(t, <-closeResult)
}

func TestUnknownJobCancellationPersistenceDoesNotHoldQueueLock(t *testing.T) {
	exec, testDB := newTestExecutorForConfig(t, nil, noOpRunners())
	execRelease := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(execRelease) }) })
	store := &blockingExecutionStore{
		delegate:    testDB,
		execEntered: make(chan struct{}),
		execRelease: execRelease,
	}
	exec.db = store
	cancelResult := make(chan error, 1)
	go func() { cancelResult <- exec.CancelJob("unknown") }()
	<-store.execEntered

	closed := executorClosedSignal(exec)
	closeResult := make(chan error, 1)
	go func() { closeResult <- exec.Close() }()
	<-closed
	select {
	case err := <-closeResult:
		t.Fatalf("Close returned before unknown-job persistence completed: %v", err)
	default:
	}

	releaseOnce.Do(func() { close(execRelease) })
	require.Error(t, <-cancelResult)
	require.NoError(t, <-closeResult)
}

func TestExecuteJobExpandedBatchIsAtomicWhenSecondRepositoryIsActive(t *testing.T) {
	runnerEntered := make(chan string, 2)
	runners := noOpRunners()
	runners.Code = func(ctx context.Context, repo typedef.Repository, _ []typedef.MultiStorage) error {
		runnerEntered <- repo.Key()
		<-ctx.Done()
		return ctx.Err()
	}
	exec, testDB := newTestExecutorForConfig(t, &config.Config{Repository: []typedef.Repository{{
		Name: "acme", URL: "github.com/acme", Type: typedef.TypeOrg, OrgName: "acme",
	}}}, runners)
	_, err := testDB.Exec(`
		INSERT INTO executions (id, job_name, repo_key, start_time, status)
		VALUES (?, ?, ?, ?, ?)`, "active-beta", "beta", "github.com/acme/beta", time.Now(), StatusRunning)
	require.NoError(t, err)

	old := expandRepos
	t.Cleanup(func() { expandRepos = old })
	expandRepos = func(context.Context, typedef.Repository) ([]typedef.Repository, error) {
		return []typedef.Repository{
			{Name: "alpha", URL: "github.com/acme/alpha"},
			{Name: "beta", URL: "github.com/acme/beta"},
		}, nil
	}

	jobIDs, err := exec.ExecuteJob("github.com/acme")
	require.ErrorIs(t, err, ErrRepositoryActive)
	require.Empty(t, jobIDs)
	var count int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM executions`).Scan(&count))
	require.Equal(t, 1, count)
	select {
	case repoKey := <-runnerEntered:
		t.Fatalf("expanded batch started hidden work for %s", repoKey)
	default:
	}
}

func TestExecuteJobExpandedBatchRollsBackAllRowsWhenLaterPersistenceFails(t *testing.T) {
	runnerEntered := make(chan string, 2)
	runners := noOpRunners()
	runners.Code = func(ctx context.Context, repo typedef.Repository, _ []typedef.MultiStorage) error {
		runnerEntered <- repo.Key()
		<-ctx.Done()
		return ctx.Err()
	}
	exec, testDB := newTestExecutorForConfig(t, &config.Config{Repository: []typedef.Repository{{
		Name: "acme", URL: "github.com/acme", Type: typedef.TypeOrg, OrgName: "acme",
	}}}, runners)
	_, err := testDB.Exec(`
		CREATE TRIGGER reject_beta_component
		BEFORE INSERT ON execution_components
		WHEN (SELECT repo_key FROM executions WHERE id = NEW.execution_id) = 'github.com/acme/beta'
		BEGIN
			SELECT RAISE(FAIL, 'forced beta component failure');
		END;`)
	require.NoError(t, err)

	old := expandRepos
	t.Cleanup(func() { expandRepos = old })
	expandRepos = func(context.Context, typedef.Repository) ([]typedef.Repository, error) {
		return []typedef.Repository{
			{Name: "alpha", URL: "github.com/acme/alpha"},
			{Name: "beta", URL: "github.com/acme/beta"},
		}, nil
	}

	jobIDs, err := exec.ExecuteJob("github.com/acme")
	require.ErrorContains(t, err, "forced beta component failure")
	require.Empty(t, jobIDs)
	require.Empty(t, executionStatusCounts(t, testDB))
	var componentCount int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM execution_components`).Scan(&componentCount))
	require.Zero(t, componentCount)
	exec.queueMu.Lock()
	require.Empty(t, exec.activeByRepo)
	require.Zero(t, exec.inflight)
	exec.queueMu.Unlock()
	select {
	case repoKey := <-runnerEntered:
		t.Fatalf("failed expanded batch started hidden work for %s", repoKey)
	default:
	}

	_, err = testDB.Exec(`DROP TRIGGER reject_beta_component`)
	require.NoError(t, err)
	retryIDs, err := exec.ExecuteJob("github.com/acme")
	require.NoError(t, err)
	require.Len(t, retryIDs, 2)
	requireRunnerEntry(t, runnerEntered)
	requireRunnerEntry(t, runnerEntered)
}

func TestExecuteJobRejectsDuplicateKeysWithinExpandedBatchBeforePersistence(t *testing.T) {
	exec, testDB := newTestExecutorForConfig(t, &config.Config{Repository: []typedef.Repository{{
		Name: "acme", URL: "github.com/acme", Type: typedef.TypeOrg, OrgName: "acme",
	}}}, noOpRunners())
	old := expandRepos
	t.Cleanup(func() { expandRepos = old })
	expandRepos = func(context.Context, typedef.Repository) ([]typedef.Repository, error) {
		return []typedef.Repository{
			{Name: "alpha", URL: "https://github.com/acme/alpha"},
			{Name: "alpha-copy", URL: "github.com/acme/alpha/"},
		}, nil
	}

	jobIDs, err := exec.ExecuteJob("github.com/acme")
	require.ErrorIs(t, err, ErrRepositoryActive)
	require.Empty(t, jobIDs)
	var count int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM executions`).Scan(&count))
	require.Zero(t, count)
}

func TestRefreshConfigDownsizeWaitsUntilRunningFallsBelowNewLimit(t *testing.T) {
	cfg := repositoryConfig(3, 4)
	entered := make(chan string, 4)
	gates := make(map[string]chan struct{}, 4)
	for _, repo := range cfg.Repository {
		gates[repo.Key()] = make(chan struct{})
	}
	runners := noOpRunners()
	runners.Code = func(ctx context.Context, repo typedef.Repository, _ []typedef.MultiStorage) error {
		entered <- repo.Key()
		select {
		case <-gates[repo.Key()]:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	exec, _ := newTestExecutorForConfig(t, cfg, runners)
	jobByRepo := make(map[string]string, 4)
	for _, repo := range cfg.Repository {
		ids, err := exec.ExecuteJob(repo.Key())
		require.NoError(t, err)
		jobByRepo[repo.Key()] = ids[0]
	}
	first := requireRunnerEntry(t, entered)
	second := requireRunnerEntry(t, entered)
	third := requireRunnerEntry(t, entered)

	exec.RefreshConfig(repositoryConfig(2, 4))
	close(gates[first])
	waitForJob(t, exec, jobByRepo[first])
	exec.queueMu.Lock()
	require.Equal(t, 2, exec.running)
	require.Len(t, exec.pending, 1)
	exec.queueMu.Unlock()
	select {
	case repoKey := <-entered:
		t.Fatalf("replacement %s started while running equalled downsized limit", repoKey)
	default:
	}

	close(gates[second])
	fourth := requireRunnerEntry(t, entered)
	close(gates[third])
	close(gates[fourth])
	for _, jobID := range jobByRepo {
		waitForJob(t, exec, jobID)
	}
}

func TestUnifiedPublicationDoesNotDeadlockQueueResizeAdmissionReservationAndAcquire(t *testing.T) {
	previous := config.GetIns()
	oldConfig := &config.Config{
		GitHubAPIConcurrency: 2,
		ConcurrencyNum:       1,
		Repository: []typedef.Repository{{
			Name: "old", URL: "github.com/acme/repo",
		}},
	}
	config.SetIns(oldConfig)
	t.Cleanup(func() { config.SetIns(previous) })
	exec, _ := newTestExecutorForConfig(t, oldConfig, Runners{})
	newConfig := config.Clone(oldConfig)
	newConfig.GitHubAPIConcurrency = 1
	newConfig.ConcurrencyNum = 2
	newConfig.Repository[0].Name = "new"

	publicationEntered := make(chan struct{})
	exec.queueMu.Lock()
	publishDone := make(chan struct{})
	go func() {
		config.PublishSnapshot(newConfig, func(published *config.Config) {
			close(publicationEntered)
			exec.RefreshConfig(published)
		})
		close(publishDone)
	}()
	<-publicationEntered

	runtimeDone := make(chan *RuntimeConfigSnapshot, 1)
	go func() { runtimeDone <- exec.RuntimeConfigSnapshot() }()
	executeDone := make(chan error, 1)
	go func() {
		_, executeErr := exec.ExecuteJob("github.com/acme/repo")
		executeDone <- executeErr
	}()
	acquireDone := make(chan error, 1)
	go func() {
		permit, acquireErr := githubapi.Acquire(context.Background(), "core")
		if permit != nil {
			permit.Done(githubapi.Observation{})
		}
		acquireDone <- acquireErr
	}()

	exec.queueMu.Unlock()
	select {
	case <-publishDone:
	case <-time.After(time.Second):
		t.Fatal("publication deadlocked waiting for queue resize")
	}
	select {
	case runtime := <-runtimeDone:
		require.Equal(t, "new", runtime.Repositories()[0].Name)
	case <-time.After(time.Second):
		t.Fatal("runtime read deadlocked with publication and queue resize")
	}
	select {
	case err := <-executeDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("reservation deadlocked with publication and queue resize")
	}
	select {
	case err := <-acquireDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("GitHub Acquire deadlocked with publication")
	}
}

func TestConcurrentCloseCallersWaitForSameCompletion(t *testing.T) {
	runnerEntered := make(chan struct{})
	runnerCancelled := make(chan struct{})
	releaseRunner := make(chan struct{})
	runners := noOpRunners()
	runners.Code = func(ctx context.Context, _ typedef.Repository, _ []typedef.MultiStorage) error {
		close(runnerEntered)
		<-ctx.Done()
		close(runnerCancelled)
		<-releaseRunner
		return ctx.Err()
	}
	exec, _ := newTestExecutorForConfig(t, repositoryConfig(1, 1), runners)
	_, err := exec.ExecuteJob("github.com/test/repo-0")
	require.NoError(t, err)
	<-runnerEntered

	results := make(chan error, 2)
	go func() { results <- exec.Close() }()
	<-runnerCancelled
	go func() { results <- exec.Close() }()
	<-executorCloseWaiterSignal(exec)
	select {
	case err := <-results:
		t.Fatalf("Close returned before worker completion: %v", err)
	default:
	}
	close(releaseRunner)
	require.NoError(t, <-results)
	require.NoError(t, <-results)
}

func TestClosePreventsDispatcherAdmissionWhileRunningWorkerFinishes(t *testing.T) {
	runnerEntered := make(chan string, 2)
	releaseFirst := make(chan struct{})
	runners := noOpRunners()
	runners.Code = func(_ context.Context, repo typedef.Repository, _ []typedef.MultiStorage) error {
		runnerEntered <- repo.Key()
		if repo.Key() == "github.com/test/repo-0" {
			<-releaseFirst
		}
		return nil
	}
	exec, testDB := newTestExecutorForConfig(t, repositoryConfig(1, 2), runners)
	firstIDs, err := exec.ExecuteJob("github.com/test/repo-0")
	require.NoError(t, err)
	require.Equal(t, "github.com/test/repo-0", requireRunnerEntry(t, runnerEntered))
	secondIDs, err := exec.ExecuteJob("github.com/test/repo-1")
	require.NoError(t, err)

	finishRelease := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(finishRelease) }) })
	store := &blockingExecutionStore{
		delegate:          testDB,
		finishExecutionID: firstIDs[0],
		finishEntered:     make(chan struct{}),
		finishRelease:     finishRelease,
	}
	exec.db = store
	close(releaseFirst)
	<-store.finishEntered

	closeResult := make(chan error, 1)
	go func() { closeResult <- exec.Close() }()
	<-executorClosedSignal(exec)
	select {
	case repoKey := <-runnerEntered:
		t.Fatalf("queued repository entered runner after Close won: %s", repoKey)
	default:
	}
	releaseOnce.Do(func() { close(finishRelease) })
	require.NoError(t, <-closeResult)
	require.Equal(t, map[string]int{"cancelled": 1, "completed": 1}, executionStatusCounts(t, testDB))
	for _, jobID := range append(firstIDs, secondIDs...) {
		require.False(t, exec.IsJobRunning(jobID))
	}
	select {
	case repoKey := <-runnerEntered:
		t.Fatalf("queued repository entered runner during shutdown: %s", repoKey)
	default:
	}
}

func TestExecuteJobCloseAfterCommitBeforePublishDiscardsBatch(t *testing.T) {
	runnerEntered := make(chan struct{}, 1)
	runners := noOpRunners()
	runners.Code = func(context.Context, typedef.Repository, []typedef.MultiStorage) error {
		runnerEntered <- struct{}{}
		return nil
	}
	exec, testDB := newTestExecutorForConfig(t, repositoryConfig(1, 1), runners)
	_, err := testDB.Exec(`
		CREATE TRIGGER reject_component_terminal_update
		BEFORE UPDATE OF status ON execution_components
		WHEN NEW.status IN ('cancelled', 'failed', 'completed', 'skipped')
		BEGIN
			SELECT RAISE(FAIL, 'forced component terminal update failure');
		END;`)
	require.NoError(t, err)
	componentsRelease := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(componentsRelease) }) })
	store := &blockingExecutionStore{
		delegate:          testDB,
		componentsEntered: make(chan struct{}),
		componentsRelease: componentsRelease,
	}
	exec.db = store
	type executeResult struct {
		ids []string
		err error
	}
	submitResult := make(chan executeResult, 1)
	go func() {
		ids, submitErr := exec.ExecuteJob("github.com/test/repo-0")
		submitResult <- executeResult{ids: ids, err: submitErr}
	}()
	<-store.componentsEntered

	closed := executorClosedSignal(exec)
	closeResult := make(chan error, 1)
	go func() { closeResult <- exec.Close() }()
	<-closed
	select {
	case <-runnerEntered:
		t.Fatal("runner started before persisted submission was published")
	default:
	}
	releaseOnce.Do(func() { close(componentsRelease) })

	result := <-submitResult
	require.ErrorIs(t, result.err, ErrExecutorClosed)
	require.Empty(t, result.ids)
	require.NoError(t, <-closeResult)
	var executionCount, componentCount int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM executions`).Scan(&executionCount))
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM execution_components`).Scan(&componentCount))
	require.Zero(t, executionCount)
	require.Zero(t, componentCount)
	exec.queueMu.Lock()
	require.Empty(t, exec.activeByRepo)
	require.Zero(t, exec.inflight)
	exec.queueMu.Unlock()
	select {
	case <-runnerEntered:
		t.Fatal("runner started after Close won the publish race")
	default:
	}

	_, err = testDB.Exec(`DROP TRIGGER reject_component_terminal_update`)
	require.NoError(t, err)
	fresh := NewExecutorWithRunners(nil, testDB, repositoryConfig(1, 1), runners)
	t.Cleanup(func() { require.NoError(t, fresh.Close()) })
	retryIDs, err := fresh.ExecuteJob("github.com/test/repo-0")
	require.NoError(t, err)
	require.Len(t, retryIDs, 1)
	select {
	case <-runnerEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("fresh executor did not start the discarded repository")
	}
}

func TestExecuteJobDiscardFailureReachesSubmitterAndClose(t *testing.T) {
	testDB, err := db.Initialize(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testDB.Close()) })
	runnerEntered := make(chan struct{}, 1)
	runners := noOpRunners()
	runners.Code = func(context.Context, typedef.Repository, []typedef.MultiStorage) error {
		runnerEntered <- struct{}{}
		return nil
	}
	exec := NewExecutorWithRunners(nil, testDB, repositoryConfig(1, 1), runners)
	componentsRelease := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(componentsRelease) }) })
	discardErr := errors.New("forced pending batch discard failure")
	store := &discardFailingStore{
		blockingExecutionStore: &blockingExecutionStore{
			delegate:          testDB,
			componentsEntered: make(chan struct{}),
			componentsRelease: componentsRelease,
		},
		err: discardErr,
	}
	exec.db = store
	type executeResult struct {
		ids []string
		err error
	}
	submitResult := make(chan executeResult, 1)
	go func() {
		ids, submitErr := exec.ExecuteJob("github.com/test/repo-0")
		submitResult <- executeResult{ids: ids, err: submitErr}
	}()
	<-store.componentsEntered

	closeResult := make(chan error, 1)
	go func() { closeResult <- exec.Close() }()
	<-executorClosedSignal(exec)
	releaseOnce.Do(func() { close(componentsRelease) })

	result := <-submitResult
	require.Empty(t, result.ids)
	require.ErrorIs(t, result.err, ErrExecutorClosed)
	require.ErrorIs(t, result.err, discardErr)
	require.ErrorIs(t, <-closeResult, discardErr)
	exec.queueMu.Lock()
	require.Empty(t, exec.activeByRepo)
	require.Zero(t, exec.inflight)
	exec.queueMu.Unlock()
	select {
	case <-runnerEntered:
		t.Fatal("runner started after Close won with a discard failure")
	default:
	}
}

func TestConcurrencyLimitClampsMaxUintToMaxInt(t *testing.T) {
	want := int(^uint(0) >> 1)
	require.Equal(t, want, concurrencyLimit(&config.Config{ConcurrencyNum: ^uint(0)}))
}

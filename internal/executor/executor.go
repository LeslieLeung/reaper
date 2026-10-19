package executor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/wnarutou/gitrieve/internal/config"
	"github.com/wnarutou/gitrieve/internal/db"
	"github.com/wnarutou/gitrieve/internal/githubapi"
	"github.com/wnarutou/gitrieve/internal/logger"
	"github.com/wnarutou/gitrieve/internal/maintenance"
	"github.com/wnarutou/gitrieve/internal/repository"
	"github.com/wnarutou/gitrieve/internal/syncresult"
	"github.com/wnarutou/gitrieve/internal/typedef"
	"github.com/wnarutou/gitrieve/internal/ui"
)

type ExecutionStatus string

const (
	StatusPending   ExecutionStatus = "pending"
	StatusRunning   ExecutionStatus = "running"
	StatusCompleted ExecutionStatus = "completed"
	StatusFailed    ExecutionStatus = "failed"
	StatusCancelled ExecutionStatus = "cancelled"
)

type JobContext struct {
	Ctx         context.Context
	CancelFunc  context.CancelFunc
	done        chan struct{}
	repoKey     string
	cancelled   bool
	maintenance *maintenance.Signal
}

type queuedJob struct {
	id       string
	repo     typedef.Repository
	storages []typedef.MultiStorage
	ctx      context.Context
	plan     []component
}

type preparedJob struct {
	queued     *queuedJob
	jobContext *JobContext
}

// EligibilityCheck runs after the Executor has reserved all concrete
// repositories but before any pending execution is persisted or published.
type EligibilityCheck func(context.Context, *RuntimeConfigSnapshot, typedef.Repository) (bool, error)

// RepositoryNoLongerEligibleError distinguishes a completed eligibility
// change from admission/storage failures for bulk response accounting.
type RepositoryNoLongerEligibleError struct {
	RepositoryKey string
}

// RepositoryExpansionEmptyError is an enqueue failure: a configured user/org
// expanded successfully but currently contains no concrete repositories.
type RepositoryExpansionEmptyError struct {
	RepositoryKey string
}

func (e *RepositoryExpansionEmptyError) Error() string {
	return fmt.Sprintf("repository %q expansion returned no concrete repositories", e.RepositoryKey)
}

func (e *RepositoryNoLongerEligibleError) Error() string {
	return fmt.Sprintf("repository %q is no longer eligible", e.RepositoryKey)
}

type executionStore interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
	ActiveExecutionExists(context.Context, string) (bool, error)
	CreatePendingExecutions(context.Context, []db.PendingExecution) error
	DiscardPendingExecutions(context.Context, []string) error
	StartComponent(context.Context, string, db.ComponentName, time.Time) error
	FinishComponent(context.Context, string, db.ComponentName, db.ComponentStatus, time.Time, string) error
}

type Executor struct {
	db             executionStore
	runtime        atomic.Pointer[RuntimeConfigSnapshot]
	nextGeneration atomic.Uint64
	runners        Runners
	expander       func(typedef.Repository) []typedef.Repository

	queueMu           sync.Mutex
	queueCond         *sync.Cond
	pending           []*queuedJob
	jobs              map[string]*JobContext
	activeByRepo      map[string]string
	running           int
	limit             int
	inflight          int
	closed            bool
	maintenance       bool
	shuttingDown      bool
	maintenanceErr    error
	settling          int
	preparations      map[uint64]context.CancelFunc
	nextPreparation   uint64
	dispatcherStarted bool
	dispatcherDone    chan struct{}
	closeDone         chan struct{}
	closeErr          error
	closeWaiters      int
	workers           sync.WaitGroup
}

// RuntimeConfigSnapshot is an immutable Executor configuration generation.
// Callers can inspect cloned repository values but cannot mutate the published
// configuration or its normalized-key index.
type RuntimeConfigSnapshot struct {
	config          *config.Config
	executionConfig *config.ExecutionSnapshot
	githubAPIScope  *githubapi.Scope
	generation      uint64
	repositories    map[string]typedef.Repository
}

func buildRuntimeConfigSnapshot(cfg *config.Config) *RuntimeConfigSnapshot {
	cloned := cloneRuntimeConfig(cfg)
	indexed := make(map[string]typedef.Repository)
	if cloned != nil {
		indexed = make(map[string]typedef.Repository, len(cloned.Repository))
		for _, repository := range cloned.Repository {
			if key := repository.Key(); key != "" {
				indexed[key] = repository
			}
		}
	}
	return &RuntimeConfigSnapshot{
		config:          cloned,
		executionConfig: config.NewExecutionSnapshot(cloned),
		githubAPIScope:  githubapi.NewScope(config.GitHubAPIConfigFrom(cloned)),
		repositories:    indexed,
	}
}

func cloneRuntimeConfig(cfg *config.Config) *config.Config {
	if cfg == nil {
		return nil
	}
	cloned := *cfg
	cloned.Repository = make([]typedef.Repository, len(cfg.Repository))
	for i, repository := range cfg.Repository {
		cloned.Repository[i] = cloneRuntimeRepository(repository)
	}
	cloned.Storage = append([]typedef.MultiStorage(nil), cfg.Storage...)
	return &cloned
}

func cloneRuntimeRepository(repository typedef.Repository) typedef.Repository {
	repository.Storage = append([]string(nil), repository.Storage...)
	return repository
}

func (s *RuntimeConfigSnapshot) Generation() uint64 {
	if s == nil {
		return 0
	}
	return s.generation
}

func (s *RuntimeConfigSnapshot) Repository(repositoryKey string) (typedef.Repository, bool) {
	if s == nil {
		return typedef.Repository{}, false
	}
	repository, found := s.repositories[typedef.NormalizeURL(repositoryKey)]
	if !found {
		return typedef.Repository{}, false
	}
	return cloneRuntimeRepository(repository), true
}

func (s *RuntimeConfigSnapshot) Repositories() []typedef.Repository {
	if s == nil || s.config == nil {
		return nil
	}
	repositories := make([]typedef.Repository, len(s.config.Repository))
	for i, repository := range s.config.Repository {
		repositories[i] = cloneRuntimeRepository(repository)
	}
	return repositories
}

// Config returns a defensive copy of this immutable runtime generation.
func (s *RuntimeConfigSnapshot) Config() *config.Config {
	if s == nil {
		return nil
	}
	return cloneRuntimeConfig(s.config)
}

func (s *RuntimeConfigSnapshot) SyncHealthThresholds() (time.Duration, time.Duration) {
	if s == nil || s.config == nil {
		return 0, 0
	}
	return s.config.SyncOverdueGrace, s.config.SyncStuckThreshold
}

func (s *RuntimeConfigSnapshot) executionContext(parent context.Context) context.Context {
	if parent == nil {
		parent = context.Background()
	}
	ctx := config.WithExecutionSnapshot(parent, s.executionConfig)
	return githubapi.WithScope(ctx, s.githubAPIScope)
}

func NewExecutor(logger *logger.Logger, db *db.DB, cfg *config.Config) *Executor {
	return NewExecutorWithRunners(logger, db, cfg, defaultRunners())
}

func NewExecutorWithRunners(logger *logger.Logger, db *db.DB, cfg *config.Config, runners Runners, expanders ...func(typedef.Repository) []typedef.Repository) *Executor {
	// Side effect: re-points the package-global ui sink so executor log output
	// is persisted to the DB. Only the server process constructs an Executor;
	// the CLI and daemon never do, so their stdout-only ui output is unchanged.
	if logger != nil {
		ui.SetSink(logger)
	}
	exec := &Executor{
		db:             db,
		runners:        runners,
		jobs:           make(map[string]*JobContext),
		activeByRepo:   make(map[string]string),
		limit:          concurrencyLimit(cfg),
		dispatcherDone: make(chan struct{}),
		closeDone:      make(chan struct{}),
	}
	if len(expanders) > 0 {
		exec.expander = expanders[0]
	}
	exec.queueCond = sync.NewCond(&exec.queueMu)
	runtime := buildRuntimeConfigSnapshot(cfg)
	runtime.generation = exec.nextGeneration.Add(1)
	exec.runtime.Store(runtime)
	return exec
}

func concurrencyLimit(cfg *config.Config) int {
	if cfg == nil || cfg.ConcurrencyNum == 0 {
		return 3
	}
	maxInt := int(^uint(0) >> 1)
	if cfg.ConcurrencyNum > uint(maxInt) {
		return maxInt
	}
	return int(cfg.ConcurrencyNum)
}

// RefreshConfig defensively clones and indexes cfg before atomically publishing
// one immutable runtime generation.
func (e *Executor) RefreshConfig(cfg *config.Config) {
	runtime := buildRuntimeConfigSnapshot(cfg)
	e.queueMu.Lock()
	runtime.generation = e.nextGeneration.Add(1)
	e.limit = concurrencyLimit(runtime.config)
	e.runtime.Store(runtime)
	e.queueCond.Broadcast()
	e.queueMu.Unlock()
}

func (e *Executor) RuntimeConfigSnapshot() *RuntimeConfigSnapshot {
	var runtime *RuntimeConfigSnapshot
	githubapi.ReadPublication(func() {
		runtime = e.runtime.Load()
	})
	return runtime
}

// ErrRepositoryNotFound 表示配置中找不到匹配该身份键的仓库条目。
var ErrRepositoryNotFound = errors.New("repository not found in configuration")
var ErrRepositoryActive = errors.New("repository is already active")
var ErrExecutorClosed = errors.New("executor is closed")
var ErrConfigGenerationChanged = errors.New("executor configuration generation changed")

// expandRepos 是把 user/org 条目展开为具体仓库的 seam：生产用 repository.Expand，
// 测试注入 fake，避免真实 GitHub 调用。
var expandRepos = repository.ExpandContext

// ExecuteJob 按仓库身份键（规范化 URL）在配置中定位条目并执行。type=repo 产生
// 一条 execution 并返回单元素 jobID；type=user/org 先在任务内展开为具体仓库，
// 每个具体仓库独立执行（各自 jobID / execution / 日志流 / 可取消）。
func (e *Executor) ExecuteJob(repoKey string) ([]string, error) {
	return e.executeJobFromSnapshot(context.Background(), e.RuntimeConfigSnapshot(), repoKey, nil, nil)
}

func (e *Executor) ExecuteJobAtGeneration(repoKey string, generation uint64) ([]string, error) {
	return e.ExecuteJobAtGenerationContext(context.Background(), repoKey, generation)
}

// ExecuteJobAtGenerationContext admits a generation-bound job using ctx for
// preflight and persistence. Once published, the job owns an independent
// cancellation context and is not tied to the HTTP request lifetime.
func (e *Executor) ExecuteJobAtGenerationContext(ctx context.Context, repoKey string, generation uint64) ([]string, error) {
	runtime := e.RuntimeConfigSnapshot()
	if runtime == nil || runtime.generation != generation {
		return nil, ErrConfigGenerationChanged
	}
	return e.executeJobFromSnapshot(ctx, runtime, repoKey, &generation, nil)
}

// ExecuteJobAtGenerationIfEligibleContext is the bulk admission path. It
// reserves first, evaluates eligible while ownership is held, then persists
// and publishes only when the callback still accepts the repository.
func (e *Executor) ExecuteJobAtGenerationIfEligibleContext(ctx context.Context, repoKey string, generation uint64, eligible EligibilityCheck) ([]string, error) {
	runtime := e.RuntimeConfigSnapshot()
	if runtime == nil || runtime.generation != generation {
		return nil, ErrConfigGenerationChanged
	}
	return e.executeJobFromSnapshot(ctx, runtime, repoKey, &generation, eligible)
}

func (e *Executor) executeJobFromSnapshot(admissionCtx context.Context, runtime *RuntimeConfigSnapshot, repoKey string, expectedGeneration *uint64, eligible EligibilityCheck) (ids []string, err error) {
	parent := admissionCtx
	var finish func()
	admissionCtx, finish, err = e.beginPreparation(admissionCtx)
	if err != nil {
		return nil, err
	}
	defer finish()
	defer func() {
		if err == nil || admissionCtx.Err() == nil || parent.Err() != nil {
			return
		}
		e.queueMu.Lock()
		defer e.queueMu.Unlock()
		if e.closed {
			err = errors.Join(ErrExecutorClosed, err)
		} else if e.maintenance {
			err = errors.Join(ErrMaintenance, err)
		}
	}()
	repo, found := runtime.Repository(repoKey)
	if !found {
		return nil, ErrRepositoryNotFound
	}

	if e.expander != nil {
		repositories := e.expander(repo)
		if len(repositories) == 0 {
			return nil, &RepositoryExpansionEmptyError{RepositoryKey: repo.Key()}
		}
		return e.submitBatch(admissionCtx, runtime, repo, repositories, expectedGeneration, eligible)
	}
	repositories, err := expandRepos(runtime.executionContext(admissionCtx), repo)
	if err != nil {
		return nil, err
	}
	if len(repositories) == 0 {
		return nil, &RepositoryExpansionEmptyError{RepositoryKey: repo.Key()}
	}
	return e.submitBatch(admissionCtx, runtime, repo, repositories, expectedGeneration, eligible)
}

// submitBatch reserves, preflights, and persists every concrete repository
// before atomically publishing any of them to the dispatcher.
func (e *Executor) submitBatch(admissionCtx context.Context, runtime *RuntimeConfigSnapshot, configured typedef.Repository, repositories []typedef.Repository, expectedGeneration *uint64, eligible EligibilityCheck) ([]string, error) {
	prepared := make([]*preparedJob, len(repositories))
	for i, repo := range repositories {
		signal := maintenance.NewSignal()
		ctx, cancel := context.WithCancel(maintenance.WithSignal(runtime.executionContext(context.Background()), signal))
		jobID := uuid.New().String()
		plan := componentPlan(repo, e.runners)
		prepared[i] = &preparedJob{
			queued: &queuedJob{
				id:       jobID,
				repo:     repo,
				storages: runtime.resolveStorages(repo.Storage),
				ctx:      ctx,
				plan:     plan,
			},
			jobContext: &JobContext{
				Ctx:         ctx,
				maintenance: signal,
				CancelFunc:  cancel,
				done:        make(chan struct{}),
				repoKey:     repo.Key(),
			},
		}
	}

	if err := e.reserveBatch(admissionCtx, prepared, expectedGeneration); err != nil {
		for _, job := range prepared {
			job.jobContext.CancelFunc()
		}
		return nil, err
	}
	if err := e.preflightBatch(admissionCtx, prepared); err != nil {
		e.releaseBatch(prepared, nil)
		return nil, err
	}
	if eligible != nil {
		stillEligible, err := eligible(admissionCtx, runtime, cloneRuntimeRepository(configured))
		if err == nil {
			err = admissionCtx.Err()
		}
		if err != nil {
			e.releaseBatch(prepared, nil)
			return nil, err
		}
		if !stillEligible {
			e.releaseBatch(prepared, nil)
			return nil, &RepositoryNoLongerEligibleError{RepositoryKey: configured.Key()}
		}
	}
	if err := e.persistBatch(admissionCtx, prepared); err != nil {
		e.releaseBatch(prepared, nil)
		return nil, err
	}

	e.queueMu.Lock()
	if err := admissionCtx.Err(); err != nil {
		e.queueMu.Unlock()
		discardErr := e.discardBatch(prepared)
		e.releaseBatch(prepared, discardErr)
		if discardErr != nil {
			return nil, errors.Join(err, discardErr)
		}
		return nil, err
	}
	if e.closed || e.maintenance {
		admissionErr := ErrExecutorClosed
		if e.maintenance {
			admissionErr = ErrMaintenance
		}
		e.queueMu.Unlock()
		discardErr := e.discardBatch(prepared)
		e.releaseBatch(prepared, discardErr)
		if discardErr != nil {
			return nil, errors.Join(admissionErr, discardErr)
		}
		return nil, admissionErr
	}
	jobIDs := make([]string, len(prepared))
	for i, job := range prepared {
		jobIDs[i] = job.queued.id
		e.jobs[job.queued.id] = job.jobContext
		e.pending = append(e.pending, job.queued)
	}
	e.inflight--
	if len(prepared) != 0 {
		e.startDispatcherLocked()
	}
	e.queueCond.Broadcast()
	e.queueMu.Unlock()
	return jobIDs, nil
}

func (s *RuntimeConfigSnapshot) resolveStorages(names []string) []typedef.MultiStorage {
	if s == nil || s.config == nil || len(names) == 0 {
		return nil
	}
	storages := make([]typedef.MultiStorage, 0, len(names))
	for _, name := range names {
		for _, storage := range s.config.Storage {
			if storage.Name == name {
				storages = append(storages, storage)
				break
			}
		}
	}
	return storages
}

func (e *Executor) reserveBatch(ctx context.Context, prepared []*preparedJob, expectedGeneration *uint64) error {
	var result error
	githubapi.ReadPublication(func() {
		result = e.reserveBatchWithinPublication(ctx, prepared, expectedGeneration)
	})
	return result
}

// reserveBatchWithinPublication takes queueMu only after the publication read
// gate. The unified writer uses the same order (publication write gate, then
// queueMu), preventing the queue/publication inversion while keeping the gate
// away from database and network work.
func (e *Executor) reserveBatchWithinPublication(ctx context.Context, prepared []*preparedJob, expectedGeneration *uint64) error {
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if expectedGeneration != nil {
		runtime := e.runtime.Load()
		if runtime == nil || runtime.generation != *expectedGeneration {
			return ErrConfigGenerationChanged
		}
	}
	if e.closed {
		return ErrExecutorClosed
	}
	if e.maintenance {
		return ErrMaintenance
	}
	seen := make(map[string]struct{}, len(prepared))
	for _, job := range prepared {
		repoKey := job.jobContext.repoKey
		if _, duplicate := seen[repoKey]; duplicate {
			return ErrRepositoryActive
		}
		seen[repoKey] = struct{}{}
		if _, active := e.activeByRepo[repoKey]; active {
			return ErrRepositoryActive
		}
	}
	for _, job := range prepared {
		e.activeByRepo[job.jobContext.repoKey] = job.queued.id
	}
	e.inflight++
	return nil
}

func (e *Executor) preflightBatch(ctx context.Context, prepared []*preparedJob) error {
	for _, job := range prepared {
		active, err := e.db.ActiveExecutionExists(ctx, job.jobContext.repoKey)
		if err != nil {
			return fmt.Errorf("check active execution: %w", err)
		}
		if active {
			return ErrRepositoryActive
		}
	}
	return nil
}

func (e *Executor) persistBatch(ctx context.Context, prepared []*preparedJob) error {
	executions := make([]db.PendingExecution, len(prepared))
	for i, job := range prepared {
		componentNames := make([]db.ComponentName, len(job.queued.plan))
		for componentIndex := range job.queued.plan {
			componentNames[componentIndex] = job.queued.plan[componentIndex].name
		}
		executions[i] = db.PendingExecution{
			ID:         job.queued.id,
			JobName:    job.queued.repo.Name,
			RepoKey:    job.jobContext.repoKey,
			StartTime:  time.Now(),
			Components: componentNames,
		}
	}
	if err := e.db.CreatePendingExecutions(ctx, executions); err != nil {
		return fmt.Errorf("create pending execution batch: %w", err)
	}
	return nil
}

func (e *Executor) discardBatch(prepared []*preparedJob) error {
	executionIDs := make([]string, len(prepared))
	for i, job := range prepared {
		executionIDs[i] = job.queued.id
	}
	if err := e.db.DiscardPendingExecutions(context.Background(), executionIDs); err != nil {
		return fmt.Errorf("discard unaccepted execution batch: %w", err)
	}
	return nil
}

func (e *Executor) releaseBatch(prepared []*preparedJob, closeErr error) {
	e.queueMu.Lock()
	if closeErr != nil {
		e.closeErr = errors.Join(e.closeErr, closeErr)
		if e.maintenance {
			e.maintenanceErr = errors.Join(e.maintenanceErr, closeErr)
		}
	}
	for _, job := range prepared {
		job.jobContext.CancelFunc()
		if e.activeByRepo[job.jobContext.repoKey] == job.queued.id {
			delete(e.activeByRepo, job.jobContext.repoKey)
		}
	}
	e.inflight--
	e.queueCond.Broadcast()
	e.queueMu.Unlock()
}

func (e *Executor) startDispatcherLocked() {
	if e.dispatcherStarted {
		return
	}
	e.dispatcherStarted = true
	go e.dispatch()
}

func (e *Executor) dispatch() {
	defer close(e.dispatcherDone)
	for {
		e.queueMu.Lock()
		for !e.closed && (e.maintenance || len(e.pending) == 0 || e.running >= e.limit) {
			e.queueCond.Wait()
		}
		if e.closed {
			e.queueMu.Unlock()
			return
		}

		job := e.pending[0]
		e.pending[0] = nil
		e.pending = e.pending[1:]
		e.running++
		e.workers.Add(1)
		e.queueMu.Unlock()

		go e.runAdmitted(job)
	}
}

func (e *Executor) runAdmitted(job *queuedJob) {
	defer e.workers.Done()
	defer e.completeRunningJob(job.id, job.repo.Key())

	if err := e.updateJobStatus(job.id, string(StatusRunning), ""); err != nil {
		e.finishAdmissionFailure(job, err)
		return
	}
	e.executeAsync(job.ctx, job.id, job.repo, job.storages, job.plan)
}

func (e *Executor) finishAdmissionFailure(job *queuedJob, admissionErr error) {
	message := fmt.Sprintf("persist running execution state: %v", admissionErr)
	allTerminal := true
	for _, planned := range job.plan {
		if err := e.db.FinishComponent(context.Background(), job.id, planned.name, db.ComponentFailed, time.Now(), message); err != nil {
			allTerminal = false
			ui.Errorf("Failed to record %s admission failure: %v", planned.name, err)
		}
	}
	if allTerminal {
		e.finishExecution(job.id, StatusFailed, message)
	} else {
		e.recordMaintenanceError(errors.New(message))
	}
}

func (e *Executor) completeRunningJob(jobID, repoKey string) {
	e.queueMu.Lock()
	jobCtx := e.jobs[jobID]
	delete(e.jobs, jobID)
	if e.activeByRepo[repoKey] == jobID {
		delete(e.activeByRepo, repoKey)
	}
	e.running--
	if jobCtx != nil {
		jobCtx.CancelFunc()
		close(jobCtx.done)
	}
	e.queueCond.Broadcast()
	e.queueMu.Unlock()
}

func (e *Executor) executeAsync(ctx context.Context, jobID string, job typedef.Repository, storages []typedef.MultiStorage, plan []component) {
	unbind := ui.Bind(jobID, job.Name)
	defer unbind()

	ui.Printf("Starting job execution")

	// Check if context is already cancelled
	if ctx.Err() != nil {
		// Write the final log line BEFORE the terminal status update so the SSE
		// log stream (which emits "done" as soon as it sees a terminal status)
		// does not flush before this row is committed and drop it.
		e.finishCancellation(jobID, plan, 0, ctx.Err())
		return
	}

	failures := make([]string, 0)
	allComponentsTerminal := true
	for i, planned := range plan {
		if maintenance.Requested(ctx) && ctx.Err() == nil {
			e.finishMaintenance(jobID, plan, i, failures, allComponentsTerminal)
			return
		}
		if ctx.Err() != nil {
			e.finishCancellation(jobID, plan, i, ctx.Err())
			return
		}

		if err := e.db.StartComponent(context.Background(), jobID, planned.name, time.Now()); err != nil {
			message := fmt.Sprintf("persist running state: %v", err)
			failures = append(failures, componentFailure(planned.name, message))
			ui.Errorf("Failed to start %s: %v", planned.name, err)
			if fallbackErr := e.finishAfterStoreFailure(jobID, planned.name, message); fallbackErr != nil {
				fallbackMessage := fmt.Sprintf("persist failed fallback: %v", fallbackErr)
				failures = append(failures, componentFailure(planned.name, fallbackMessage))
				ui.Errorf("Failed to record %s store failure: %v", planned.name, fallbackErr)
				allComponentsTerminal = false
			}
			continue
		}

		if planned.name != db.ComponentCode {
			ui.Printf("Downloading %s", componentLogName(planned.name))
		}
		runErr := planned.run(ctx, job, storages)
		if cancelErr := componentCancellation(ctx); cancelErr != nil {
			e.finishCancellation(jobID, plan, i, cancelErr)
			return
		}

		if errors.Is(runErr, maintenance.ErrRequested) {
			e.finishMaintenance(jobID, plan, i, failures, allComponentsTerminal)
			return
		}
		status := db.ComponentCompleted
		errorMessage := ""
		if reason, skipped := syncresult.SkippedReason(runErr); skipped {
			status = db.ComponentSkipped
			errorMessage = reason
			ui.Printf("Skipping %s: %s", componentLogName(planned.name), reason)
		} else if runErr != nil {
			status = db.ComponentFailed
			errorMessage = runErr.Error()
			failures = append(failures, componentFailure(planned.name, errorMessage))
			ui.Errorf("Failed to download %s: %v", componentLogName(planned.name), runErr)
		}

		if err := e.db.FinishComponent(context.Background(), jobID, planned.name, status, time.Now(), errorMessage); err != nil {
			message := fmt.Sprintf("persist %s state: %v", status, err)
			failures = append(failures, componentFailure(planned.name, message))
			ui.Errorf("Failed to finish %s: %v", planned.name, err)
			if fallbackErr := e.finishAfterStoreFailure(jobID, planned.name, message); fallbackErr != nil {
				fallbackMessage := fmt.Sprintf("persist failed fallback: %v", fallbackErr)
				failures = append(failures, componentFailure(planned.name, fallbackMessage))
				ui.Errorf("Failed to record %s store failure: %v", planned.name, fallbackErr)
				allComponentsTerminal = false
			}
		}
	}

	if !allComponentsTerminal {
		ui.Errorf("Job cannot be finalized while component rows remain active")
		e.recordMaintenanceError(errors.New("cannot finalize execution while component rows remain active"))
		return
	}
	if len(failures) != 0 {
		e.finishExecution(jobID, StatusFailed, strings.Join(failures, "; "))
		return
	}

	// Final log line before the terminal status update (see note above).
	ui.Printf("Job completed successfully")
	e.finishExecution(jobID, StatusCompleted, "")
}

func componentFailure(name db.ComponentName, message string) string {
	label := string(name)
	if name == db.ComponentIssue {
		label = "issue"
	}
	return fmt.Sprintf("%s: %s", label, message)
}

func componentCancellation(ctx context.Context) error {
	return ctx.Err()
}

func componentLogName(name db.ComponentName) string {
	if name == db.ComponentRelease {
		return "releases"
	}
	return string(name)
}

func (e *Executor) finishCancellation(jobID string, plan []component, first int, cancelErr error) {
	failures, allTerminal := e.cancelComponents(jobID, plan, first, cancelErr)
	ui.Printf("Job was cancelled")
	if !allTerminal {
		ui.Errorf("Job cannot be finalized while component rows remain active")
		e.recordMaintenanceError(errors.New("cannot finalize execution while component rows remain active"))
		return
	}
	if len(failures) != 0 {
		e.finishExecution(jobID, StatusFailed, strings.Join(failures, "; "))
		return
	}
	message := ""
	if errors.Is(cancelErr, maintenance.ErrRequested) {
		message = maintenance.ErrRequested.Error()
	}
	e.finishExecution(jobID, StatusCancelled, message)
}

func (e *Executor) cancelComponents(jobID string, plan []component, first int, cancelErr error) ([]string, bool) {
	failures := make([]string, 0)
	allTerminal := true
	message := cancelErr.Error()
	for _, planned := range plan[first:] {
		if err := e.db.FinishComponent(context.Background(), jobID, planned.name, db.ComponentCancelled, time.Now(), message); err != nil {
			ui.Errorf("Failed to cancel %s: %v", planned.name, err)
			persistenceMessage := fmt.Sprintf("persist cancelled state: %v", err)
			failures = append(failures, componentFailure(planned.name, persistenceMessage))
			if fallbackErr := e.finishAfterStoreFailure(jobID, planned.name, persistenceMessage); fallbackErr != nil {
				fallbackMessage := fmt.Sprintf("persist failed fallback: %v", fallbackErr)
				failures = append(failures, componentFailure(planned.name, fallbackMessage))
				ui.Errorf("Failed to record %s cancellation store failure: %v", planned.name, fallbackErr)
				allTerminal = false
			}
		}
	}
	return failures, allTerminal
}

func (e *Executor) finishAfterStoreFailure(jobID string, name db.ComponentName, message string) error {
	return e.db.FinishComponent(context.Background(), jobID, name, db.ComponentFailed, time.Now(), message)
}

func (e *Executor) finishExecution(jobID string, status ExecutionStatus, errorMessage string) {
	if err := e.updateJobStatus(jobID, string(status), errorMessage); err != nil {
		ui.Errorf("Failed to finish job as %s: %v", status, err)
		if status == StatusFailed {
			e.recordMaintenanceError(err)
			return
		}

		failureMessage := fmt.Sprintf("persist %s execution state: %v", status, err)
		if fallbackErr := e.updateJobStatus(jobID, string(StatusFailed), failureMessage); fallbackErr != nil {
			ui.Errorf("Failed to record overall execution store failure: %v", fallbackErr)
			e.recordMaintenanceError(fallbackErr)
		}
	}
}

func (e *Executor) CancelJob(jobID string) error {
	e.queueMu.Lock()
	if e.closed {
		e.queueMu.Unlock()
		return ErrExecutorClosed
	}
	jobCtx, exists := e.jobs[jobID]
	if !exists {
		// Account the persistence operation so Close cannot let the server close
		// SQLite underneath it, but do not hold queueMu during the DB call.
		e.inflight++
		e.queueMu.Unlock()
		err := e.updateJobStatus(jobID, string(StatusCancelled), "")
		e.queueMu.Lock()
		e.inflight--
		e.queueCond.Broadcast()
		e.queueMu.Unlock()
		return err
	}

	for i, pending := range e.pending {
		if pending.id != jobID {
			continue
		}
		e.pending = append(e.pending[:i], e.pending[i+1:]...)
		e.signalCancellationLocked(jobCtx)
		delete(e.jobs, jobID)
		if e.activeByRepo[jobCtx.repoKey] == jobID {
			delete(e.activeByRepo, jobCtx.repoKey)
		}
		e.workers.Add(1)
		e.settling++
		e.queueCond.Broadcast()
		e.queueMu.Unlock()

		defer e.workers.Done()
		defer func() { e.queueMu.Lock(); e.settling--; e.queueCond.Broadcast(); e.queueMu.Unlock() }()
		e.finishQueuedCancellation(pending)
		close(jobCtx.done)
		return nil
	}

	e.signalCancellationLocked(jobCtx)
	e.queueMu.Unlock()
	return nil
}

func (e *Executor) signalCancellationLocked(jobCtx *JobContext) {
	if jobCtx.cancelled {
		return
	}
	jobCtx.cancelled = true
	jobCtx.CancelFunc()
}

func (e *Executor) finishQueuedCancellation(job *queuedJob) {
	unbind := ui.Bind(job.id, job.repo.Name)
	defer unbind()
	e.finishCancellation(job.id, job.plan, 0, context.Canceled)
}

// Close stops admission, terminalizes queued jobs without invoking their
// runners, signals every running job once, and waits for all executor-owned
// goroutines and persistence work to finish.
func (e *Executor) Close() error {
	e.queueMu.Lock()
	if e.closed {
		e.closeWaiters++
		e.queueCond.Broadcast()
		done := e.closeDone
		e.queueMu.Unlock()
		<-done
		e.queueMu.Lock()
		e.closeWaiters--
		err := e.closeErr
		e.queueMu.Unlock()
		return err
	}

	e.closed = true
	for _, cancel := range e.preparations {
		cancel()
	}
	pending := e.pending
	e.pending = nil
	pendingContexts := make(map[string]*JobContext, len(pending))
	for _, job := range pending {
		jobCtx := e.jobs[job.id]
		if jobCtx == nil {
			continue
		}
		pendingContexts[job.id] = jobCtx
		e.signalCancellationLocked(jobCtx)
		delete(e.jobs, job.id)
		if e.activeByRepo[jobCtx.repoKey] == job.id {
			delete(e.activeByRepo, jobCtx.repoKey)
		}
	}
	for _, jobCtx := range e.jobs {
		e.signalCancellationLocked(jobCtx)
	}
	dispatcherStarted := e.dispatcherStarted
	e.queueCond.Broadcast()
	e.queueMu.Unlock()

	for _, job := range pending {
		e.finishQueuedCancellation(job)
		if jobCtx := pendingContexts[job.id]; jobCtx != nil {
			close(jobCtx.done)
		}
	}
	if dispatcherStarted {
		<-e.dispatcherDone
	}
	e.workers.Wait()

	e.queueMu.Lock()
	for e.inflight != 0 || len(e.preparations) != 0 {
		e.queueCond.Wait()
	}
	close(e.closeDone)
	err := e.closeErr
	e.queueMu.Unlock()
	return err
}

func (e *Executor) updateJobStatus(jobID string, status string, errorMessage string) error {
	var (
		result interface {
			RowsAffected() (int64, error)
		}
		err error
	)
	if status == string(StatusPending) || status == string(StatusRunning) {
		result, err = e.db.Exec(`
			UPDATE executions SET status = ?, error_message = ?
			WHERE id = ?
		`, status, errorMessage, jobID)
	} else {
		endTime := time.Now()
		result, err = e.db.Exec(`
			UPDATE executions SET status = ?, error_message = ?, end_time = ?
			WHERE id = ?
		`, status, errorMessage, endTime, jobID)
	}
	if err != nil {
		return fmt.Errorf("update execution %q to %q: %w", jobID, status, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update execution %q to %q: inspect affected rows: %w", jobID, status, err)
	}
	if rows != 1 {
		return fmt.Errorf("update execution %q to %q: expected one row, updated %d", jobID, status, rows)
	}
	return nil
}

func (e *Executor) IsJobRunning(jobID string) bool {
	e.queueMu.Lock()
	_, exists := e.jobs[jobID]
	e.queueMu.Unlock()
	return exists
}

package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wnarutou/gitrieve/internal/config"
	"github.com/wnarutou/gitrieve/internal/db"
	"github.com/wnarutou/gitrieve/internal/executor"
	"github.com/wnarutou/gitrieve/internal/typedef"
)

type API struct {
	configMu            sync.RWMutex
	config              *config.Config
	db                  *db.DB
	executor            *executor.Executor
	readReloadSnapshot  func() (*config.ReloadSnapshot, error)
	bulkRepositoryStats func(context.Context, typedef.Repository) (map[string]db.RepositoryRunStats, error)
	bulkExecute         func(context.Context, string, uint64, executor.EligibilityCheck) ([]string, error)
	scheduleRefresher   ScheduleRefresher

	// Test-only synchronization seam used to pause inside the unified
	// publication callback after the Executor runtime store.
	afterRuntimePublishForTest func()
}

// ScheduleRefresher updates the live server scheduler after repository or
// configuration changes.
type ScheduleRefresher interface {
	RefreshSchedules(*config.Config) error
}

func NewAPI(cfg *config.Config, db *db.DB, exec *executor.Executor) *API {
	initial := config.Clone(cfg)
	if exec != nil {
		initial = exec.RuntimeConfigSnapshot().Config()
	}
	api := &API{config: initial, db: db, executor: exec, readReloadSnapshot: config.ReadReloadSnapshot}
	api.bulkRepositoryStats = api.repositoryRunStatsForCandidate
	if exec != nil {
		api.bulkExecute = exec.ExecuteJobAtGenerationIfEligibleContext
	}
	return api
}

func (a *API) configSnapshot() *config.Config {
	a.configMu.RLock()
	defer a.configMu.RUnlock()
	if a.executor != nil {
		return a.executor.RuntimeConfigSnapshot().Config()
	}
	return config.Clone(a.config)
}

func (a *API) publishConfigLocked(next *config.Config) *config.Config {
	return config.PublishSnapshot(next, a.installPublishedConfig)
}

func (a *API) publishReloadConfigLocked(snapshot *config.ReloadSnapshot) *config.Config {
	return config.PublishReloadSnapshot(snapshot, a.installPublishedConfig)
}

func (a *API) installPublishedConfig(published *config.Config) {
	if a.executor != nil {
		a.executor.RefreshConfig(published)
	}
	a.config = config.Clone(published)
	if a.afterRuntimePublishForTest != nil {
		a.afterRuntimePublishForTest()
	}
}

func (a *API) SetScheduleRefresher(refresher ScheduleRefresher) {
	a.scheduleRefresher = refresher
}

// publishPersistAndRefreshConfigLocked completes one configuration generation
// while its caller retains configMu for the entire operation.
func (a *API) publishPersistAndRefreshConfigLocked(next *config.Config, saveFailurePrefix string) (*config.Config, string) {
	published := a.publishConfigLocked(next)
	message := ""
	if err := config.SaveSnapshot(published); err != nil {
		message = saveFailurePrefix + err.Error()
	}
	return published, joinMessages(message, a.refreshSchedules(published))
}

// publishAndRefreshConfigLocked installs one in-memory generation and updates
// schedules without writing config.yaml. Reload uses this path because the
// operator's on-disk bytes are the source of truth for that operation.
func (a *API) publishAndRefreshConfigLocked(next *config.Config) (*config.Config, string) {
	published := a.publishConfigLocked(next)
	message := ""
	return published, joinMessages(message, a.refreshSchedules(published))
}

func (a *API) refreshSchedules(cfg *config.Config) string {
	if a.scheduleRefresher == nil {
		return ""
	}
	if err := a.scheduleRefresher.RefreshSchedules(config.Clone(cfg)); err != nil {
		return "Cron schedules refreshed with errors: " + err.Error()
	}
	return ""
}

func joinMessages(messages ...string) string {
	nonEmpty := messages[:0]
	for _, message := range messages {
		if message != "" {
			nonEmpty = append(nonEmpty, message)
		}
	}
	return strings.Join(nonEmpty, "; ")
}

func (a *API) CreateJob(c *gin.Context) {
	var req CreateJobRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, Response{
			Code:    400,
			Message: "Invalid request: " + err.Error(),
		})
		return
	}

	jobIDs, err := a.executor.ExecuteJob(req.RepositoryKey)
	if err != nil {
		if errors.Is(err, executor.ErrRepositoryActive) {
			c.JSON(http.StatusConflict, Response{
				Code:    http.StatusConflict,
				Message: "Repository is already active",
			})
			return
		}
		if errors.Is(err, executor.ErrRepositoryNotFound) {
			c.JSON(http.StatusNotFound, Response{
				Code:    404,
				Message: "Repository not found in configuration",
			})
			return
		}
		c.JSON(http.StatusInternalServerError, Response{
			Code:    500,
			Message: "Failed to execute job: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, Response{
		Code: 200,
		Data: CreateJobResponse{
			JobIDs: jobIDs,
			Status: string(executor.StatusPending),
		},
	})
}

// BulkCreateJobs retries the current server-side eligible set selected by the
// request. It never accepts repository IDs, and each confirmed candidate is
// independently rechecked and submitted through the shared Executor.
func (a *API) BulkCreateJobs(c *gin.Context) {
	req, err := decodeBulkCreateJobsRequest(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, Response{Code: http.StatusBadRequest, Message: "Invalid request: " + err.Error()})
		return
	}

	if a.executor == nil {
		c.JSON(http.StatusInternalServerError, Response{Code: http.StatusInternalServerError, Message: "Executor is unavailable"})
		return
	}
	runtime := a.executor.RuntimeConfigSnapshot()
	confirmed, err := a.bulkEligibleSnapshot(c.Request.Context(), runtime, req.Selector, time.Now())
	if err != nil {
		c.JSON(http.StatusInternalServerError, Response{Code: http.StatusInternalServerError, Message: "Failed to query repository stats: " + err.Error()})
		return
	}
	if len(confirmed) != req.ExpectedCount {
		c.JSON(http.StatusConflict, Response{
			Code:    http.StatusConflict,
			Data:    gin.H{"actual_count": len(confirmed)},
			Message: "Eligible repository count changed",
		})
		return
	}

	result := BulkCreateJobsResponse{Requested: len(confirmed)}
	ctx := c.Request.Context()
bulkLoop:
	for index, candidate := range confirmed {
		remaining := len(confirmed) - index
		if ctx.Err() != nil {
			result.FailedToEnqueue += remaining
			break
		}
		runtime = a.executor.RuntimeConfigSnapshot()
		eligible, recheckErr := a.bulkRepositoryEligible(ctx, runtime, candidate.Key(), req.Selector, time.Now())
		if recheckErr != nil {
			if ctx.Err() != nil || errors.Is(recheckErr, context.Canceled) || errors.Is(recheckErr, context.DeadlineExceeded) {
				result.FailedToEnqueue += remaining
				break
			}
			result.FailedToEnqueue++
			continue
		}
		if !eligible {
			result.NoLongerEligible++
			continue
		}
		if ctx.Err() != nil {
			result.FailedToEnqueue += remaining
			break
		}

		_, executeErr := a.bulkExecute(ctx, candidate.Key(), runtime.Generation(),
			func(checkCtx context.Context, admitted *executor.RuntimeConfigSnapshot, repository typedef.Repository) (bool, error) {
				return a.bulkRepositoryEligible(checkCtx, admitted, repository.Key(), req.Selector, time.Now())
			})
		var noLongerEligible *executor.RepositoryNoLongerEligibleError
		switch {
		case executeErr == nil:
			result.Queued++
		case ctx.Err() != nil || errors.Is(executeErr, context.Canceled) || errors.Is(executeErr, context.DeadlineExceeded):
			result.FailedToEnqueue += remaining
			break bulkLoop
		case errors.Is(executeErr, executor.ErrRepositoryActive):
			result.SkippedActive++
		case errors.Is(executeErr, executor.ErrRepositoryNotFound):
			result.NoLongerEligible++
		case errors.Is(executeErr, executor.ErrConfigGenerationChanged):
			result.NoLongerEligible++
		case errors.As(executeErr, &noLongerEligible):
			result.NoLongerEligible++
		default:
			result.FailedToEnqueue++
		}
	}

	c.JSON(http.StatusOK, Response{Code: http.StatusOK, Data: result})
}

func decodeBulkCreateJobsRequest(body io.Reader) (BulkCreateJobsRequest, error) {
	var envelope struct {
		Selector      json.RawMessage `json:"selector"`
		ExpectedCount json.RawMessage `json:"expected_count"`
	}
	if err := decodeStrictJSON(body, &envelope); err != nil {
		return BulkCreateJobsRequest{}, err
	}
	if len(envelope.Selector) == 0 || string(envelope.Selector) == "null" {
		return BulkCreateJobsRequest{}, fmt.Errorf("selector is required")
	}
	if len(envelope.ExpectedCount) == 0 || string(envelope.ExpectedCount) == "null" {
		return BulkCreateJobsRequest{}, fmt.Errorf("expected_count is required")
	}

	var req BulkCreateJobsRequest
	if err := decodeStrictJSON(strings.NewReader(string(envelope.Selector)), &req.Selector); err != nil {
		return BulkCreateJobsRequest{}, fmt.Errorf("selector: %w", err)
	}
	var selectorFields map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Selector, &selectorFields); err != nil {
		return BulkCreateJobsRequest{}, fmt.Errorf("selector: %w", err)
	}
	for _, field := range []string{"search", "health", "overdue"} {
		if raw, present := selectorFields[field]; present && string(raw) == "null" {
			return BulkCreateJobsRequest{}, fmt.Errorf("selector.%s must not be null", field)
		}
	}
	if req.Selector.Health != "" && !validRepositoryHealth(req.Selector.Health) {
		return BulkCreateJobsRequest{}, fmt.Errorf("health %q is not supported", req.Selector.Health)
	}
	if err := json.Unmarshal(envelope.ExpectedCount, &req.ExpectedCount); err != nil {
		return BulkCreateJobsRequest{}, fmt.Errorf("expected_count must be an integer")
	}
	if req.ExpectedCount < 0 {
		return BulkCreateJobsRequest{}, fmt.Errorf("expected_count must not be negative")
	}
	return req, nil
}

func decodeStrictJSON(reader io.Reader, target interface{}) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("request must contain one JSON object")
		}
		return err
	}
	return nil
}

func (a *API) bulkEligibleSnapshot(ctx context.Context, runtime *executor.RuntimeConfigSnapshot, selector BulkJobSelector, now time.Time) ([]RepositoryOverview, error) {
	stats, err := a.db.RepositoryRunStats(ctx)
	if err != nil {
		return nil, err
	}
	overdueGrace, stuckThreshold := runtime.SyncHealthThresholds()
	snapshot := buildRepositorySnapshot(runtime.Repositories(), stats, now, overdueGrace, stuckThreshold)
	return selectBulkEligible(snapshot, selector), nil
}

func selectBulkEligible(snapshot []RepositoryOverview, selector BulkJobSelector) []RepositoryOverview {
	matched := searchRepositorySnapshot(snapshot, selector.Search)
	matched = filterRepositorySnapshot(matched, RepositoryHealthFilter{
		Health:  selector.Health,
		Overdue: selector.Overdue,
	})
	eligible := make([]RepositoryOverview, 0, len(matched))
	for _, repository := range matched {
		if repository.Stuck {
			continue
		}
		if repository.LastStatus == string(StatusFailed) ||
			repository.LastStatus == string(StatusCancelled) || repository.Overdue {
			eligible = append(eligible, repository)
		}
	}
	sortRepositorySnapshot(eligible, "name", "asc")
	return eligible
}

func (a *API) currentRepositorySnapshot(ctx context.Context, now time.Time) ([]RepositoryOverview, error) {
	stats, err := a.db.RepositoryRunStats(ctx)
	if err != nil {
		return nil, err
	}
	var repos []typedef.Repository
	var overdueGrace, stuckThreshold time.Duration
	if cfg := a.configSnapshot(); cfg != nil {
		repos = cfg.Repository
		overdueGrace = cfg.SyncOverdueGrace
		stuckThreshold = cfg.SyncStuckThreshold
	}
	return buildRepositorySnapshot(repos, stats, now, overdueGrace, stuckThreshold), nil
}

func (a *API) bulkRepositoryEligible(ctx context.Context, runtime *executor.RuntimeConfigSnapshot, repositoryKey string, selector BulkJobSelector, now time.Time) (bool, error) {
	repository, found := runtime.Repository(repositoryKey)
	if !found {
		return false, nil
	}

	stats, err := a.bulkRepositoryStats(ctx, repository)
	if err != nil {
		return false, err
	}
	overdueGrace, stuckThreshold := runtime.SyncHealthThresholds()
	snapshot := buildRepositorySnapshot(
		[]typedef.Repository{repository}, stats, now,
		overdueGrace, stuckThreshold,
	)
	return len(selectBulkEligible(snapshot, selector)) == 1, nil
}

// repositoryRunStatsForCandidate reads only the candidate's indexed execution
// history. This keeps enqueue-time rechecks O(candidates) instead of rebuilding
// the full ~6,000-repository fleet snapshot for every item.
func (a *API) repositoryRunStatsForCandidate(ctx context.Context, repository typedef.Repository) (map[string]db.RepositoryRunStats, error) {
	query, args := repositoryRunStatsQuery(repository)

	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stats := make(map[string]db.RepositoryRunStats)
	for rows.Next() {
		var (
			id           string
			repoKey      string
			start        time.Time
			end          sql.NullTime
			status       string
			errorMessage sql.NullString
		)
		if err := rows.Scan(&id, &repoKey, &start, &end, &status, &errorMessage); err != nil {
			return nil, err
		}
		if _, exists := stats[repoKey]; exists {
			continue
		}
		entry := db.RepositoryRunStats{
			LatestExecutionID: id,
			LatestStatus:      status,
			LatestStart:       start,
			TotalRuns:         1,
		}
		if end.Valid {
			latestEnd := end.Time
			entry.LatestEnd = &latestEnd
		}
		if errorMessage.Valid {
			entry.LatestError = errorMessage.String
		}
		stats[repoKey] = entry
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return stats, nil
}

func repositoryRunStatsQuery(repository typedef.Repository) (string, []interface{}) {
	key := repository.Key()
	query := `
		SELECT id, repo_key, start_time, end_time, status, error_message
		FROM executions WHERE repo_key = ?
		ORDER BY repo_key, start_time DESC, id DESC LIMIT 1`
	args := []interface{}{key}
	if repository.GetType() == typedef.TypeOrg || repository.GetType() == typedef.TypeUser {
		prefix := key + "/"
		query = `
			SELECT id, repo_key, start_time, end_time, status, error_message
			FROM executions WHERE repo_key >= ? AND repo_key < ?
			ORDER BY repo_key, start_time DESC, id DESC`
		args = []interface{}{prefix, key + "0"}
	}
	return query, args
}

func (a *API) CancelJob(c *gin.Context) {
	jobID := c.Param("id")

	if jobID == "" {
		c.JSON(http.StatusBadRequest, Response{
			Code:    400,
			Message: "Job ID is required",
		})
		return
	}

	// Cancel the job
	err := a.executor.CancelJob(jobID)
	if err != nil {
		exists, lookupErr := a.db.ExecutionExists(c.Request.Context(), jobID)
		if lookupErr == nil && !exists {
			c.JSON(http.StatusOK, Response{
				Code: 200,
				Data: CancelJobResponse{Status: string(executor.StatusCancelled)},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, Response{
			Code:    500,
			Message: "Failed to cancel job: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, Response{
		Code: 200,
		Data: CancelJobResponse{
			Status: string(executor.StatusCancelled),
		},
	})
}

func (a *API) GetJobs(c *gin.Context) {
	// Parse query parameters
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	status := c.Query("status")
	repository := c.Query("repository")

	// Validate pagination
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}

	const jobSelect = "SELECT id, job_name, repo_key, start_time, end_time, status, error_message FROM executions"

	// Build query
	query := jobSelect + " WHERE 1=1"
	args := []interface{}{}

	if status != "" && status != "all" {
		query += " AND status = ?"
		args = append(args, status)
	}

	if repository != "" {
		// name 模糊匹配原始输入；URL 搜索词先规范化，命中规范化后的 repo_key。
		query += " AND (job_name LIKE ? ESCAPE '\\' OR repo_key LIKE ? ESCAPE '\\')"
		args = append(args, "%"+escapeLike(repository)+"%", "%"+escapeLike(typedef.NormalizeURL(repository))+"%")
	}

	// Get total count
	countQuery := "SELECT COUNT(*) FROM executions" + query[len(jobSelect):]
	var total int64
	err := a.db.QueryRow(countQuery, args...).Scan(&total)
	if err != nil {
		c.JSON(http.StatusInternalServerError, Response{
			Code:    500,
			Message: "Failed to count jobs: " + err.Error(),
		})
		return
	}

	// Get paginated results
	query += " ORDER BY start_time DESC LIMIT ? OFFSET ?"
	args = append(args, limit, (page-1)*limit)

	rows, err := a.db.Query(query, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, Response{
			Code:    500,
			Message: "Failed to query jobs: " + err.Error(),
		})
		return
	}
	defer rows.Close()

	jobs := make([]Job, 0)
	cfg := a.configSnapshot()
	for rows.Next() {
		var job Job
		var startTime time.Time
		var endTime *time.Time
		var errorMessage *string

		var repoKey string
		err := rows.Scan(&job.ID, &job.Name, &repoKey, &startTime, &endTime, &job.Status, &errorMessage)
		if err != nil {
			c.JSON(http.StatusInternalServerError, Response{
				Code:    500,
				Message: "Failed to scan job: " + err.Error(),
			})
			return
		}

		job.StartTime = &startTime
		job.EndTime = endTime
		if errorMessage != nil {
			job.ErrorMessage = *errorMessage
		}

		// Resolve URL from config by identity key; unmatched (renamed/deleted/old
		// empty-key rows) keeps the job_name snapshot and empty URL.
		for _, repo := range cfg.Repository {
			if repo.Matches(repoKey) {
				job.URL = repo.URL
				break
			}
		}

		jobs = append(jobs, job)
	}

	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, Response{
			Code:    500,
			Message: "Failed to iterate jobs: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, Response{
		Code: 200,
		Data: ListJobsResponse{
			Jobs:  jobs,
			Total: total,
			Page:  page,
			Limit: limit,
		},
	})
}

// GetJobLogs streams logs for a given job ID as Server-Sent Events.
func (a *API) GetJobLogs(c *gin.Context) {
	jobID := c.Param("id")
	if jobID == "" {
		c.JSON(http.StatusBadRequest, Response{
			Code:    400,
			Message: "Job ID is required",
		})
		return
	}

	// Validate the job exists
	var status string
	err := a.db.QueryRow("SELECT status FROM executions WHERE id = ?", jobID).Scan(&status)
	if err != nil {
		c.JSON(http.StatusNotFound, Response{
			Code:    404,
			Message: "Job not found",
		})
		return
	}

	// Set SSE headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	var lastID int64
	heartbeatTicker := time.NewTicker(15 * time.Second)
	defer heartbeatTicker.Stop()

	c.Stream(func(w io.Writer) bool {
		// Check if the client disconnected
		if c.Request.Context().Err() != nil {
			return false
		}

		// Query logs newer than lastID
		rows, err := a.db.Query(
			"SELECT id, execution_id, timestamp, level, message FROM logs WHERE execution_id = ? AND id > ? ORDER BY id ASC",
			jobID, lastID,
		)
		if err != nil {
			return true
		}

		for rows.Next() {
			var entry LogEntry
			if err := rows.Scan(&entry.ID, &entry.ExecutionID, &entry.Timestamp, &entry.Level, &entry.Message); err != nil {
				rows.Close()
				return true
			}
			lastID = entry.ID

			data, err := json.Marshal(entry)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		rows.Close()

		// Check job completion status; stop streaming after flushing remaining logs
		var currentStatus string
		if err := a.db.QueryRow("SELECT status FROM executions WHERE id = ?", jobID).Scan(&currentStatus); err == nil {
			if currentStatus == string(executor.StatusCompleted) ||
				currentStatus == string(executor.StatusFailed) ||
				currentStatus == string(executor.StatusCancelled) {
				fmt.Fprintf(w, "event: done\ndata: {\"status\":\"%s\"}\n\n", currentStatus)
				return false
			}
		}

		// Wait before polling again, but bail out on disconnect or heartbeat
		select {
		case <-heartbeatTicker.C:
			// Send a heartbeat comment to keep the connection alive
			fmt.Fprintf(w, ": heartbeat\n\n")
			return true
		case <-c.Request.Context().Done():
			// Client disconnected; stop streaming
			return false
		case <-time.After(time.Second):
			// Poll for new logs
		}

		return true
	})
}

// GetRepositories returns repositories with per-repo execution stats, last/next
// run times, search (fuzzy name or URL match) and pagination.
func (a *API) GetRepositories(c *gin.Context) {
	now := time.Now()
	filter, page, limit, err := parseRepositoryHealthQuery(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, Response{Code: 400, Message: "Invalid repository query: " + err.Error()})
		return
	}

	snapshot, err := a.currentRepositorySnapshot(c.Request.Context(), now)
	if err != nil {
		c.JSON(http.StatusInternalServerError, Response{Code: 500, Message: "Failed to query repository stats: " + err.Error()})
		return
	}
	matched := searchRepositorySnapshot(snapshot, filter.Search)
	summary := summarizeRepositorySnapshot(matched)
	filtered := filterRepositorySnapshot(matched, filter)
	sortRepositorySnapshot(filtered, filter.Sort, filter.Direction)

	total := len(filtered)
	start := total
	if page <= 1+total/limit {
		start = (page - 1) * limit
	}
	end := start + limit
	if end > total {
		end = total
	}

	c.JSON(http.StatusOK, Response{Code: 200, Data: ListRepositoriesResponse{
		Repositories: filtered[start:end],
		Summary:      summary,
		Total:        total,
		Page:         page,
		Limit:        limit,
	}})
}

func parseRepositoryHealthQuery(c *gin.Context) (RepositoryHealthFilter, int, int, error) {
	health, healthSet := c.GetQuery("health")
	sortKey, sortSet := c.GetQuery("sort")
	direction, directionSet := c.GetQuery("direction")
	if !sortSet {
		sortKey = "attention"
	}
	if !directionSet {
		direction = "asc"
	}
	filter := RepositoryHealthFilter{
		Search:    c.Query("search"),
		Health:    health,
		Sort:      sortKey,
		Direction: direction,
	}
	if healthSet && !validRepositoryHealth(filter.Health) {
		return RepositoryHealthFilter{}, 0, 0, fmt.Errorf("health %q is not supported", filter.Health)
	}
	if !validRepositorySort(filter.Sort) {
		return RepositoryHealthFilter{}, 0, 0, fmt.Errorf("sort %q is not supported", filter.Sort)
	}
	if filter.Direction != "asc" && filter.Direction != "desc" {
		return RepositoryHealthFilter{}, 0, 0, fmt.Errorf("direction %q is not supported", filter.Direction)
	}

	var err error
	if raw, ok := c.GetQuery("overdue"); ok {
		filter.Overdue, err = strictQueryBool(raw)
		if err != nil {
			return RepositoryHealthFilter{}, 0, 0, fmt.Errorf("overdue: %w", err)
		}
	}
	if raw, ok := c.GetQuery("stuck"); ok {
		filter.Stuck, err = strictQueryBool(raw)
		if err != nil {
			return RepositoryHealthFilter{}, 0, 0, fmt.Errorf("stuck: %w", err)
		}
	}
	page, err := strictQueryInt(c, "page", 1, 1, int(^uint(0)>>1))
	if err != nil {
		return RepositoryHealthFilter{}, 0, 0, err
	}
	limit, err := strictQueryInt(c, "limit", 20, 1, 100)
	if err != nil {
		return RepositoryHealthFilter{}, 0, 0, err
	}
	return filter, page, limit, nil
}

func validRepositoryHealth(health string) bool {
	switch health {
	case "healthy", "failed", "overdue", "stuck", "never_synced", "cancelled", "pending", "running", "syncing":
		return true
	default:
		return false
	}
}

func validRepositorySort(sortKey string) bool {
	switch sortKey {
	case "attention", "name", "last_attempt", "last_success", "next_run":
		return true
	default:
		return false
	}
}

func strictQueryBool(raw string) (*bool, error) {
	switch raw {
	case "true":
		value := true
		return &value, nil
	case "false":
		value := false
		return &value, nil
	default:
		return nil, fmt.Errorf("must be true or false")
	}
}

func strictQueryInt(c *gin.Context, name string, defaultValue, minimum, maximum int) (int, error) {
	raw, ok := c.GetQuery(name)
	if !ok {
		return defaultValue, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be between %d and %d", name, minimum, maximum)
	}
	return value, nil
}

// GetJobComponents returns the persisted component rows for one execution.
func (a *API) GetJobComponents(c *gin.Context) {
	jobID := c.Param("id")
	if jobID == "" {
		c.JSON(http.StatusBadRequest, Response{Code: 400, Message: "Job ID is required"})
		return
	}

	exists, err := a.db.ExecutionExists(c.Request.Context(), jobID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, Response{Code: 500, Message: "Failed to check job: " + err.Error()})
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, Response{Code: 404, Message: "Job not found"})
		return
	}

	components, err := a.db.ListComponents(c.Request.Context(), jobID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, Response{Code: 500, Message: "Failed to query job components: " + err.Error()})
		return
	}
	if components == nil {
		components = []db.ComponentExecution{}
	}
	c.JSON(http.StatusOK, Response{Code: 200, Data: ListComponentsResponse{Components: components}})
}

// CreateRepository adds a new repository to the configuration.
func (a *API) CreateRepository(c *gin.Context) {
	var repo typedef.Repository
	if err := c.ShouldBindJSON(&repo); err != nil {
		c.JSON(http.StatusBadRequest, Response{
			Code:    400,
			Message: "Invalid request: " + err.Error(),
		})
		return
	}

	if repo.Name == "" {
		c.JSON(http.StatusBadRequest, Response{
			Code:    400,
			Message: "Repository name is required",
		})
		return
	}

	// user/org 空 URL → 填合成 URL；随后统一判身份键非空。
	repo.URL = repo.EffectiveURL()
	if repo.Key() == "" {
		c.JSON(http.StatusBadRequest, Response{
			Code:    400,
			Message: "Repository needs a non-empty URL (or orgName for user/org type)",
		})
		return
	}
	a.configMu.Lock()
	defer a.configMu.Unlock()
	next := config.Clone(a.config)

	// 判重按身份键（URL），name 允许重复。
	for _, existing := range next.Repository {
		if existing.Key() == repo.Key() {
			c.JSON(http.StatusConflict, Response{
				Code:    409,
				Message: "Repository with URL '" + repo.Key() + "' already exists",
			})
			return
		}
	}

	// Publish a copy-on-write replacement to the API and Executor together.
	next.Repository = append(next.Repository, repo)
	_, msg := a.publishPersistAndRefreshConfigLocked(next, "Repository added in memory but failed to persist config: ")

	c.JSON(http.StatusOK, Response{
		Code:    200,
		Data:    repo,
		Message: msg,
	})
}

// UpdateRepository modifies an existing repository by identity key (partial update via JSON merge).
func (a *API) UpdateRepository(c *gin.Context) {
	// The route uses a *id catch-all so identity keys containing "/" reach the
	// handler; gin prefixes the captured value with "/", which we strip before
	// matching against repo keys.
	id := strings.TrimPrefix(c.Param("id"), "/")
	// Decode request input before entering the configuration generation lock.
	var patch map[string]interface{}
	if err := c.ShouldBindJSON(&patch); err != nil {
		c.JSON(http.StatusBadRequest, Response{
			Code:    400,
			Message: "Invalid request: " + err.Error(),
		})
		return
	}
	a.configMu.Lock()
	defer a.configMu.Unlock()
	next := config.Clone(a.config)

	// Locate the existing repository by identity key (URL).
	idx := -1
	for i, existing := range next.Repository {
		if existing.Matches(id) {
			idx = i
			break
		}
	}
	if idx == -1 {
		c.JSON(http.StatusNotFound, Response{
			Code:    404,
			Message: "Repository not found",
		})
		return
	}

	// Marshal the existing repository into a map, overlay the patch fields,
	// then unmarshal back into a typed struct. This preserves unspecified
	// fields while applying only the supplied changes.
	existingRaw, err := json.Marshal(next.Repository[idx])
	if err != nil {
		c.JSON(http.StatusInternalServerError, Response{
			Code:    500,
			Message: "Failed to marshal repository: " + err.Error(),
		})
		return
	}
	var mergedMap map[string]interface{}
	if err := json.Unmarshal(existingRaw, &mergedMap); err != nil {
		c.JSON(http.StatusInternalServerError, Response{
			Code:    500,
			Message: "Failed to unmarshal repository: " + err.Error(),
		})
		return
	}
	for k, v := range patch {
		mergedMap[k] = v
	}
	finalRaw, err := json.Marshal(mergedMap)
	if err != nil {
		c.JSON(http.StatusInternalServerError, Response{
			Code:    500,
			Message: "Failed to marshal merged repository: " + err.Error(),
		})
		return
	}
	var updated typedef.Repository
	if err := json.Unmarshal(finalRaw, &updated); err != nil {
		c.JSON(http.StatusInternalServerError, Response{
			Code:    500,
			Message: "Failed to unmarshal merged repository: " + err.Error(),
		})
		return
	}

	// user/org 空 URL → 填合成 URL；空身份键拒绝；与其他仓库 URL 冲突拒绝。
	updated.URL = updated.EffectiveURL()
	if updated.Key() == "" {
		c.JSON(http.StatusBadRequest, Response{
			Code:    400,
			Message: "Repository needs a non-empty URL (or orgName for user/org type)",
		})
		return
	}
	for i, other := range next.Repository {
		if i != idx && other.Key() == updated.Key() {
			c.JSON(http.StatusConflict, Response{
				Code:    409,
				Message: "Repository with URL '" + updated.Key() + "' already exists",
			})
			return
		}
	}

	next.Repository[idx] = updated
	_, msg := a.publishPersistAndRefreshConfigLocked(next, "Repository updated in memory but failed to persist config: ")

	c.JSON(http.StatusOK, Response{
		Code:    200,
		Data:    updated,
		Message: msg,
	})
}

// DeleteRepository removes a repository from the configuration by identity key.
func (a *API) DeleteRepository(c *gin.Context) {
	// See UpdateRepository: strip the leading "/" gin adds to *id catch-all values.
	id := strings.TrimPrefix(c.Param("id"), "/")
	a.configMu.Lock()
	defer a.configMu.Unlock()
	next := config.Clone(a.config)

	idx := -1
	for i, existing := range next.Repository {
		if existing.Matches(id) {
			idx = i
			break
		}
	}
	if idx == -1 {
		c.JSON(http.StatusNotFound, Response{
			Code:    404,
			Message: "Repository not found",
		})
		return
	}

	// Remove element at idx
	next.Repository = append(next.Repository[:idx], next.Repository[idx+1:]...)
	_, msg := a.publishPersistAndRefreshConfigLocked(next, "Repository deleted in memory but failed to persist config: ")

	c.JSON(http.StatusOK, Response{
		Code:    200,
		Data:    SuccessResponse{Success: true},
		Message: msg,
	})
}

// GetStorages returns all storage backends from the configuration.
func (a *API) GetStorages(c *gin.Context) {
	cfg := a.configSnapshot()
	c.JSON(http.StatusOK, Response{
		Code: 200,
		Data: cfg.Storage,
	})
}

// CreateStorage adds a new storage backend to the configuration.
func (a *API) CreateStorage(c *gin.Context) {
	var storage typedef.MultiStorage
	if err := c.ShouldBindJSON(&storage); err != nil {
		c.JSON(http.StatusBadRequest, Response{
			Code:    400,
			Message: "Invalid request: " + err.Error(),
		})
		return
	}

	if storage.Name == "" {
		c.JSON(http.StatusBadRequest, Response{
			Code:    400,
			Message: "Storage name is required",
		})
		return
	}

	// Validate type
	if err := storage.ValidateType(); err != nil {
		c.JSON(http.StatusBadRequest, Response{
			Code:    400,
			Message: err.Error(),
		})
		return
	}
	a.configMu.Lock()
	defer a.configMu.Unlock()
	next := config.Clone(a.config)

	// Check for duplicates
	for _, existing := range next.Storage {
		if existing.Name == storage.Name {
			c.JSON(http.StatusConflict, Response{
				Code:    409,
				Message: "Storage with name '" + storage.Name + "' already exists",
			})
			return
		}
	}

	// Publish a copy-on-write replacement to the API and Executor together.
	next.Storage = append(next.Storage, storage)
	_, msg := a.publishPersistAndRefreshConfigLocked(next, "Storage added in memory but failed to persist config: ")

	c.JSON(http.StatusOK, Response{
		Code:    200,
		Data:    storage,
		Message: msg,
	})
}

// UpdateStorage modifies an existing storage backend by name (partial update via JSON merge).
func (a *API) UpdateStorage(c *gin.Context) {
	id := c.Param("id")
	// Decode request input before entering the configuration generation lock.
	var patch map[string]interface{}
	if err := c.ShouldBindJSON(&patch); err != nil {
		c.JSON(http.StatusBadRequest, Response{
			Code:    400,
			Message: "Invalid request: " + err.Error(),
		})
		return
	}
	a.configMu.Lock()
	defer a.configMu.Unlock()
	next := config.Clone(a.config)

	// Locate the existing storage
	idx := -1
	for i, existing := range next.Storage {
		if existing.Name == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		c.JSON(http.StatusNotFound, Response{
			Code:    404,
			Message: "Storage not found",
		})
		return
	}

	// Marshal the existing storage into a map, overlay the patch fields,
	// then unmarshal back into a typed struct. This preserves unspecified
	// fields while applying only the supplied changes.
	existingRaw, err := json.Marshal(next.Storage[idx])
	if err != nil {
		c.JSON(http.StatusInternalServerError, Response{
			Code:    500,
			Message: "Failed to marshal storage: " + err.Error(),
		})
		return
	}
	var mergedMap map[string]interface{}
	if err := json.Unmarshal(existingRaw, &mergedMap); err != nil {
		c.JSON(http.StatusInternalServerError, Response{
			Code:    500,
			Message: "Failed to unmarshal storage: " + err.Error(),
		})
		return
	}
	for k, v := range patch {
		mergedMap[k] = v
	}
	finalRaw, err := json.Marshal(mergedMap)
	if err != nil {
		c.JSON(http.StatusInternalServerError, Response{
			Code:    500,
			Message: "Failed to marshal merged storage: " + err.Error(),
		})
		return
	}
	var updated typedef.MultiStorage
	if err := json.Unmarshal(finalRaw, &updated); err != nil {
		c.JSON(http.StatusInternalServerError, Response{
			Code:    500,
			Message: "Failed to unmarshal merged storage: " + err.Error(),
		})
		return
	}

	if err := updated.ValidateType(); err != nil {
		c.JSON(http.StatusBadRequest, Response{
			Code:    400,
			Message: err.Error(),
		})
		return
	}
	next.Storage[idx] = updated
	_, msg := a.publishPersistAndRefreshConfigLocked(next, "Storage updated in memory but failed to persist config: ")

	c.JSON(http.StatusOK, Response{
		Code:    200,
		Data:    updated,
		Message: msg,
	})
}

// DeleteStorage removes a storage backend from the configuration by name.
func (a *API) DeleteStorage(c *gin.Context) {
	id := c.Param("id")
	a.configMu.Lock()
	defer a.configMu.Unlock()
	next := config.Clone(a.config)

	idx := -1
	for i, existing := range next.Storage {
		if existing.Name == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		c.JSON(http.StatusNotFound, Response{
			Code:    404,
			Message: "Storage not found",
		})
		return
	}

	// Remove element at idx
	next.Storage = append(next.Storage[:idx], next.Storage[idx+1:]...)
	_, msg := a.publishPersistAndRefreshConfigLocked(next, "Storage deleted in memory but failed to persist config: ")

	c.JSON(http.StatusOK, Response{
		Code:    200,
		Data:    SuccessResponse{Success: true},
		Message: msg,
	})
}

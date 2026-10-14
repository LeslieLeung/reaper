package server

import (
	"sort"
	"strings"
	"time"

	"github.com/wnarutou/gitrieve/internal/config"
	"github.com/wnarutou/gitrieve/internal/db"
	"github.com/wnarutou/gitrieve/internal/typedef"
)

func buildRepositorySnapshot(
	repos []typedef.Repository,
	stats map[string]db.RepositoryRunStats,
	now time.Time,
	overdueGrace time.Duration,
	stuckThreshold time.Duration,
) []RepositoryOverview {
	if overdueGrace <= 0 {
		overdueGrace = config.DefaultSyncOverdueGrace
	}
	if stuckThreshold <= 0 {
		stuckThreshold = config.DefaultSyncStuckThreshold
	}

	overviews := make([]RepositoryOverview, 0, len(repos))
	for _, configured := range repos {
		repo := configured
		repo.URL = repo.EffectiveURL()
		runStats, hasRuns := repositoryStats(stats, repo)
		overview := RepositoryOverview{
			Repository:        repo,
			TotalRuns:         runStats.TotalRuns,
			SuccessRuns:       runStats.SuccessRuns,
			FailedRuns:        runStats.FailedRuns,
			LastStatus:        runStats.LatestStatus,
			LatestExecutionID: runStats.LatestExecutionID,
			LastErrorMessage:  runStats.LatestError,
		}

		if hasRuns {
			attempt := runStats.LatestStart
			overview.LastAttemptTime = &attempt
			overview.LastRunTime = &attempt
		}
		if runStats.LastSuccess != nil {
			success := *runStats.LastSuccess
			overview.LastSuccessTime = &success
		}
		if duration := latestDuration(runStats, hasRuns, now); duration != nil {
			overview.LastDurationSeconds = duration
		}

		schedule, scheduleErr := parseRepositorySchedule(repo.Cron)
		if scheduleErr != nil {
			overview.ScheduleError = scheduleErr.Error()
		} else if schedule != nil {
			next := schedule.Next(now)
			overview.NextRunTime = &next
		}

		active := hasRuns && (runStats.LatestStatus == string(StatusPending) || runStats.LatestStatus == string(StatusRunning))
		if active {
			overview.Stuck = now.Sub(runStats.LatestStart) > stuckThreshold
		}
		if hasRuns && !active && schedule != nil {
			lastAttemptInScheduleLocation := runStats.LatestStart.In(now.Location())
			overview.Overdue = !schedule.Next(lastAttemptInScheduleLocation).After(now.Add(-overdueGrace))
		}
		overview.HealthStatus = repositoryHealthStatus(overview, hasRuns)
		overviews = append(overviews, overview)
	}
	return overviews
}

func repositoryStats(stats map[string]db.RepositoryRunStats, repo typedef.Repository) (db.RepositoryRunStats, bool) {
	key := repo.Key()
	if key == "" {
		return db.RepositoryRunStats{}, false
	}
	if repo.GetType() != typedef.TypeOrg && repo.GetType() != typedef.TypeUser {
		value, ok := stats[key]
		return cloneRepositoryRunStats(value), ok
	}

	prefix := key + "/"
	var aggregate db.RepositoryRunStats
	found := false
	for memberKey, member := range stats {
		if !strings.HasPrefix(memberKey, prefix) {
			continue
		}
		aggregate.TotalRuns += member.TotalRuns
		aggregate.SuccessRuns += member.SuccessRuns
		aggregate.FailedRuns += member.FailedRuns
		if member.LastSuccess != nil && (aggregate.LastSuccess == nil || member.LastSuccess.After(*aggregate.LastSuccess)) {
			lastSuccess := *member.LastSuccess
			aggregate.LastSuccess = &lastSuccess
		}
		if !found || member.LatestStart.After(aggregate.LatestStart) ||
			(member.LatestStart.Equal(aggregate.LatestStart) && member.LatestExecutionID > aggregate.LatestExecutionID) {
			aggregate.LatestExecutionID = member.LatestExecutionID
			aggregate.LatestStatus = member.LatestStatus
			aggregate.LatestStart = member.LatestStart
			aggregate.LatestError = member.LatestError
			aggregate.LatestEnd = nil
			if member.LatestEnd != nil {
				latestEnd := *member.LatestEnd
				aggregate.LatestEnd = &latestEnd
			}
		}
		found = true
	}
	return aggregate, found
}

func cloneRepositoryRunStats(value db.RepositoryRunStats) db.RepositoryRunStats {
	if value.LatestEnd != nil {
		latestEnd := *value.LatestEnd
		value.LatestEnd = &latestEnd
	}
	if value.LastSuccess != nil {
		lastSuccess := *value.LastSuccess
		value.LastSuccess = &lastSuccess
	}
	return value
}

func latestDuration(stats db.RepositoryRunStats, hasRuns bool, now time.Time) *int64 {
	if !hasRuns {
		return nil
	}
	var duration time.Duration
	if stats.LatestStatus == string(StatusPending) || stats.LatestStatus == string(StatusRunning) {
		duration = now.Sub(stats.LatestStart)
	} else {
		if stats.LatestEnd == nil {
			return nil
		}
		duration = stats.LatestEnd.Sub(stats.LatestStart)
	}
	if duration < 0 {
		duration = 0
	}
	seconds := int64(duration / time.Second)
	return &seconds
}

func repositoryHealthStatus(overview RepositoryOverview, hasRuns bool) string {
	switch {
	case !hasRuns:
		return "never_synced"
	case overview.Stuck:
		return "stuck"
	case overview.LastStatus == string(StatusPending):
		return "pending"
	case overview.LastStatus == string(StatusRunning):
		return "running"
	case overview.LastStatus == string(StatusFailed):
		return "failed"
	case overview.LastStatus == string(StatusCancelled):
		return "cancelled"
	case overview.Overdue:
		return "overdue"
	default:
		return "healthy"
	}
}

func searchRepositorySnapshot(in []RepositoryOverview, search string) []RepositoryOverview {
	needle := strings.ToLower(search)
	out := make([]RepositoryOverview, 0, len(in))
	for _, overview := range in {
		if needle == "" || strings.Contains(strings.ToLower(overview.Name), needle) ||
			strings.Contains(strings.ToLower(overview.EffectiveURL()), needle) {
			out = append(out, overview)
		}
	}
	return out
}

func summarizeRepositorySnapshot(in []RepositoryOverview) RepositoryHealthSummary {
	summary := RepositoryHealthSummary{Total: len(in)}
	for _, overview := range in {
		switch overview.LastStatus {
		case "":
			summary.NeverSynced++
		case string(StatusFailed):
			summary.Failed++
		case string(StatusPending):
			summary.Pending++
		case string(StatusRunning):
			summary.Running++
		case string(StatusCancelled):
			summary.Cancelled++
		default:
			if overview.HealthStatus == "healthy" {
				summary.Healthy++
			}
		}
		if overview.Overdue {
			summary.Overdue++
		}
		if overview.Stuck {
			summary.Stuck++
		}
	}
	return summary
}

func filterRepositorySnapshot(in []RepositoryOverview, filter RepositoryHealthFilter) []RepositoryOverview {
	out := make([]RepositoryOverview, 0, len(in))
	for _, overview := range in {
		if filter.Health != "" {
			switch filter.Health {
			case "pending", "running":
				// Stuck is a health warning; it does not replace the execution state.
				if overview.LastStatus != filter.Health {
					continue
				}
			case "syncing":
				if overview.LastStatus != string(StatusPending) && overview.LastStatus != string(StatusRunning) {
					continue
				}
			default:
				if overview.HealthStatus != filter.Health {
					continue
				}
			}
		}
		if filter.Overdue != nil && overview.Overdue != *filter.Overdue {
			continue
		}
		if filter.Stuck != nil && overview.Stuck != *filter.Stuck {
			continue
		}
		out = append(out, overview)
	}
	return out
}

func sortRepositorySnapshot(in []RepositoryOverview, sortKey, direction string) {
	descending := direction == "desc"
	sort.SliceStable(in, func(i, j int) bool {
		left, right := in[i], in[j]
		if sortKey == "last_attempt" && (left.LastAttemptTime == nil) != (right.LastAttemptTime == nil) {
			return left.LastAttemptTime != nil
		}
		if sortKey == "last_success" && (left.LastSuccessTime == nil) != (right.LastSuccessTime == nil) {
			return left.LastSuccessTime != nil
		}
		if sortKey == "next_run" && (left.NextRunTime == nil) != (right.NextRunTime == nil) {
			return left.NextRunTime != nil
		}
		comparison := compareOverviewPrimary(left, right, sortKey)
		if comparison != 0 {
			if descending {
				return comparison > 0
			}
			return comparison < 0
		}
		leftName, rightName := strings.ToLower(left.Name), strings.ToLower(right.Name)
		if leftName != rightName {
			return leftName < rightName
		}
		return typedef.NormalizeURL(left.EffectiveURL()) < typedef.NormalizeURL(right.EffectiveURL())
	})
}

func compareOverviewPrimary(left, right RepositoryOverview, sortKey string) int {
	switch sortKey {
	case "attention":
		return compareInt(attentionRank(left), attentionRank(right))
	case "name":
		return strings.Compare(strings.ToLower(left.Name), strings.ToLower(right.Name))
	case "last_attempt":
		return compareOptionalTime(left.LastAttemptTime, right.LastAttemptTime)
	case "last_success":
		return compareOptionalTime(left.LastSuccessTime, right.LastSuccessTime)
	case "next_run":
		return compareOptionalTime(left.NextRunTime, right.NextRunTime)
	default:
		return 0
	}
}

func attentionRank(overview RepositoryOverview) int {
	switch {
	case overview.Stuck || overview.HealthStatus == "stuck":
		return 0
	case overview.HealthStatus == "failed":
		return 1
	case overview.Overdue || overview.HealthStatus == "overdue":
		return 2
	case overview.HealthStatus == "never_synced":
		return 3
	case overview.HealthStatus == "cancelled":
		return 4
	case overview.HealthStatus == "pending":
		return 5
	case overview.HealthStatus == "running":
		return 6
	default:
		return 7
	}
}

func compareOptionalTime(left, right *time.Time) int {
	if left == nil && right == nil {
		return 0
	}
	if left == nil {
		return 1
	}
	if right == nil {
		return -1
	}
	if left.Before(*right) {
		return -1
	}
	if left.After(*right) {
		return 1
	}
	return 0
}

func compareInt(left, right int) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

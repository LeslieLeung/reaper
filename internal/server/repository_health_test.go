package server

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/wnarutou/gitrieve/internal/config"
	"github.com/wnarutou/gitrieve/internal/db"
	"github.com/wnarutou/gitrieve/internal/typedef"
)

func TestRepositoryHealthUsesAttentionPrecedenceAndSafeDurations(t *testing.T) {
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	ended := now.Add(-2 * time.Hour)
	cases := []struct {
		name            string
		cron            string
		stats           *db.RepositoryRunStats
		wantHealth      string
		wantOverdue     bool
		wantStuck       bool
		wantDuration    *int64
		wantScheduleErr bool
	}{
		{name: "never synced", cron: "0 * * * *", wantHealth: "never_synced"},
		{name: "stuck pending", stats: runStatsAt("pending", now.Add(-25*time.Hour), nil), wantHealth: "stuck", wantStuck: true, wantDuration: secondsPtr(25 * time.Hour)},
		{name: "stuck running", stats: runStatsAt("running", now.Add(-25*time.Hour), nil), wantHealth: "stuck", wantStuck: true, wantDuration: secondsPtr(25 * time.Hour)},
		{name: "exact stuck threshold is still pending", stats: runStatsAt("pending", now.Add(-24*time.Hour), nil), wantHealth: "pending", wantDuration: secondsPtr(24 * time.Hour)},
		{name: "pending", stats: runStatsAt("pending", now.Add(-time.Hour), nil), wantHealth: "pending", wantDuration: secondsPtr(time.Hour)},
		{name: "running", stats: runStatsAt("running", now.Add(-time.Hour), nil), wantHealth: "running", wantDuration: secondsPtr(time.Hour)},
		{name: "failed beats overdue", cron: "0 * * * *", stats: runStatsAt("failed", now.Add(-3*time.Hour), &ended), wantHealth: "failed", wantOverdue: true, wantDuration: secondsPtr(time.Hour)},
		{name: "cancelled", stats: runStatsAt("cancelled", now.Add(-time.Hour), &ended), wantHealth: "cancelled", wantDuration: secondsPtr(0)},
		{name: "cancelled beats overdue", cron: "0 * * * *", stats: runStatsAt("cancelled", now.Add(-3*time.Hour), &ended), wantHealth: "cancelled", wantOverdue: true, wantDuration: secondsPtr(time.Hour)},
		{name: "overdue completed", cron: "0 * * * *", stats: runStatsAt("completed", now.Add(-3*time.Hour), &ended), wantHealth: "overdue", wantOverdue: true, wantDuration: secondsPtr(time.Hour)},
		{name: "healthy", cron: "0 * * * *", stats: runStatsAt("completed", now.Add(-20*time.Minute), &ended), wantHealth: "healthy", wantDuration: secondsPtr(0)},
		{name: "active is never overdue", cron: "0 * * * *", stats: runStatsAt("running", now.Add(-3*time.Hour), nil), wantHealth: "running", wantDuration: secondsPtr(3 * time.Hour)},
		{name: "invalid cron", cron: "bad cron", stats: runStatsAt("completed", now.Add(-3*time.Hour), &ended), wantHealth: "healthy", wantDuration: secondsPtr(time.Hour), wantScheduleErr: true},
		{name: "terminal row without end has no duration", stats: runStatsAt("failed", now.Add(-time.Hour), nil), wantHealth: "failed"},
		{name: "future active start clamps duration", stats: runStatsAt("running", now.Add(time.Hour), nil), wantHealth: "running", wantDuration: secondsPtr(0)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := typedef.Repository{Name: tc.name, URL: "https://github.com/acme/" + tc.name, Cron: tc.cron}
			stats := map[string]db.RepositoryRunStats{}
			if tc.stats != nil {
				stats[repo.Key()] = *tc.stats
			}

			got := buildRepositorySnapshot([]typedef.Repository{repo}, stats, now, 30*time.Minute, 24*time.Hour)
			require.Len(t, got, 1)
			require.Equal(t, tc.wantHealth, got[0].HealthStatus)
			require.Equal(t, tc.wantOverdue, got[0].Overdue)
			require.Equal(t, tc.wantStuck, got[0].Stuck)
			require.Equal(t, tc.wantDuration, got[0].LastDurationSeconds)
			require.Equal(t, tc.wantScheduleErr, got[0].ScheduleError != "")
			if tc.stats == nil {
				require.Nil(t, got[0].LastAttemptTime)
				require.Nil(t, got[0].LastRunTime)
			} else {
				require.Equal(t, tc.stats.LatestStart, *got[0].LastAttemptTime)
				require.Equal(t, got[0].LastAttemptTime, got[0].LastRunTime)
			}
		})
	}
}

func TestRepositoryOverdueUsesCronBoundaryGraceAndLocalDST(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)

	cases := []struct {
		name        string
		now         time.Time
		lastAttempt time.Time
		cron        string
		grace       time.Duration
		want        bool
	}{
		{
			name:        "next boundary equal to grace cutoff is overdue",
			now:         time.Date(2026, time.September, 4, 12, 30, 0, 0, time.UTC),
			lastAttempt: time.Date(2026, time.September, 4, 10, 0, 0, 0, time.UTC),
			cron:        "0 11 * * *",
			grace:       90 * time.Minute,
			want:        true,
		},
		{
			name:        "next boundary after grace cutoff is current",
			now:         time.Date(2026, time.September, 4, 12, 29, 59, 0, time.UTC),
			lastAttempt: time.Date(2026, time.September, 4, 10, 0, 0, 0, time.UTC),
			cron:        "0 11 * * *",
			grace:       90 * time.Minute,
			want:        false,
		},
		{
			name:        "spring DST skips nonexistent local boundary",
			now:         time.Date(2026, time.March, 8, 4, 0, 0, 0, newYork),
			lastAttempt: time.Date(2026, time.March, 7, 3, 30, 0, 0, newYork),
			cron:        "30 2 * * *",
			grace:       30 * time.Minute,
			want:        false,
		},
		{
			name:        "fall DST evaluates boundary in repository location",
			now:         time.Date(2026, time.November, 1, 2, 45, 0, 0, newYork),
			lastAttempt: time.Date(2026, time.October, 31, 2, 30, 0, 0, newYork),
			cron:        "30 2 * * *",
			grace:       15 * time.Minute,
			want:        true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			end := tc.lastAttempt.Add(time.Minute)
			repo := typedef.Repository{Name: tc.name, URL: "github.com/acme/" + tc.name, Cron: tc.cron}
			stats := map[string]db.RepositoryRunStats{repo.Key(): *runStatsAt("completed", tc.lastAttempt, &end)}
			got := buildRepositorySnapshot([]typedef.Repository{repo}, stats, tc.now, tc.grace, time.Hour)
			require.Equal(t, tc.want, got[0].Overdue)
		})
	}
}

func TestRepositoryHealthDefaultsThresholdsAndPreservesOrgRollup(t *testing.T) {
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	alphaEnd := now.Add(-47 * time.Hour)
	betaEnd := now.Add(-22 * time.Hour)
	stats := map[string]db.RepositoryRunStats{
		"github.com/acme/alpha": {
			LatestExecutionID: "alpha", LatestStatus: "completed", LatestStart: now.Add(-48 * time.Hour), LatestEnd: &alphaEnd,
			LastSuccess: &alphaEnd, TotalRuns: 2, SuccessRuns: 2,
		},
		"github.com/acme/beta": {
			LatestExecutionID: "beta", LatestStatus: "failed", LatestStart: now.Add(-23 * time.Hour), LatestEnd: &betaEnd,
			LatestError: "beta failed", TotalRuns: 3, FailedRuns: 3,
		},
		"github.com/acme2/outside": {
			LatestExecutionID: "outside", LatestStatus: "running", LatestStart: now.Add(-48 * time.Hour), TotalRuns: 99,
		},
	}
	repos := []typedef.Repository{{Name: "Acme", Type: typedef.TypeOrg, OrgName: "acme"}}

	got := buildRepositorySnapshot(repos, stats, now, 0, 0)
	require.Len(t, got, 1)
	require.Equal(t, "https://github.com/acme", got[0].URL)
	require.Equal(t, int64(5), got[0].TotalRuns)
	require.Equal(t, int64(2), got[0].SuccessRuns)
	require.Equal(t, int64(3), got[0].FailedRuns)
	require.Equal(t, "beta", got[0].LatestExecutionID)
	require.Equal(t, "failed", got[0].HealthStatus)
	require.Equal(t, "beta failed", got[0].LastErrorMessage)
	require.Equal(t, config.DefaultSyncOverdueGrace, 30*time.Minute)
	require.Equal(t, config.DefaultSyncStuckThreshold, 24*time.Hour)
	require.Empty(t, repos[0].URL, "snapshot projection must not mutate config entries")

	pendingRecent := typedef.Repository{Name: "recent", URL: "github.com/acme/recent"}
	pendingOld := typedef.Repository{Name: "old", URL: "github.com/acme/old"}
	graceBoundary := typedef.Repository{Name: "grace", URL: "github.com/acme/grace", Cron: "45 * * * *"}
	graceEnd := now.Add(-59 * time.Minute)
	defaultStats := map[string]db.RepositoryRunStats{
		pendingRecent.Key(): *runStatsAt("pending", now.Add(-23*time.Hour), nil),
		pendingOld.Key():    *runStatsAt("pending", now.Add(-25*time.Hour), nil),
		graceBoundary.Key(): *runStatsAt("completed", now.Add(-time.Hour), &graceEnd),
	}
	defaults := buildRepositorySnapshot([]typedef.Repository{pendingRecent, pendingOld, graceBoundary}, defaultStats, now, -time.Second, 0)
	require.False(t, defaults[0].Stuck)
	require.True(t, defaults[1].Stuck)
	require.False(t, defaults[2].Overdue, "default grace keeps the 11:45 boundary within grace at noon")
}

func TestRepositoryHealthSearchFilterSummaryAndSortAreDeterministic(t *testing.T) {
	when := time.Date(2026, time.September, 4, 8, 0, 0, 0, time.UTC)
	input := []RepositoryOverview{
		{Repository: typedef.Repository{Name: "beta", URL: "https://github.com/acme/beta"}, HealthStatus: "failed", LastStatus: "failed", LastAttemptTime: timePointer(when.Add(time.Hour)), LastSuccessTime: timePointer(when), Overdue: true},
		{Repository: typedef.Repository{Name: "Alpha", URL: "https://github.com/acme/zeta"}, HealthStatus: "pending", LastStatus: "pending", LastAttemptTime: timePointer(when), Stuck: true},
		{Repository: typedef.Repository{Name: "alpha", URL: "https://github.com/acme/alpha"}, HealthStatus: "running", LastStatus: "running", LastAttemptTime: timePointer(when)},
		{Repository: typedef.Repository{Name: "never", URL: "https://gitlab.com/other/never"}, HealthStatus: "never_synced"},
		{Repository: typedef.Repository{Name: "done", URL: "https://github.com/acme/done"}, HealthStatus: "overdue", LastStatus: "completed", LastSuccessTime: timePointer(when.Add(-time.Hour)), Overdue: true},
		{Repository: typedef.Repository{Name: "stop", URL: "https://github.com/acme/stop"}, HealthStatus: "cancelled", LastStatus: "cancelled"},
	}

	searched := searchRepositorySnapshot(input, "GITHUB.COM/ACME")
	require.Len(t, searched, 5)
	summary := summarizeRepositorySnapshot(searched)
	require.Equal(t, RepositoryHealthSummary{Total: 5, Failed: 1, Pending: 1, Running: 1, Cancelled: 1, Overdue: 2, Stuck: 1}, summary)
	healthy := filterRepositorySnapshot(searched, RepositoryHealthFilter{Health: "healthy"})
	require.Equal(t, summary.Healthy, len(healthy), "Healthy summary card must equal its health filter")

	syncing := filterRepositorySnapshot(searched, RepositoryHealthFilter{Health: "syncing"})
	require.Len(t, syncing, 2)
	require.Equal(t, []string{"Alpha", "alpha"}, overviewNames(syncing))
	require.Len(t, filterRepositorySnapshot(searched, RepositoryHealthFilter{Health: "failed", Overdue: boolPointer(true)}), 1)
	require.Len(t, filterRepositorySnapshot(searched, RepositoryHealthFilter{Stuck: boolPointer(false)}), 4)

	sorted := append([]RepositoryOverview(nil), input...)
	sortRepositorySnapshot(sorted, "attention", "asc")
	require.Equal(t, []string{"Alpha", "beta", "done", "never", "stop", "alpha"}, overviewNames(sorted))
	sortRepositorySnapshot(sorted, "name", "asc")
	require.Equal(t, []string{"alpha", "Alpha", "beta", "done", "never", "stop"}, overviewNames(sorted), "normalized key breaks case-folded name ties")
	sortRepositorySnapshot(sorted, "last_attempt", "desc")
	require.Equal(t, []string{"beta", "alpha", "Alpha", "done", "never", "stop"}, overviewNames(sorted), "descending times keep nil values last")

	sortCases := []struct {
		sortKey   string
		direction string
		want      []string
	}{
		{sortKey: "attention", direction: "desc", want: []string{"alpha", "stop", "never", "done", "beta", "Alpha"}},
		{sortKey: "name", direction: "desc", want: []string{"stop", "never", "done", "beta", "alpha", "Alpha"}},
		{sortKey: "last_attempt", direction: "asc", want: []string{"alpha", "Alpha", "beta", "done", "never", "stop"}},
		{sortKey: "last_success", direction: "asc", want: []string{"done", "beta", "alpha", "Alpha", "never", "stop"}},
		{sortKey: "last_success", direction: "desc", want: []string{"beta", "done", "alpha", "Alpha", "never", "stop"}},
	}
	for _, tc := range sortCases {
		candidate := append([]RepositoryOverview(nil), input...)
		sortRepositorySnapshot(candidate, tc.sortKey, tc.direction)
		require.Equal(t, tc.want, overviewNames(candidate), tc.sortKey+" "+tc.direction)
	}

	require.Equal(t, "beta", input[0].Name, "helpers must not mutate shared snapshot inputs except the explicit sort target")
}

func TestRepositoryNextRunSort(t *testing.T) {
	when := time.Date(2026, time.September, 27, 8, 0, 0, 0, time.UTC)
	input := []RepositoryOverview{
		{Repository: typedef.Repository{Name: "unscheduled", URL: "github.com/acme/a"}},
		{Repository: typedef.Repository{Name: "later", URL: "github.com/acme/b"}, NextRunTime: timePointer(when.Add(time.Hour))},
		{Repository: typedef.Repository{Name: "early-z", URL: "github.com/acme/z"}, NextRunTime: timePointer(when)},
		{Repository: typedef.Repository{Name: "early-c", URL: "github.com/acme/c"}, NextRunTime: timePointer(when)},
		{Repository: typedef.Repository{Name: "invalid", URL: "github.com/acme/d"}, ScheduleError: "invalid cron"},
	}
	for _, tc := range []struct {
		direction string
		want      []string
	}{
		{"asc", []string{"early-c", "early-z", "later", "invalid", "unscheduled"}},
		{"desc", []string{"later", "early-c", "early-z", "invalid", "unscheduled"}},
	} {
		t.Run(tc.direction, func(t *testing.T) {
			candidate := append([]RepositoryOverview(nil), input...)
			sortRepositorySnapshot(candidate, "next_run", tc.direction)
			require.Equal(t, tc.want, overviewNames(candidate))
		})
	}
}

func TestRepositoryActiveFiltersIncludeStuckExecutions(t *testing.T) {
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	var repos []typedef.Repository
	stats := make(map[string]db.RepositoryRunStats)
	for _, status := range []string{"running", "pending"} {
		for _, age := range []struct {
			name     string
			duration time.Duration
		}{{"recent", time.Hour}, {"stuck", 25 * time.Hour}} {
			name := status + "-" + age.name
			repo := typedef.Repository{Name: name, URL: "github.com/acme/" + name}
			repos = append(repos, repo)
			stats[repo.Key()] = *runStatsAt(status, now.Add(-age.duration), nil)
		}
	}
	snapshot := buildRepositorySnapshot(repos, stats, now, time.Hour, 24*time.Hour)
	for _, tc := range []struct {
		name   string
		filter RepositoryHealthFilter
		want   []string
	}{
		{"running", RepositoryHealthFilter{Health: "running"}, []string{"running-recent", "running-stuck"}},
		{"pending", RepositoryHealthFilter{Health: "pending"}, []string{"pending-recent", "pending-stuck"}},
		{"syncing", RepositoryHealthFilter{Health: "syncing"}, []string{"running-recent", "running-stuck", "pending-recent", "pending-stuck"}},
		{"stuck", RepositoryHealthFilter{Health: "stuck"}, []string{"running-stuck", "pending-stuck"}},
		{"stuck running", RepositoryHealthFilter{Health: "running", Stuck: boolPointer(true)}, []string{"running-stuck"}},
		{"recent pending", RepositoryHealthFilter{Health: "pending", Stuck: boolPointer(false)}, []string{"pending-recent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, overviewNames(filterRepositorySnapshot(snapshot, tc.filter)))
		})
	}
}

func runStatsAt(status string, start time.Time, end *time.Time) *db.RepositoryRunStats {
	return &db.RepositoryRunStats{
		LatestExecutionID: "execution-" + status,
		LatestStatus:      status,
		LatestStart:       start,
		LatestEnd:         end,
		LatestError:       "error-" + status,
		TotalRuns:         1,
	}
}

func secondsPtr(duration time.Duration) *int64 {
	seconds := int64(duration / time.Second)
	return &seconds
}

func timePointer(value time.Time) *time.Time { return &value }

func boolPointer(value bool) *bool { return &value }

func overviewNames(overviews []RepositoryOverview) []string {
	names := make([]string, len(overviews))
	for i := range overviews {
		names[i] = overviews[i].Name
	}
	return names
}

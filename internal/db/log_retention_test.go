package db

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPruneExecutionLogsUsesTaskEndTime(t *testing.T) {
	d, err := Initialize(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		id, status string
		end        interface{}
	}{
		{"old-completed", "completed", "2026-08-12 11:59:59"},
		{"old-failed", "failed", now.AddDate(0, -3, 0)},
		{"old-cancelled", "cancelled", "2026-07-01T00:00:00Z"},
		{"boundary", "completed", "2026-08-12 12:00:00"},
		{"recent", "completed", "2026-10-11 12:00:00"},
		{"running", "running", nil},
		{"pending", "pending", nil},
		{"active-with-stale-end", "running", "2026-01-01 00:00:00"},
		{"unknown-end", "failed", nil},
		// Local wall times on either side of the cutoff have opposite UTC outcomes.
		{"expired-offset", "completed", time.Date(2026, 8, 12, 19, 59, 59, 0, time.FixedZone("CST", 8*3600))},
		{"retained-offset", "completed", time.Date(2026, 8, 12, 4, 0, 1, 0, time.FixedZone("PST", -8*3600))},
		{"fraction-after", "completed", "2026-08-12T12:00:00.000000001Z"},
	} {
		_, err = d.Exec(`INSERT INTO executions (id, job_name, start_time, end_time, status)
			VALUES (?, 'repo', '2026-01-01 00:00:00', ?, ?)`, tc.id, tc.end, tc.status)
		require.NoError(t, err)
		// Both very old and fresh log timestamps must follow the execution's end time.
		_, err = d.Exec(`INSERT INTO logs (execution_id, timestamp, level, message)
			VALUES (?, '2026-01-01 00:00:00', 'info', 'old line'),
			       (?, '2026-10-12 12:00:00', 'info', 'fresh line')`, tc.id, tc.id)
		require.NoError(t, err)
		require.NoError(t, d.CreateComponents(context.Background(), tc.id, []ComponentName{ComponentCode}))
	}
	deleted, err := d.PruneExecutionLogs(context.Background(), now, 2)
	require.NoError(t, err)
	require.EqualValues(t, 8, deleted)
	var count int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM executions`).Scan(&count))
	require.Equal(t, 12, count)
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM execution_components`).Scan(&count))
	require.Equal(t, 12, count)
	for _, id := range []string{"old-completed", "old-failed", "old-cancelled", "expired-offset"} {
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM logs WHERE execution_id = ?`, id).Scan(&count))
		require.Zero(t, count, id)
	}
	for _, id := range []string{"boundary", "recent", "running", "pending", "active-with-stale-end", "unknown-end", "retained-offset", "fraction-after"} {
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM logs WHERE execution_id = ?`, id).Scan(&count))
		require.Equal(t, 2, count, id)
	}
	deleted, err = d.PruneExecutionLogs(context.Background(), now, 2)
	require.NoError(t, err)
	require.Zero(t, deleted)
}

func TestPruneExecutionLogsConfigurableCalendarMonths(t *testing.T) {
	for _, tc := range []struct {
		name, end string
		now       time.Time
		months    int
		deleted   int64
	}{
		{"one-month", "2026-08-31 11:59:59", time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), 1, 1},
		{"three-months", "2026-08-31 11:59:59", time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), 3, 0},
		{"clamp-short-month", "2026-02-28 12:00:00", time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC), 1, 0},
		{"before-clamped-boundary", "2026-02-28 11:59:59", time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC), 1, 1},
		{"leap-year", "2024-02-29 12:00:00", time.Date(2024, 3, 31, 12, 0, 0, 0, time.UTC), 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Initialize(":memory:")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, d.Close()) })
			_, err = d.Exec(`INSERT INTO executions (id, job_name, start_time, end_time, status)
				VALUES ('task', 'repo', '2024-01-01', ?, 'completed')`, tc.end)
			require.NoError(t, err)
			_, err = d.Exec(`INSERT INTO logs (execution_id, timestamp, level, message)
				VALUES ('task', '2024-01-01', 'info', 'line')`)
			require.NoError(t, err)
			deleted, err := d.PruneExecutionLogs(context.Background(), tc.now, tc.months)
			require.NoError(t, err)
			require.Equal(t, tc.deleted, deleted)
		})
	}
}

func TestPruneExecutionLogsHandlesMultipleBatchesAndCancellation(t *testing.T) {
	d, err := Initialize(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	_, err = d.Exec(`INSERT INTO executions (id, job_name, start_time, end_time, status)
		VALUES ('task', 'repo', '2020-01-01', '2020-01-02', 'completed');
		WITH RECURSIVE lines(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM lines WHERE n < 2501)
		INSERT INTO logs (execution_id, timestamp, level, message)
		SELECT 'task', '2020-01-01', 'info', 'line' FROM lines`)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = d.PruneExecutionLogs(ctx, time.Now(), 2)
	require.ErrorIs(t, err, context.Canceled)
	for _, months := range []int{0, -1} {
		_, err = d.PruneExecutionLogs(context.Background(), time.Now(), months)
		require.Error(t, err)
	}
	var count int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM logs`).Scan(&count))
	require.Equal(t, 2501, count)
	deleted, err := d.PruneExecutionLogs(context.Background(), time.Now(), 2)
	require.NoError(t, err)
	require.EqualValues(t, 2501, deleted)
}

func TestPruneExecutionLogsPagesAcrossExecutionsWithEqualEndTimes(t *testing.T) {
	d, err := Initialize(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	_, err = d.Exec(`
		WITH RECURSIVE tasks(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM tasks WHERE n < 251)
		INSERT INTO executions (id, job_name, start_time, end_time, status)
		SELECT printf('task-%03d', n), 'repo', '2020-01-01', '2020-01-02', 'completed' FROM tasks;
		INSERT INTO logs (execution_id, timestamp, level, message)
		SELECT id, '2020-01-01', 'info', 'line' FROM executions;`)
	require.NoError(t, err)
	deleted, err := d.PruneExecutionLogs(context.Background(), time.Now(), 2)
	require.NoError(t, err)
	require.EqualValues(t, 251, deleted)
	var count int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM logs`).Scan(&count))
	require.Zero(t, count)
}

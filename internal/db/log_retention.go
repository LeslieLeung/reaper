package db

import (
	"context"
	"fmt"
	"time"
)

const logCleanupBatchSize = 1000

// PruneExecutionLogs removes all log lines belonging to terminal executions
// whose end time is strictly before the calendar-month retention cutoff.
// Execution and component history are retained for repository health statistics.
func (d *DB) PruneExecutionLogs(ctx context.Context, now time.Time, months int) (int64, error) {
	if months <= 0 {
		return 0, fmt.Errorf("log retention months must be positive")
	}
	cutoff := logRetentionCutoff(now, months)
	// Existing DATETIME columns contain both Go time.String values (including
	// offsets/monotonic annotations) and SQL timestamps. Use a deliberately wide
	// indexed date bound, then compare parsed instants in Go; SQLite datetime()
	// cannot parse all those legacy encodings. Two days cover timezone offsets
	// on either side of UTC without excluding an expired execution.
	upperDate := cutoff.UTC().Add(48 * time.Hour).Format("2006-01-02")
	lastEnd, lastID := "", ""
	var deleted int64
	for {
		rows, err := d.QueryContext(ctx, `
			SELECT id, end_time, CAST(end_time AS TEXT) FROM executions
			WHERE status IN ('completed', 'failed', 'cancelled') AND end_time IS NOT NULL
			  AND end_time < ? AND (end_time, id) > (?, ?)
			  AND EXISTS (SELECT 1 FROM logs WHERE execution_id = executions.id)
			ORDER BY end_time, id LIMIT 100`, upperDate, lastEnd, lastID)
		if err != nil {
			return deleted, fmt.Errorf("find executions with expired logs: %w", err)
		}
		type candidate struct {
			id, rawEnd string
			end        time.Time
		}
		var candidates []candidate
		for rows.Next() {
			var execution candidate
			if err := rows.Scan(&execution.id, &execution.end, &execution.rawEnd); err != nil {
				rows.Close()
				return deleted, fmt.Errorf("read execution end time for log cleanup: %w", err)
			}
			candidates = append(candidates, execution)
		}
		err = rows.Err()
		rows.Close() // Release the read connection before any write, including :memory:.
		if err != nil {
			return deleted, fmt.Errorf("read log cleanup candidates: %w", err)
		}
		if len(candidates) == 0 {
			return deleted, nil
		}
		for _, execution := range candidates {
			lastEnd, lastID = execution.rawEnd, execution.id
			if !execution.end.Before(cutoff) {
				continue
			}
			for {
				// Recheck the terminal status and unchanged end time in each batch.
				result, err := d.ExecContext(ctx, `DELETE FROM logs WHERE id IN (
					SELECT logs.id FROM logs JOIN executions ON executions.id = logs.execution_id
					WHERE logs.execution_id = ? AND executions.end_time = ?
					  AND executions.status IN ('completed', 'failed', 'cancelled')
					ORDER BY logs.id LIMIT ?)`, execution.id, execution.rawEnd, logCleanupBatchSize)
				if err != nil {
					return deleted, fmt.Errorf("prune logs for execution %q: %w", execution.id, err)
				}
				count, err := result.RowsAffected()
				if err != nil {
					return deleted, fmt.Errorf("count pruned logs: %w", err)
				}
				deleted += count
				if count < logCleanupBatchSize {
					break
				}
			}
		}
	}
}

// Clamp month-end dates instead of letting AddDate normalize February 31 into March.
func logRetentionCutoff(now time.Time, months int) time.Time {
	year, month, day := now.Date()
	target := time.Date(year, month-time.Month(months), 1, now.Hour(), now.Minute(), now.Second(), now.Nanosecond(), now.Location())
	lastDay := time.Date(target.Year(), target.Month()+1, 0, 0, 0, 0, 0, now.Location()).Day()
	if day > lastDay {
		day = lastDay
	}
	return time.Date(target.Year(), target.Month(), day, now.Hour(), now.Minute(), now.Second(), now.Nanosecond(), now.Location())
}

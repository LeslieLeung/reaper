package db

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileDBReadersDoNotBlockWriter(t *testing.T) {
	d, err := Initialize(filepath.Join(t.TempDir(), "readers.db"))
	require.NoError(t, err)
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	insertExecution(t, d, "reader-writer", "github.com/acme/widgets", ComponentRunning)
	require.NoError(t, d.CreateComponents(ctx, "reader-writer", []ComponentName{ComponentRelease}))

	// Hold a reader's snapshot open, as an SSE stream can while sending logs.
	rows, err := d.QueryContext(ctx, `SELECT status FROM execution_components`)
	require.NoError(t, err)
	defer rows.Close()
	require.True(t, rows.Next())
	require.NoError(t, d.FinishComponent(ctx, "reader-writer", ComponentRelease, ComponentCompleted, time.Now(), ""))
	require.NoError(t, rows.Close())

	// A writer transaction must also leave readers able to see committed data.
	tx, err := d.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `UPDATE execution_components SET status = 'failed'`)
	require.NoError(t, err)
	components, err := d.ListComponents(ctx, "reader-writer")
	require.NoError(t, err)
	require.Len(t, components, 1)
	require.Equal(t, ComponentCompleted, components[0].Status)
}

func TestDatabaseInitialization(t *testing.T) {
	db, err := Initialize(":memory:")
	assert.NoError(t, err)
	assert.NotNil(t, db)

	// Test tables exist
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table'").Scan(&count)
	assert.NoError(t, err)
	assert.Greater(t, count, 0)
}

// TestFileDBEnablesWALAndBusyTimeout guards the fix for silently dropped log
// lines: a file-backed DB must run in WAL mode with a busy timeout so the SSE
// log-stream reader and the job-goroutine log writers can operate concurrently
// without "database is locked (SQLITE_BUSY)" write failures.
func TestFileDBEnablesWALAndBusyTimeout(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gitrieve.db")
	testDB, err := Initialize(dbPath)
	assert.NoError(t, err)
	defer testDB.Close()

	var mode string
	err = testDB.QueryRow("PRAGMA journal_mode").Scan(&mode)
	assert.NoError(t, err)
	assert.Equal(t, "wal", strings.ToLower(mode), "file-backed DB must run in WAL journal mode")

	var timeout int
	err = testDB.QueryRow("PRAGMA busy_timeout").Scan(&timeout)
	assert.NoError(t, err)
	assert.Equal(t, 5000, timeout, "busy_timeout must be 5000ms")
}

// A file-backed DB is served to many concurrent goroutines on separate pooled
// connections: the executor job goroutine inserting log rows, SSE log streams
// polling for new rows, and API handlers running the per-repository stats query.
// These readers and writers must wait for the lock instead of failing with
// SQLITE_BUSY — otherwise log rows are silently dropped and /api/repositories
// 500s with "database is locked". This is a behavioral companion to
// TestFileDBEnablesWALAndBusyTimeout: that test checks the pragmas are set,
// this one proves concurrent access actually runs without BUSY failures.
func TestFileBackedDBConcurrentAccessNoSQLiteBusy(t *testing.T) {
	d, err := Initialize(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer d.Close()

	_, err = d.Exec(`INSERT INTO executions (id, job_name, start_time, status) VALUES (?, ?, ?, ?)`,
		"job-1", "test-repo", time.Now(), "running")
	require.NoError(t, err)

	var writeErrs, readErrs int64
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Two writer goroutines mimic the executor's ui.Printf -> logger.Log path.
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := d.Exec(
					`INSERT INTO logs (execution_id, timestamp, level, message) VALUES (?, datetime('now'), ?, ?)`,
					"job-1", "info", "progress line",
				); err != nil {
					atomic.AddInt64(&writeErrs, 1)
				}
			}
		}()
	}

	// A reader goroutine mimics the SSE log-stream poll.
	wg.Add(1)
	go func() {
		defer wg.Done()
		var lastID int64
		for {
			select {
			case <-stop:
				return
			default:
			}
			rows, err := d.Query(
				`SELECT id, execution_id, timestamp, level, message FROM logs WHERE execution_id = ? AND id > ? ORDER BY id ASC`,
				"job-1", lastID,
			)
			if err != nil {
				atomic.AddInt64(&readErrs, 1)
				continue
			}
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id, new(string), new(string), new(string), new(string)); err == nil {
					lastID = id
				}
			}
			rows.Close()
		}
	}()

	// A reader goroutine mimics GetRepositories' per-repo stats aggregation.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			rows, err := d.Query(`
				SELECT job_name, start_time AS last_run, COUNT(*),
				       COALESCE(SUM(status = 'completed'), 0),
				       COALESCE(SUM(status = 'failed'), 0)
				FROM executions
				GROUP BY job_name
				HAVING start_time = MAX(start_time)`)
			if err != nil {
				atomic.AddInt64(&readErrs, 1)
				continue
			}
			rows.Close()
		}
	}()

	// Hammer for long enough that lock contention occurs (without the busy
	// timeout the first run of this test produced dozens of failures).
	time.Sleep(time.Second)
	close(stop)
	wg.Wait()

	assert.Zero(t, writeErrs, "log inserts failed with SQLITE_BUSY; busy_timeout not effective")
	assert.Zero(t, readErrs, "reads failed with SQLITE_BUSY; busy_timeout not effective")
}

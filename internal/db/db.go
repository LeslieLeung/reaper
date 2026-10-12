package db

import (
	"context"
	"database/sql"
	"errors"
	_ "modernc.org/sqlite" // pure-Go SQLite driver, no CGo required
)

const componentSchema = `
	CREATE TABLE IF NOT EXISTS execution_components (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		execution_id TEXT NOT NULL,
		component TEXT NOT NULL,
		status TEXT NOT NULL,
		start_time DATETIME,
		end_time DATETIME,
		error_message TEXT,
		UNIQUE(execution_id, component),
		FOREIGN KEY (execution_id) REFERENCES executions(id)
	);

	CREATE INDEX IF NOT EXISTS idx_execution_components_execution
		ON execution_components(execution_id);
`

const executionIndexSchema = `
	CREATE INDEX IF NOT EXISTS idx_executions_repo_start
		ON executions(repo_key, start_time DESC);
	CREATE INDEX IF NOT EXISTS idx_executions_repo_status_end
		ON executions(repo_key, status, end_time DESC);
	CREATE INDEX IF NOT EXISTS idx_executions_status
		ON executions(status);
`

const logRetentionIndexSchema = `
	CREATE INDEX IF NOT EXISTS idx_logs_execution_id ON logs(execution_id, id);
	CREATE INDEX IF NOT EXISTS idx_executions_log_retention ON executions(end_time, id)
		WHERE status IN ('completed', 'failed', 'cancelled') AND end_time IS NOT NULL;
`

type DB struct {
	*sql.DB
	readDB *sql.DB
}

// Queries use a separate pool so WAL readers (including slow log streams) do
// not hold up the single writer. Transactions always use the embedded writer
// pool, keeping all statements in a transaction on the same connection.
func (d *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.readDB.QueryContext(ctx, query, args...)
}

func (d *DB) Query(query string, args ...any) (*sql.Rows, error) {
	return d.QueryContext(context.Background(), query, args...)
}

func (d *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return d.readDB.QueryRowContext(ctx, query, args...)
}

func (d *DB) QueryRow(query string, args ...any) *sql.Row {
	return d.QueryRowContext(context.Background(), query, args...)
}

func (d *DB) Close() error {
	if d.readDB == d.DB {
		return d.DB.Close()
	}
	return errors.Join(d.readDB.Close(), d.DB.Close())
}

func Initialize(path string) (*DB, error) {
	// File-backed SQLite needs WAL journaling plus a busy timeout. With the
	// default rollback journal, a concurrent reader + writer pair (the SSE
	// log-stream poller and the job goroutines INSERTing log lines) reliably
	// collides on the shared journal and the writer fails with "database is
	// locked (SQLITE_BUSY)" — a failure that internal/ui swallows, silently
	// dropping log lines mid-run. WAL lets readers run concurrently with a
	// writer (each sees the last committed snapshot), and busy_timeout makes a
	// colliding writer wait up to 5s instead of failing outright. ":memory:"
	// is untouched: its single pinned connection needs no journaling pragma
	// (and WAL only makes sense on a file anyway).
	dsn := path
	if path != ":memory:" {
		dsn = path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	// SQLite permits only one writer, even in WAL mode. Queue local writes in
	// database/sql instead of letting log inserts and terminal status updates
	// compete until busy_timeout expires. Pool waits respect context deadlines.
	db.SetMaxOpenConns(1)
	d := &DB{DB: db, readDB: db}
	if path != ":memory:" {
		// Keep readers concurrent with the writer. query_only prevents accidental
		// writes through Query/QueryRow from bypassing the serialized writer.
		d.readDB, err = sql.Open("sqlite", dsn+"&_pragma=query_only(1)")
		if err != nil {
			db.Close()
			return nil, err
		}
	}
	// :memory: must share its one connection: each connection has its own DB.

	// Create tables
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS executions (
			id TEXT PRIMARY KEY,
			job_name TEXT NOT NULL,
			repo_key TEXT NOT NULL DEFAULT '',
			start_time DATETIME NOT NULL,
			end_time DATETIME,
			status TEXT NOT NULL,
			error_message TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			execution_id TEXT NOT NULL,
			timestamp DATETIME NOT NULL,
			level TEXT NOT NULL,
			message TEXT NOT NULL,
			FOREIGN KEY (execution_id) REFERENCES executions(id)
		);
	` + componentSchema + logRetentionIndexSchema)
	if err != nil {
		return d, err
	}

	hasRepoKey, err := columnExists(d, "executions", "repo_key")
	if err != nil {
		return d, err
	}
	if hasRepoKey {
		_, err = db.Exec(executionIndexSchema)
	}

	return d, err
}

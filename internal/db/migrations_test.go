package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInitializeCreatesTables verifies that Initialize creates the executions
// and logs tables with the expected columns.
func TestInitializeCreatesTables(t *testing.T) {
	testDB, err := Initialize(":memory:")
	assert.NoError(t, err)
	defer testDB.Close()
	assertComponentSchemaObjects(t, testDB)
	require.NoError(t, Migrate(testDB))
	require.NoError(t, Migrate(testDB))
	assertComponentSchemaObjects(t, testDB)

	// executions table should exist and accept inserts
	_, err = testDB.Exec(`INSERT INTO executions (id, job_name, start_time, status) VALUES (?, ?, ?, ?)`,
		"exec-1", "test-job", time.Now(), "running")
	assert.NoError(t, err)

	var count int
	err = testDB.QueryRow("SELECT COUNT(*) FROM executions").Scan(&count)
	assert.NoError(t, err)
	assert.Equal(t, 1, count)

	// logs table should exist and accept inserts with a foreign key to executions
	_, err = testDB.Exec(`INSERT INTO logs (execution_id, timestamp, level, message) VALUES (?, ?, ?, ?)`,
		"exec-1", time.Now(), "info", "Test log message")
	assert.NoError(t, err)

	err = testDB.QueryRow("SELECT COUNT(*) FROM logs").Scan(&count)
	assert.NoError(t, err)
	assert.Equal(t, 1, count)
}

// TestInitializeIsIdempotent verifies that calling Initialize on an existing
// database does not error (CREATE TABLE IF NOT EXISTS).
func TestInitializeIsIdempotent(t *testing.T) {
	testDB, err := Initialize(":memory:")
	assert.NoError(t, err)

	// Re-running the same schema statement should not fail
	_, err = testDB.Exec(`
		CREATE TABLE IF NOT EXISTS executions (
			id TEXT PRIMARY KEY,
			job_name TEXT NOT NULL,
			start_time DATETIME NOT NULL,
			end_time DATETIME,
			status TEXT NOT NULL,
			error_message TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`)
	assert.NoError(t, err)
}

// TestMigrateAddsRepoKeyColumn verifies Migrate upgrades a legacy executions
// table (no repo_key) in place without backfilling, and that new rows can
// write repo_key.
func TestMigrateAddsRepoKeyColumn(t *testing.T) {
	testDB, err := Initialize(":memory:")
	assert.NoError(t, err)
	defer testDB.Close()

	// Rebuild executions in the pre-migration shape (no repo_key).
	_, err = testDB.Exec(`DROP TABLE executions`)
	assert.NoError(t, err)
	_, err = testDB.Exec(`
		CREATE TABLE executions (
			id TEXT PRIMARY KEY,
			job_name TEXT NOT NULL,
			start_time DATETIME NOT NULL,
			end_time DATETIME,
			status TEXT NOT NULL,
			error_message TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`)
	assert.NoError(t, err)

	// A pre-existing legacy row.
	_, err = testDB.Exec(`INSERT INTO executions (id, job_name, start_time, status) VALUES (?, ?, ?, ?)`,
		"old", "repo-a", time.Now(), "completed")
	assert.NoError(t, err)

	require.NoError(t, Migrate(testDB))
	require.NoError(t, Migrate(testDB))
	assertComponentSchemaObjects(t, testDB)

	// Column now exists; legacy row's key stays empty (no backfill).
	var key string
	err = testDB.QueryRow(`SELECT repo_key FROM executions WHERE id = 'old'`).Scan(&key)
	assert.NoError(t, err)
	assert.Equal(t, "", key)

	// New rows can write repo_key.
	_, err = testDB.Exec(`INSERT INTO executions (id, job_name, repo_key, start_time, status) VALUES (?, ?, ?, ?, ?)`,
		"new", "repo-b", "github.com/b/b", time.Now(), "running")
	assert.NoError(t, err)
}

func TestInitializeThenMigrateUpgradesLegacyFileDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacyDB, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = legacyDB.Exec(`
		CREATE TABLE executions (
			id TEXT PRIMARY KEY,
			job_name TEXT NOT NULL,
			start_time DATETIME NOT NULL,
			end_time DATETIME,
			status TEXT NOT NULL,
			error_message TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`)
	require.NoError(t, err)
	require.NoError(t, legacyDB.Close())

	testDB, err := Initialize(path)
	if testDB != nil {
		defer testDB.Close()
	}
	require.NoError(t, err)
	require.NoError(t, Migrate(testDB))
	require.NoError(t, Migrate(testDB))
	assertComponentSchemaObjects(t, testDB)
}

// TestMigrateIsIdempotent verifies Migrate on a fresh (already current) DB is a no-op.
func TestMigrateIsIdempotent(t *testing.T) {
	testDB, err := Initialize(":memory:")
	assert.NoError(t, err)
	defer testDB.Close()

	assert.NoError(t, Migrate(testDB))
	assert.NoError(t, Migrate(testDB))
}

func assertComponentSchemaObjects(t *testing.T, testDB *DB) {
	t.Helper()
	for _, name := range []string{
		"execution_components",
		"idx_executions_repo_start",
		"idx_executions_repo_status_end",
		"idx_executions_status",
		"idx_execution_components_execution",
		"idx_logs_execution_id",
		"idx_executions_log_retention",
	} {
		var got string
		require.NoError(t, testDB.QueryRow(
			`SELECT name FROM sqlite_master WHERE name = ?`, name,
		).Scan(&got))
		require.Equal(t, name, got)
	}
}

package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
	"github.com/wnarutou/gitrieve/internal/db"
	internalserver "github.com/wnarutou/gitrieve/internal/server"
)

func TestServerCleansLogsAtStartupUsingConfiguredRetention(t *testing.T) {
	viper.Set("server.logRetentionMonths", 6)
	t.Cleanup(func() { viper.Set("server.logRetentionMonths", nil) })
	d, err := db.Initialize(internalserver.GetServerConfig().DbPath)
	require.NoError(t, err)
	prefix := fmt.Sprintf("retention-%d-", time.Now().UnixNano())
	insertRetentionTask(t, d, prefix+"expired", time.Now().AddDate(0, -7, 0))
	insertRetentionTask(t, d, prefix+"retained", time.Now().AddDate(0, -3, 0))
	require.NoError(t, d.Close())
	s := NewServer(nil)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.Eventually(t, func() bool {
		var count int
		err := s.database.QueryRow(`SELECT COUNT(*) FROM logs WHERE execution_id = ?`, prefix+"expired").Scan(&count)
		return err == nil && count == 0
	}, time.Second, 10*time.Millisecond)
	var count int
	require.NoError(t, s.database.QueryRow(`SELECT COUNT(*) FROM logs WHERE execution_id = ?`, prefix+"retained").Scan(&count))
	require.Equal(t, 1, count)
}

func TestServerLogCleanupRepeatsAndStopsBeforeDatabaseClose(t *testing.T) {
	d, err := db.Initialize(":memory:")
	require.NoError(t, err)
	s := &Server{database: d}
	s.startLogCleanup(10 * time.Millisecond)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	insertRetentionTask(t, d, "expired-later", time.Now().AddDate(0, -3, 0))
	require.Eventually(t, func() bool {
		var count int
		err := d.QueryRow(`SELECT COUNT(*) FROM logs WHERE execution_id = 'expired-later'`).Scan(&count)
		return err == nil && count == 0
	}, time.Second, 10*time.Millisecond)
	// A later insert must also be cleaned; the first pass alone is insufficient.
	insertRetentionTask(t, d, "expired-next-pass", time.Now().AddDate(0, -3, 0))
	require.Eventually(t, func() bool {
		var count int
		err := d.QueryRow(`SELECT COUNT(*) FROM logs WHERE execution_id = 'expired-next-pass'`).Scan(&count)
		return err == nil && count == 0
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, s.Close())
	select {
	case <-s.logCleanupDone:
	default:
		t.Fatal("cleanup worker still running after database close")
	}
	require.Error(t, d.PingContext(context.Background()))
}

func insertRetentionTask(t *testing.T, d *db.DB, id string, end time.Time) {
	t.Helper()
	_, err := d.Exec(`INSERT INTO executions (id, job_name, start_time, end_time, status)
		VALUES (?, 'retention-test', ?, ?, 'completed')`, id, end.Add(-time.Hour), end)
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO logs (execution_id, timestamp, level, message)
		VALUES (?, '2020-01-01 00:00:00', 'info', 'line')`, id)
	require.NoError(t, err)
}

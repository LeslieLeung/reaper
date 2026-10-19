package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
	"github.com/wnarutou/gitrieve/internal/config"
	"github.com/wnarutou/gitrieve/internal/executor"
	"github.com/wnarutou/gitrieve/internal/typedef"
)

func TestMaintenanceRoutesAndAdmission(t *testing.T) {
	s := NewServer(&config.Config{Repository: []typedef.Repository{{URL: "github.com/test/maintenance"}}})
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	request := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		s.ServeHTTP(w, r)
		return w
	}
	get := request(http.MethodGet, "/api/maintenance", "")
	require.Equal(t, http.StatusOK, get.Code)
	enter := request(http.MethodPost, "/api/maintenance", "")
	require.Equal(t, http.StatusOK, enter.Code)
	var response struct {
		Data executor.MaintenanceStatus `json:"data"`
	}
	require.NoError(t, json.Unmarshal(enter.Body.Bytes(), &response))
	require.True(t, response.Data.Enabled)
	require.True(t, response.Data.Ready)
	for _, path := range []string{"/api/jobs", "/api/jobs/bulk"} {
		rejected := request(http.MethodPost, path, `{"repository_key":"github.com/test/maintenance"}`)
		require.Equal(t, http.StatusServiceUnavailable, rejected.Code)
	}
	exit := request(http.MethodDelete, "/api/maintenance", "")
	require.Equal(t, http.StatusOK, exit.Code)
	require.False(t, s.executor.MaintenanceStatus().Enabled)
	var count int
	require.NoError(t, s.database.QueryRow("SELECT COUNT(*) FROM executions WHERE repo_key = ?", "github.com/test/maintenance").Scan(&count))
	require.Zero(t, count, "leaving maintenance must not trigger new executions")
}

func TestMaintenanceRoutesRequireConfiguredAuthentication(t *testing.T) {
	oldEnabled, oldToken := viper.Get("server.authEnabled"), viper.Get("server.authToken")
	viper.Set("server.authEnabled", true)
	viper.Set("server.authToken", "maintenance-test-token")
	defer viper.Set("server.authEnabled", oldEnabled)
	defer viper.Set("server.authToken", oldToken)
	s := NewServer(nil)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, "/api/maintenance", nil)
		s.ServeHTTP(w, r)
		require.Equal(t, http.StatusUnauthorized, w.Code)
		w = httptest.NewRecorder()
		r.Header.Set("Authorization", "Bearer maintenance-test-token")
		s.ServeHTTP(w, r)
		require.Equal(t, http.StatusOK, w.Code)
	}
}

func TestServerShutdownWaitsForSafeBoundary(t *testing.T) {
	s := NewServer(nil)
	require.NoError(t, s.executor.Close())
	entered, release := make(chan struct{}), make(chan struct{})
	cfg := &config.Config{Repository: []typedef.Repository{{URL: "github.com/test/shutdown"}}}
	s.executor = executor.NewExecutorWithRunners(nil, s.database, cfg, executor.Runners{Code: func(ctx context.Context, _ typedef.Repository, _ []typedef.MultiStorage) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	_, err := s.executor.ExecuteJob("github.com/test/shutdown")
	require.NoError(t, err)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("job did not start")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.serve(ctx, listener) }()
	cancel()
	require.Eventually(t, func() bool { return s.executor.MaintenanceStatus().Enabled }, 3*time.Second, time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("shutdown interrupted active page: %v", err)
	default:
	}
	require.Error(t, s.executor.ExitMaintenance(), "shutdown cannot be reversed through API")
	close(release)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not finish")
	}
}

package monitoring

import (
	"net/http"
	"runtime"
	"time"

	"github.com/gin-gonic/gin"
)

// Monitor tracks server start time and exposes health/metrics endpoints.
type Monitor struct {
	startTime time.Time
}

// NewMonitor creates a new Monitor with the current time as the start time.
func NewMonitor() *Monitor {
	return &Monitor{startTime: time.Now()}
}

// HealthCheck handles GET /health, returning a simple liveness response.
func (m *Monitor) HealthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"code": http.StatusOK,
		"data": gin.H{
			"status":    "ok",
			"uptime":    formatUptime(time.Since(m.startTime)),
			"timestamp": time.Now().Format(time.RFC3339),
		},
	})
}

// GetMetrics handles GET /api/metrics, returning uptime, runtime, and Go
// version information.
func (m *Monitor) GetMetrics(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"code": http.StatusOK,
		"data": gin.H{
			"uptime":     formatUptime(time.Since(m.startTime)),
			"timestamp":  time.Now().Format(time.RFC3339),
			"goroutines": runtime.NumGoroutine(),
			"go_version": runtime.Version(),
		},
	})
}

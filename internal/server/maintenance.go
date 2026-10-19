package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func (a *API) GetMaintenance(c *gin.Context) {
	if a.executor == nil {
		c.JSON(http.StatusServiceUnavailable, Response{Code: 503, Message: "Executor is unavailable"})
		return
	}
	c.JSON(http.StatusOK, Response{Code: 200, Data: a.executor.MaintenanceStatus()})
}

func (a *API) EnterMaintenance(c *gin.Context) {
	if a.executor == nil {
		c.JSON(http.StatusServiceUnavailable, Response{Code: 503, Message: "Executor is unavailable"})
		return
	}
	c.JSON(http.StatusOK, Response{Code: 200, Data: a.executor.EnterMaintenance()})
}

func (a *API) ExitMaintenance(c *gin.Context) {
	if a.executor == nil {
		c.JSON(http.StatusServiceUnavailable, Response{Code: 503, Message: "Executor is unavailable"})
		return
	}
	if err := a.executor.ExitMaintenance(); err != nil {
		c.JSON(http.StatusConflict, Response{Code: 409, Message: err.Error(), Data: a.executor.MaintenanceStatus()})
		return
	}
	c.JSON(http.StatusOK, Response{Code: 200, Data: a.executor.MaintenanceStatus()})
}

func (a *API) rejectMaintenance(c *gin.Context) bool {
	if a.executor != nil && a.executor.MaintenanceStatus().Enabled {
		c.JSON(http.StatusServiceUnavailable, Response{Code: 503, Message: "Server is in maintenance mode; new jobs are disabled"})
		return true
	}
	return false
}

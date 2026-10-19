package server

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-co-op/gocron/v2"
	"github.com/spf13/cobra"
	"github.com/wnarutou/gitrieve/internal/auth"
	"github.com/wnarutou/gitrieve/internal/config"
	"github.com/wnarutou/gitrieve/internal/db"
	"github.com/wnarutou/gitrieve/internal/executor"
	"github.com/wnarutou/gitrieve/internal/logger"
	"github.com/wnarutou/gitrieve/internal/monitoring"
	internalserver "github.com/wnarutou/gitrieve/internal/server"
	"github.com/wnarutou/gitrieve/internal/ui"
	"github.com/wnarutou/gitrieve/web"
)

type Server struct {
	router      *gin.Engine
	scheduler   gocron.Scheduler
	database    *db.DB
	executor    *executor.Executor
	schedulerMu sync.Mutex
}

func NewServer(cfg *config.Config) *Server {
	s := &Server{
		router: gin.Default(),
	}
	s.setupRoutes(cfg)
	return s
}

func NewTestServer(db *db.DB) *Server {
	s := &Server{
		router: gin.Default(),
	}
	s.setupTestRoutes(db)
	return s
}

func (s *Server) setupRoutes(cfg *config.Config) {
	// Initialize database — path is configurable via `server.dbPath` so the
	// SQLite file (which also holds job history and logs) can be placed on a
	// mounted volume for persistence, backup and migration.
	serverCfg := internalserver.GetServerConfig()
	database, err := db.Initialize(serverCfg.DbPath)
	if err != nil {
		ui.ErrorfExit("Failed to initialize database: %s", err)
	}
	s.database = database

	// 迁移旧库（新增 repo_key 列）。失败宁可起不来，也不在坏 schema 上跑。
	if err := db.Migrate(database); err != nil {
		ui.ErrorfExit("Failed to migrate database: %s", err)
	}
	if err := database.ReconcileInterrupted(context.Background(), time.Now()); err != nil {
		ui.ErrorfExit("Failed to reconcile interrupted executions: %s", err)
	}

	// Initialize logger
	log := logger.NewLogger(database)

	// Initialize executor
	exec := executor.NewExecutor(log, database, cfg)
	s.executor = exec

	// Initialize API
	api := internalserver.NewAPI(cfg, database, exec)
	api.SetScheduleRefresher(s)
	if err := s.RefreshSchedules(cfg); err != nil {
		ui.Errorf("Failed to schedule one or more repositories: %s", err)
	}

	// Static files and templates are embedded in the binary (see package web),
	// so the server works from any working directory.
	tmpl, err := template.ParseFS(web.TemplatesFS, "templates/*")
	if err != nil {
		ui.ErrorfExit("Failed to load templates: %s", err)
	}
	s.router.SetHTMLTemplate(tmpl)
	s.router.StaticFS("/static", http.FS(web.StaticFS))

	// Main page (public)
	s.router.GET("/", func(c *gin.Context) {
		c.HTML(200, "index.html", gin.H{
			"title": "Gitrieve",
		})
	})

	// Monitoring: health check is always public
	monitor := monitoring.NewMonitor()
	s.router.GET("/health", monitor.HealthCheck)

	// API routes — protected by auth when enabled
	var apiGroup *gin.RouterGroup
	if serverCfg.AuthEnabled && serverCfg.AuthToken != "" {
		authMW := auth.NewAuthMiddleware(serverCfg.AuthToken)
		apiGroup = s.router.Group("/", authMW.Middleware())
	} else {
		apiGroup = s.router.Group("/")
	}

	apiGroup.GET("/api/maintenance", api.GetMaintenance)
	apiGroup.POST("/api/maintenance", api.EnterMaintenance)
	apiGroup.DELETE("/api/maintenance", api.ExitMaintenance)
	apiGroup.POST("/api/jobs", api.CreateJob)
	apiGroup.POST("/api/jobs/bulk", api.BulkCreateJobs)
	apiGroup.DELETE("/api/jobs/:id", api.CancelJob)
	apiGroup.GET("/api/jobs", api.GetJobs)
	apiGroup.GET("/api/jobs/:id/logs", api.GetJobLogs)
	apiGroup.GET("/api/jobs/:id/components", api.GetJobComponents)
	apiGroup.GET("/api/repositories", api.GetRepositories)
	apiGroup.POST("/api/repositories", api.CreateRepository)
	// *id catch-all: the identity key is a URL like github.com/owner/repo and
	// contains "/", so a single-segment :id param cannot match it. gin prefixes
	// the captured value with "/" (trimmed inside the handlers).
	apiGroup.PUT("/api/repositories/*id", api.UpdateRepository)
	apiGroup.DELETE("/api/repositories/*id", api.DeleteRepository)
	apiGroup.GET("/api/storage", api.GetStorages)
	apiGroup.POST("/api/storage", api.CreateStorage)
	apiGroup.PUT("/api/storage/:id", api.UpdateStorage)
	apiGroup.DELETE("/api/storage/:id", api.DeleteStorage)
	apiGroup.GET("/api/config/export", api.ExportConfig)
	apiGroup.POST("/api/config/import/preview", api.PreviewImport)
	apiGroup.POST("/api/config/import", api.ApplyImport)
	apiGroup.POST("/api/config/reload", api.ReloadConfig)
	apiGroup.GET("/api/metrics", monitor.GetMetrics)
}

func (s *Server) setupTestRoutes(db *db.DB) {
	s.router.POST("/api/jobs/bulk", func(c *gin.Context) {
		api := internalserver.NewAPI(&config.Config{}, db, nil)
		api.BulkCreateJobs(c)
	})
	s.router.GET("/api/jobs", func(c *gin.Context) {
		api := internalserver.NewAPI(&config.Config{}, db, nil)
		api.GetJobs(c)
	})
	s.router.GET("/api/jobs/:id/components", func(c *gin.Context) {
		api := internalserver.NewAPI(&config.Config{}, db, nil)
		api.GetJobComponents(c)
	})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

// RefreshSchedules atomically replaces the server's cron registrations with
// the schedules in cfg. Valid repositories remain scheduled even when another
// repository contains an invalid cron expression.
func (s *Server) RefreshSchedules(cfg *config.Config) error {
	s.schedulerMu.Lock()
	defer s.schedulerMu.Unlock()

	concurrency := uint(3)
	if cfg != nil && cfg.ConcurrencyNum > 0 {
		concurrency = cfg.ConcurrencyNum
	}
	next, err := gocron.NewScheduler(
		gocron.WithLocation(time.Local),
		gocron.WithLimitConcurrentJobs(concurrency, gocron.LimitModeWait),
	)
	if err != nil {
		return fmt.Errorf("create scheduler: %w", err)
	}

	var scheduleErrs []error
	if cfg != nil {
		for _, repo := range cfg.Repository {
			if repo.Cron == "" {
				continue
			}
			repoKey := repo.Key()
			_, scheduleErr := next.NewJob(
				gocron.CronJob(repo.Cron, false),
				gocron.NewTask(func() {
					if _, executeErr := s.executor.ExecuteJob(repoKey); executeErr != nil && !errors.Is(executeErr, executor.ErrMaintenance) {
						ui.Errorf("Failed to execute scheduled repository %s: %s", repoKey, executeErr)
					}
				}),
			)
			if scheduleErr != nil {
				scheduleErrs = append(scheduleErrs, fmt.Errorf("repository %s: %w", repoKey, scheduleErr))
			}
		}
	}

	var shutdownErr error
	if s.scheduler != nil {
		shutdownErr = s.scheduler.Shutdown()
	}
	next.Start()
	s.scheduler = next
	return errors.Join(append(scheduleErrs, shutdownErr)...)
}

// Close releases the scheduler and database resources owned by the server.
func (s *Server) Close() error {
	s.schedulerMu.Lock()
	defer s.schedulerMu.Unlock()

	var schedulerErr, executorErr, databaseErr error
	if s.scheduler != nil {
		schedulerErr = s.scheduler.Shutdown()
	}
	if s.executor != nil {
		executorErr = s.executor.Close()
	}
	if s.database != nil {
		databaseErr = s.database.Close()
	}
	return errors.Join(schedulerErr, executorErr, databaseErr)
}

var Cmd = &cobra.Command{
	Use:   "server",
	Short: "start web server",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.GetIns()
		s := NewServer(cfg)
		defer func() {
			if err := s.Close(); err != nil {
				ui.Errorf("Failed to close server resources: %s", err)
			}
		}()
		serverCfg := internalserver.GetServerConfig()
		addr := fmt.Sprintf("%s:%s", serverCfg.Host, serverCfg.Port)
		ui.Printf("Starting server on %s", addr)
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		return s.serve(ctx, listener)
	},
}

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mxschmitt/pg-backup-scheduler/internal/config"
	"github.com/mxschmitt/pg-backup-scheduler/internal/service"
	"go.uber.org/zap"
)

type Server struct {
	config     *config.Config
	service    *service.Service
	logger     *zap.Logger
	httpServer *http.Server
}

func New(cfg *config.Config, svc *service.Service, logger *zap.Logger) *Server {
	s := &Server{
		config:  cfg,
		service: svc,
		logger:  logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/readyz", s.handleReady)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/run", s.handleRun)
	mux.HandleFunc("/run/", s.handleRunProject)
	mux.HandleFunc("/", s.handleRoot)

	s.httpServer = &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	return s
}

func (s *Server) Start() error {
	addr := fmt.Sprintf("0.0.0.0:%d", s.config.ServicePort)
	s.httpServer.Addr = addr
	s.logger.Info("API server listening", zap.String("address", addr))
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("HTTP server failed: %w", err)
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	if err := s.httpServer.Shutdown(ctx); err != nil {
		return fmt.Errorf("failed to shutdown HTTP server: %w", err)
	}
	return nil
}

// Handler returns the HTTP handler for testing purposes
func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.jsonResponse(w, map[string]interface{}{
		"status":    "healthy",
		"timestamp": time.Now().Format(time.RFC3339),
	})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	running, err := s.service.GetRunning()
	if err != nil {
		s.errorResponse(w, "Failed to get running status", http.StatusInternalServerError)
		return
	}

	s.jsonResponse(w, map[string]interface{}{
		"status":    "ready",
		"running":   running,
		"timestamp": time.Now().Format(time.RFC3339),
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	running, err := s.service.GetRunning()
	if err != nil {
		s.errorResponse(w, "Failed to get running status", http.StatusInternalServerError)
		return
	}

	lastRun, err := s.service.GetLastRun()
	if err != nil {
		s.logger.Warn("Failed to get last run", zap.Error(err))
	}

	databases := s.service.GetDatabases()
	dbNames := make([]string, len(databases))
	for i, db := range databases {
		dbNames[i] = db.Identifier
	}

	statusData := map[string]interface{}{
		"databases_configured": len(databases),
		"database_names":       dbNames,
		"currently_running":    running,
		"scheduler_cron":       s.config.BackupCron,
		"timezone":             s.config.TZ,
	}

	if lastRun == nil {
		statusData["status"] = "no_runs_yet"
		statusData["message"] = "No backup runs have been executed yet"
		statusData["last_run"] = nil
	} else {
		statusData["last_run"] = lastRun
	}

	s.jsonResponse(w, statusData)
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.errorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	running, err := s.service.GetRunning()
	if err != nil {
		s.errorResponse(w, "Failed to get running status", http.StatusInternalServerError)
		return
	}

	if running {
		w.WriteHeader(http.StatusConflict)
		s.jsonResponse(w, map[string]interface{}{
			"detail": "Backup job is already running",
		})
		return
	}

	// Run backup in background
	go func() {
		ctx := context.Background()
		if _, err := s.service.RunBackupJob(ctx); err != nil {
			s.logger.Error("Background backup job failed", zap.Error(err))
		}
	}()

	s.jsonResponse(w, map[string]interface{}{
		"status":    "accepted",
		"message":   "Backup job started in background",
		"timestamp": time.Now().Format(time.RFC3339),
	})
}

func (s *Server) handleRunProject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.errorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract project ID from path: /run/{project}
	projectID := strings.TrimPrefix(r.URL.Path, "/run/")
	if projectID == "" {
		s.errorResponse(w, "Project ID is required", http.StatusBadRequest)
		return
	}

	// Run backup in background
	go func() {
		ctx := context.Background()
		result, err := s.service.RunBackupForProject(ctx, projectID)
		if err != nil {
			s.logger.Error("Project backup failed", zap.String("project", projectID), zap.Error(err))
		} else {
			status, _ := result["status"].(string)
			if status == "failed" {
				errorMsg, _ := result["error"].(string)
				s.logger.Error("Project backup failed", zap.String("project", projectID), zap.String("error", errorMsg))
			} else {
				s.logger.Info("Project backup completed", zap.String("project", projectID), zap.String("status", status))
			}
		}
	}()

	s.jsonResponse(w, map[string]interface{}{
		"status":    "accepted",
		"message":   fmt.Sprintf("Backup started for project: %s", projectID),
		"timestamp": time.Now().Format(time.RFC3339),
	})
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	s.jsonResponse(w, map[string]interface{}{
		"service": "PostgreSQL Backup Service",
		"version": "1.0.0",
		"endpoints": map[string]string{
			"health":          "/healthz",
			"readiness":       "/readyz",
			"status":          "/status",
			"trigger_all":     "/run (POST)",
			"trigger_project": "/run/{project} (POST)",
		},
	})
}

func (s *Server) jsonResponse(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		s.logger.Error("Failed to encode JSON response", zap.Error(err))
	}
}

func (s *Server) errorResponse(w http.ResponseWriter, message string, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": message,
	})
}

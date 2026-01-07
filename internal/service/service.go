package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mxschmitt/pg-backup-scheduler/internal/backup"
	"github.com/mxschmitt/pg-backup-scheduler/internal/config"
	"github.com/mxschmitt/pg-backup-scheduler/internal/database"
	"github.com/mxschmitt/pg-backup-scheduler/internal/docker"
	"github.com/mxschmitt/pg-backup-scheduler/internal/metadata"
	"github.com/mxschmitt/pg-backup-scheduler/internal/retention"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
)

type Service struct {
	config       *config.Config
	logger       *zap.Logger
	backupRunner *backup.BackupRunner
	baseDir      string
	databases    []*database.Database
	cron         *cron.Cron
}

func New(ctx context.Context, cfg *config.Config, logger *zap.Logger) (*Service, error) {
	// Initialize Docker client
	if _, err := docker.Init(); err != nil {
		return nil, fmt.Errorf("failed to initialize Docker client: %w", err)
	}

	// Check Docker availability
	if err := docker.CheckDocker(ctx); err != nil {
		return nil, fmt.Errorf("Docker check failed: %w", err)
	}

	// Ensure base directory exists
	if err := os.MkdirAll(cfg.LocalBackupDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create backup directory: %w", err)
	}

	// Parse databases
	var databases []*database.Database
	for projectName, connURL := range cfg.Databases {
		db, err := database.New(connURL, projectName)
		if err != nil {
			logger.Warn("Failed to parse database config", zap.String("project", projectName), zap.Error(err))
			continue
		}
		databases = append(databases, db)
	}

	if len(databases) == 0 {
		logger.Warn("No databases configured. Set environment variables like BACKUP_PROJECTNAME=postgresql://...")
	} else {
		logger.Info("Configured databases for backup", zap.Int("count", len(databases)))
	}

	s := &Service{
		config:       cfg,
		logger:       logger,
		backupRunner: backup.New(logger),
		baseDir:      cfg.LocalBackupDir,
		databases:    databases,
	}

	// Setup scheduler
	if err := s.setupScheduler(); err != nil {
		return nil, fmt.Errorf("failed to setup scheduler: %w", err)
	}

	return s, nil
}

func (s *Service) setupScheduler() error {
	// robfig/cron/v3 expects 5 fields: minute hour day month weekday
	// User input format: "30 0 * * *" (minute hour day month weekday)
	// We need to parse it correctly
	cronExpr := s.config.BackupCron

	// Remove seconds field if present (6 fields -> 5 fields)
	parts := strings.Fields(cronExpr)
	if len(parts) == 6 {
		// Remove first field (seconds)
		cronExpr = strings.Join(parts[1:], " ")
	}

	loc, err := time.LoadLocation(s.config.TZ)
	if err != nil {
		s.logger.Warn("Invalid timezone, using UTC", zap.String("tz", s.config.TZ), zap.Error(err))
		loc = time.UTC
	}

	c := cron.New(cron.WithLocation(loc))
	_, err = c.AddFunc(cronExpr, func() {
		ctx := context.Background()
		if _, err := s.RunBackupJob(ctx); err != nil {
			s.logger.Error("Scheduled backup job failed", zap.Error(err))
		}
	})
	if err != nil {
		return fmt.Errorf("invalid cron expression: %w", err)
	}

	c.Start()
	s.cron = c

	s.logger.Info("Scheduled daily backups",
		zap.String("cron", cronExpr),
		zap.String("timezone", s.config.TZ))

	return nil
}

func (s *Service) RunBackupJob(ctx context.Context) (map[string]interface{}, error) {
	// Check if already running
	status, err := metadata.ReadServiceStatus(s.baseDir)
	if err != nil {
		s.logger.Warn("Failed to read service status", zap.Error(err))
	}

	if status.Running {
		s.logger.Warn("Backup job already running, skipping")
		return map[string]interface{}{
			"status": "failed",
			"error":  "already_running",
		}, nil
	}

	// Mark as running
	runStarted := time.Now()
	runID := fmt.Sprintf("run-%s", runStarted.Format("20060102-150405"))

	if err := metadata.WriteServiceStatus(s.baseDir, &metadata.ServiceStatus{Running: true}); err != nil {
		s.logger.Warn("Failed to write service status", zap.Error(err))
	}

	defer func() {
		_ = metadata.WriteServiceStatus(s.baseDir, &metadata.ServiceStatus{Running: false})
	}()

	s.logger.Info("Starting backup job", zap.String("run_id", runID))

	result := map[string]interface{}{
		"run_id":     runID,
		"started_at": runStarted.Format(time.RFC3339),
		"status":     "failed",
		"backups":    []interface{}{},
	}

	if len(s.databases) == 0 {
		result["error"] = "No databases configured"
		result["finished_at"] = time.Now().Format(time.RFC3339)
		result["duration_ms"] = 0
		_ = metadata.WriteLastRun(s.baseDir, result)
		return result, nil
	}

	// Run backups
	backupDate := time.Now().Format("2006-01-02")
	var backupResults []interface{}
	succeeded := 0
	failed := 0

	// Create temp base directory once for all backups (in baseDir to avoid cross-device link errors)
	tempBaseDir := filepath.Join(s.baseDir, ".tmp")
	if err := os.MkdirAll(tempBaseDir, 0755); err != nil {
		s.logger.Error("Failed to create temp base directory", zap.Error(err))
		result["error"] = fmt.Sprintf("failed to create temp base directory: %v", err)
		result["finished_at"] = time.Now().Format(time.RFC3339)
		result["duration_ms"] = time.Since(runStarted).Milliseconds()
		_ = metadata.WriteLastRun(s.baseDir, result)
		return result, nil
	}

	for _, db := range s.databases {
		s.logger.Info("Backing up database", zap.String("database", db.Identifier))

		tempDir, err := os.MkdirTemp(tempBaseDir, fmt.Sprintf("backup-%s-%s-", db.Identifier, backupDate))
		if err != nil {
			s.logger.Error("Failed to create temp directory", zap.Error(err))
			backupResults = append(backupResults, map[string]interface{}{
				"database_identifier": db.Identifier,
				"status":              "failed",
				"error":               err.Error(),
			})
			failed++
			continue
		}

		manifest, err := s.backupRunner.CreateBackup(ctx, db, tempDir, backupDate)
		if err != nil {
			s.logger.Error("Backup failed", zap.String("database", db.Identifier), zap.Error(err))
			backupResults = append(backupResults, map[string]interface{}{
				"database_identifier": db.Identifier,
				"status":              "failed",
				"error":               err.Error(),
			})
			failed++
			_ = os.RemoveAll(tempDir)
			continue
		}

		if manifest.Status == "success" && len(manifest.Files) > 0 {
			// Move backup files to final location
			backupDir := filepath.Join(s.baseDir, db.Identifier, backupDate)
			if err := os.MkdirAll(backupDir, 0755); err != nil {
				s.logger.Error("Failed to create backup directory", zap.Error(err))
				failed++
				_ = os.RemoveAll(tempDir)
				continue
			}

			// Move archive and manifest
			archiveFile := fmt.Sprintf("backup-%s.tar.gz", manifest.RunID)
			manifestFile := fmt.Sprintf("manifest-%s.json", manifest.RunID)

			srcArchive := filepath.Join(tempDir, archiveFile)
			dstArchive := filepath.Join(backupDir, archiveFile)

			srcManifest := filepath.Join(tempDir, manifestFile)
			dstManifest := filepath.Join(backupDir, manifestFile)

			if _, err := os.Stat(srcArchive); err == nil {
				if err := os.Rename(srcArchive, dstArchive); err != nil {
					s.logger.Warn("Failed to move archive", zap.Error(err))
				}
			}

			if _, err := os.Stat(srcManifest); err == nil {
				if err := os.Rename(srcManifest, dstManifest); err != nil {
					s.logger.Warn("Failed to move manifest", zap.Error(err))
				}
			}
		}

		backupResults = append(backupResults, map[string]interface{}{
			"database_identifier": manifest.DatabaseID,
			"run_id":              manifest.RunID,
			"status":              manifest.Status,
			"error":               manifest.Error,
		})

		if manifest.Status == "success" {
			succeeded++
		} else {
			failed++
		}

		_ = os.RemoveAll(tempDir)
	}

	// Retention cleanup
	var dbIDs []string
	for _, db := range s.databases {
		dbIDs = append(dbIDs, db.Identifier)
	}

	cleanupResults, err := retention.CleanupAllDatabases(s.baseDir, dbIDs, s.config.RetentionDays)
	if err != nil {
		s.logger.Warn("Retention cleanup failed", zap.Error(err))
	}

	runFinished := time.Now()
	durationMs := runFinished.Sub(runStarted).Milliseconds()

	statusStr := "failed"
	if failed == 0 {
		statusStr = "success"
	} else if succeeded > 0 {
		statusStr = "partial"
	}

	result["finished_at"] = runFinished.Format(time.RFC3339)
	result["duration_ms"] = durationMs
	result["status"] = statusStr
	result["databases_total"] = len(s.databases)
	result["databases_succeeded"] = succeeded
	result["databases_failed"] = failed
	result["backups"] = backupResults
	result["retention_cleanup"] = cleanupResults

	if err := metadata.WriteLastRun(s.baseDir, result); err != nil {
		s.logger.Warn("Failed to write last run", zap.Error(err))
	}

	s.logger.Info("Backup job completed",
		zap.String("run_id", runID),
		zap.Int("succeeded", succeeded),
		zap.Int("failed", failed),
		zap.Int64("duration_ms", durationMs))

	return result, nil
}

func (s *Service) GetLastRun() (map[string]interface{}, error) {
	return metadata.ReadLastRun(s.baseDir)
}

func (s *Service) GetRunning() (bool, error) {
	status, err := metadata.ReadServiceStatus(s.baseDir)
	if err != nil {
		return false, err
	}
	return status.Running, nil
}

func (s *Service) GetDatabases() []*database.Database {
	return s.databases
}

func (s *Service) GetDatabase(identifier string) *database.Database {
	for _, db := range s.databases {
		if db.Identifier == identifier {
			return db
		}
	}
	return nil
}

// RunBackupForProject backs up a single project by identifier
func (s *Service) RunBackupForProject(ctx context.Context, projectID string) (map[string]interface{}, error) {
	db := s.GetDatabase(projectID)
	if db == nil {
		return nil, fmt.Errorf("project not found: %s", projectID)
	}

	// Check if a full backup job is already running
	status, err := metadata.ReadServiceStatus(s.baseDir)
	if err != nil {
		s.logger.Warn("Failed to read service status", zap.Error(err))
	}

	if status.Running {
		return nil, fmt.Errorf("backup job is already running")
	}

	backupDate := time.Now().Format("2006-01-02")
	s.logger.Info("Backing up database", zap.String("database", db.Identifier))

	// Create temp directory in baseDir to avoid cross-device link errors
	// (system /tmp is often tmpfs, while baseDir is a mounted volume)
	tempBaseDir := filepath.Join(s.baseDir, ".tmp")
	if err := os.MkdirAll(tempBaseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create temp base directory: %w", err)
	}

	tempDir, err := os.MkdirTemp(tempBaseDir, fmt.Sprintf("backup-%s-%s-", db.Identifier, backupDate))
	if err != nil {
		return nil, fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(tempDir)

	manifest, err := s.backupRunner.CreateBackup(ctx, db, tempDir, backupDate)
	if err != nil {
		return nil, fmt.Errorf("backup failed: %w", err)
	}

	if manifest.Status == "success" && len(manifest.Files) > 0 {
		// Move backup files to final location
		backupDir := filepath.Join(s.baseDir, db.Identifier, backupDate)
		if err := os.MkdirAll(backupDir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create backup directory: %w", err)
		}

		// Move archive and manifest
		archiveFile := fmt.Sprintf("backup-%s.tar.gz", manifest.RunID)
		manifestFile := fmt.Sprintf("manifest-%s.json", manifest.RunID)

		srcArchive := filepath.Join(tempDir, archiveFile)
		dstArchive := filepath.Join(backupDir, archiveFile)

		srcManifest := filepath.Join(tempDir, manifestFile)
		dstManifest := filepath.Join(backupDir, manifestFile)

		if _, err := os.Stat(srcArchive); err == nil {
			if err := os.Rename(srcArchive, dstArchive); err != nil {
				s.logger.Warn("Failed to move archive", zap.Error(err))
			}
		}

		if _, err := os.Stat(srcManifest); err == nil {
			if err := os.Rename(srcManifest, dstManifest); err != nil {
				s.logger.Warn("Failed to move manifest", zap.Error(err))
			}
		}
	}

	result := map[string]interface{}{
		"database_identifier": manifest.DatabaseID,
		"run_id":              manifest.RunID,
		"status":              manifest.Status,
		"started_at":          manifest.StartedAt,
		"finished_at":         manifest.FinishedAt,
		"duration_ms":         manifest.DurationMs,
	}

	if manifest.Error != "" {
		result["error"] = manifest.Error
	}

	return result, nil
}

func (s *Service) Shutdown(ctx context.Context) error {
	if s.cron != nil {
		cronCtx := s.cron.Stop()
		select {
		case <-cronCtx.Done():
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

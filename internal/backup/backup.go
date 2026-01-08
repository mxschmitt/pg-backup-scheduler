package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mxschmitt/pg-backup-scheduler/internal/database"
	"github.com/mxschmitt/pg-backup-scheduler/internal/docker"
	"go.uber.org/zap"

	"github.com/docker/docker/api/types/container"
)

const (
	dbConnectionTimeout = 30 * time.Second
)

type BackupRunner struct {
	logger *zap.Logger
}

func New(logger *zap.Logger) *BackupRunner {
	return &BackupRunner{
		logger: logger,
	}
}

type BackupManifest struct {
	RunID             string `json:"run_id"`
	DatabaseID        string `json:"database_identifier"`
	StartedAt         string `json:"started_at"`
	FinishedAt        string `json:"finished_at"`
	DurationMs        int64  `json:"duration_ms"`
	Status            string `json:"status"`
	Files             []File `json:"files"`
	Error             string `json:"error,omitempty"`
	PGVersion         string `json:"pg_version,omitempty"`
	DatabaseSizeBytes *int64 `json:"database_size_bytes,omitempty"`
}

type File struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

func (br *BackupRunner) CreateBackup(ctx context.Context, db *database.Database, outputDir, backupDate string) (*BackupManifest, error) {
	startedAt := br.now()
	runID := fmt.Sprintf("%s-%s-%s", db.Identifier, backupDate, startedAt.Format("150405"))

	br.logger.Info("Starting backup", zap.String("database", db.Identifier))

	// Detect PostgreSQL version
	pgVersion, err := br.detectVersion(ctx, db.ConnectionURL)
	if err != nil {
		br.logger.Warn("Failed to detect PostgreSQL version, defaulting to 17", zap.Error(err))
		pgVersion = "17"
	} else {
		br.logger.Debug("Detected PostgreSQL version", zap.String("version", pgVersion))
	}

	// Collect metrics
	metrics, err := br.collectMetrics(ctx, db.ConnectionURL)
	if err != nil {
		br.logger.Warn("Failed to collect metrics", zap.Error(err))
	}

	// Create temp directory for dumps
	tempDir := filepath.Join(outputDir, runID)
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create temp directory: %w", err)
	}

	var files []string

	// 1. Dump roles
	rolesFile := filepath.Join(tempDir, "roles.sql")
	if err := br.dumpRoles(ctx, db.ConnectionURL, rolesFile, pgVersion); err != nil {
		br.logger.Error("Roles dump failed", zap.String("database", db.Identifier), zap.Error(err))
		return br.createFailedManifest(runID, db.Identifier, startedAt, fmt.Errorf("roles dump failed: %w", err))
	}
	files = append(files, rolesFile)

	// 2. Dump schema
	schemaFile := filepath.Join(tempDir, "schema.sql")
	if err := br.dumpSchema(ctx, db.ConnectionURL, schemaFile, pgVersion); err != nil {
		br.logger.Error("Schema dump failed", zap.String("database", db.Identifier), zap.Error(err))
		return br.createFailedManifest(runID, db.Identifier, startedAt, fmt.Errorf("schema dump failed: %w", err))
	}
	files = append(files, schemaFile)

	// 3. Dump data
	dataFile := filepath.Join(tempDir, "data.sql")
	if err := br.dumpData(ctx, db.ConnectionURL, dataFile, pgVersion); err != nil {
		br.logger.Error("Data dump failed", zap.String("database", db.Identifier), zap.Error(err))
		return br.createFailedManifest(runID, db.Identifier, startedAt, fmt.Errorf("data dump failed: %w", err))
	}
	files = append(files, dataFile)

	// Create archive
	archivePath := filepath.Join(outputDir, fmt.Sprintf("backup-%s.tar.gz", runID))
	if err := br.createArchive(files, archivePath, tempDir); err != nil {
		return br.createFailedManifest(runID, db.Identifier, startedAt, fmt.Errorf("archive creation failed: %w", err))
	}

	finishedAt := br.now()
	durationMs := finishedAt.Sub(startedAt).Milliseconds()

	archiveInfo, err := os.Stat(archivePath)
	if err != nil {
		return br.createFailedManifest(runID, db.Identifier, startedAt, fmt.Errorf("failed to stat archive: %w", err))
	}

	manifest := &BackupManifest{
		RunID:      runID,
		DatabaseID: db.Identifier,
		StartedAt:  startedAt.Format("2006-01-02T15:04:05Z07:00"),
		FinishedAt: finishedAt.Format("2006-01-02T15:04:05Z07:00"),
		DurationMs: durationMs,
		Status:     "success",
		Files: []File{{
			Name: filepath.Base(archivePath),
			Size: archiveInfo.Size(),
		}},
		PGVersion:         metrics.PGVersion,
		DatabaseSizeBytes: metrics.DatabaseSizeBytes,
	}

	// Save manifest
	manifestPath := filepath.Join(outputDir, fmt.Sprintf("manifest-%s.json", runID))
	if err := br.saveManifest(manifestPath, manifest); err != nil {
		br.logger.Warn("Failed to save manifest", zap.Error(err))
	}

	// Cleanup temp directory
	if err := os.RemoveAll(tempDir); err != nil {
		br.logger.Warn("Failed to cleanup temp directory", zap.Error(err))
	}

	br.logger.Info("Backup completed",
		zap.String("database", db.Identifier),
		zap.Int64("duration_ms", durationMs),
		zap.Int64("size_bytes", archiveInfo.Size()))

	return manifest, nil
}

func (br *BackupRunner) detectVersion(ctx context.Context, connURL string) (string, error) {
	connCtx, cancel := context.WithTimeout(ctx, dbConnectionTimeout)
	defer cancel()

	conn, err := pgx.Connect(connCtx, connURL)
	if err != nil {
		return "", err
	}
	defer conn.Close(context.Background())

	var version string
	err = conn.QueryRow(ctx, "SELECT version()").Scan(&version)
	if err != nil {
		return "", err
	}

	// Extract major version number (e.g., "PostgreSQL 17.1" -> "17")
	re := regexp.MustCompile(`PostgreSQL (\d+)`)
	matches := re.FindStringSubmatch(version)
	if len(matches) >= 2 {
		return matches[1], nil
	}

	// Fallback: try to parse from version string
	parts := strings.Fields(version)
	if len(parts) >= 2 {
		major := strings.Split(parts[1], ".")[0]
		return major, nil
	}

	return "17", nil // Default to 17
}

type Metrics struct {
	PGVersion         string
	DatabaseSizeBytes *int64
}

func (br *BackupRunner) collectMetrics(ctx context.Context, connURL string) (*Metrics, error) {
	connCtx, cancel := context.WithTimeout(ctx, dbConnectionTimeout)
	defer cancel()

	conn, err := pgx.Connect(connCtx, connURL)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.Background())

	metrics := &Metrics{}

	// Get PostgreSQL version
	var version string
	if err := conn.QueryRow(ctx, "SELECT version()").Scan(&version); err == nil {
		re := regexp.MustCompile(`PostgreSQL (\d+(?:\.\d+)?)`)
		matches := re.FindStringSubmatch(version)
		if len(matches) >= 2 {
			metrics.PGVersion = matches[1]
		}
	}

	// Get database size
	var sizeBytes int64
	if err := conn.QueryRow(ctx, "SELECT pg_database_size(current_database())").Scan(&sizeBytes); err == nil {
		metrics.DatabaseSizeBytes = &sizeBytes
	}

	return metrics, nil
}

func (br *BackupRunner) dumpRoles(ctx context.Context, connURL, outputFile string, pgVersion string) error {
	parsed, err := parseConnectionURL(connURL)
	if err != nil {
		return err
	}

	// Ensure output directory exists
	outputDir := filepath.Dir(outputFile)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	// On macOS, Docker containers need host.docker.internal to reach host services
	host := parsed.host
	if runtime.GOOS == "darwin" && (host == "localhost" || host == "127.0.0.1") {
		host = "host.docker.internal"
	}

	// Run pg_dumpall and capture stdout (no file redirect, no bind mount needed)
	cmd := []string{"pg_dumpall", "--roles-only"}
	env := []string{
		fmt.Sprintf("PGHOST=%s", host),
		fmt.Sprintf("PGPORT=%d", parsed.port),
		fmt.Sprintf("PGUSER=%s", parsed.user),
		fmt.Sprintf("PGPASSWORD=%s", parsed.password),
	}

	cfg := container.Config{
		Image: fmt.Sprintf("postgres:%s", pgVersion),
		Env:   env,
		Cmd:   cmd,
	}

	hostConfig := container.HostConfig{
		NetworkMode: container.NetworkMode("host"),
		// No bind mounts needed - we'll capture stdout and write to file directly
	}

	stdout := docker.NewContainerOutput()
	stderr := docker.NewContainerOutput()

	if err = docker.RunOnceWithConfig(ctx, cfg, hostConfig, stdout, stderr); err != nil {
		stderrStr := stderr.String()
		if stderrStr != "" {
			br.logger.Error("Docker command stderr", zap.String("output", stderrStr))
		}
		return err
	}

	// Write captured stdout to file
	stdoutData := stdout.Bytes()
	if err := os.WriteFile(outputFile, stdoutData, 0644); err != nil {
		return fmt.Errorf("failed to write output file: %w", err)
	}

	return nil
}

func (br *BackupRunner) dumpSchema(ctx context.Context, connURL, outputFile string, pgVersion string) error {
	return br.runPgDump(ctx, connURL, outputFile, pgVersion, []string{
		"--schema-only",
		"--no-owner",
		"--no-acl",
		"--no-privileges",
	})
}

func (br *BackupRunner) dumpData(ctx context.Context, connURL, outputFile string, pgVersion string) error {
	return br.runPgDump(ctx, connURL, outputFile, pgVersion, []string{
		"--data-only",
		"--use-set-session-authorization",
		"--no-owner",
		"--no-acl",
		"--column-inserts",
	})
}

func (br *BackupRunner) runPgDump(ctx context.Context, connURL, outputFile string, pgVersion string, options []string) error {
	parsed, err := parseConnectionURL(connURL)
	if err != nil {
		return err
	}

	// Ensure output directory exists
	outputDir := filepath.Dir(outputFile)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	// On macOS, Docker containers need host.docker.internal to reach host services
	host := parsed.host
	if runtime.GOOS == "darwin" && (host == "localhost" || host == "127.0.0.1") {
		host = "host.docker.internal"
	}

	pgDumpArgs := []string{"pg_dump",
		fmt.Sprintf("--host=%s", host),
		fmt.Sprintf("--port=%d", parsed.port),
		fmt.Sprintf("--username=%s", parsed.user),
		fmt.Sprintf("--dbname=%s", parsed.database),
		"--no-password",
	}
	pgDumpArgs = append(pgDumpArgs, options...)

	// Run pg_dump and capture stdout (no file redirect, no bind mount needed)
	cmd := pgDumpArgs
	env := []string{
		fmt.Sprintf("PGPASSWORD=%s", parsed.password),
	}

	cfg := container.Config{
		Image: fmt.Sprintf("postgres:%s", pgVersion),
		Env:   env,
		Cmd:   cmd,
	}

	hostConfig := container.HostConfig{
		NetworkMode: container.NetworkMode("host"),
		// No bind mounts needed - we'll capture stdout and write to file directly
	}

	stdout := docker.NewContainerOutput()
	stderr := docker.NewContainerOutput()

	if err := docker.RunOnceWithConfig(ctx, cfg, hostConfig, stdout, stderr); err != nil {
		if stderrStr := stderr.String(); stderrStr != "" {
			br.logger.Error("Docker command stderr", zap.String("output", stderrStr))
			return fmt.Errorf("%w: stderr: %s", err, stderrStr)
		}
		return err
	}

	// Write captured stdout to file
	stdoutData := stdout.Bytes()
	if err := os.WriteFile(outputFile, stdoutData, 0644); err != nil {
		return fmt.Errorf("failed to write output file: %w", err)
	}

	return nil
}

type connParams struct {
	host     string
	port     int
	user     string
	password string
	database string
}

func parseConnectionURL(connURL string) (*connParams, error) {
	parsed, err := url.Parse(connURL)
	if err != nil {
		return nil, fmt.Errorf("invalid connection URL: %w", err)
	}

	host := parsed.Hostname()
	port := 5432
	if parsed.Port() != "" {
		if p, err := strconv.Atoi(parsed.Port()); err == nil {
			port = p
		}
	}

	user := parsed.User.Username()
	password, _ := parsed.User.Password()

	database := strings.TrimPrefix(parsed.Path, "/")
	if database == "" {
		database = "postgres"
	}

	return &connParams{
		host:     host,
		port:     port,
		user:     user,
		password: password,
		database: database,
	}, nil
}

func (br *BackupRunner) createArchive(files []string, archivePath, baseDir string) error {
	// Create tar.gz archive
	file, err := os.Create(archivePath)
	if err != nil {
		return fmt.Errorf("failed to create archive file: %w", err)
	}
	defer file.Close()

	gzw := gzip.NewWriter(file)
	defer gzw.Close()

	tw := tar.NewWriter(gzw)
	defer tw.Close()

	for _, filePath := range files {
		relPath, err := filepath.Rel(baseDir, filePath)
		if err != nil {
			return fmt.Errorf("failed to get relative path: %w", err)
		}

		f, err := os.Open(filePath)
		if err != nil {
			return fmt.Errorf("failed to open file %s: %w", filePath, err)
		}

		info, err := f.Stat()
		if err != nil {
			f.Close()
			return fmt.Errorf("failed to stat file %s: %w", filePath, err)
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			f.Close()
			return fmt.Errorf("failed to create tar header: %w", err)
		}
		header.Name = relPath

		if err := tw.WriteHeader(header); err != nil {
			f.Close()
			return fmt.Errorf("failed to write tar header: %w", err)
		}

		if _, err := io.Copy(tw, f); err != nil {
			f.Close()
			return fmt.Errorf("failed to write file to archive: %w", err)
		}

		f.Close()
	}

	return nil
}

func (br *BackupRunner) saveManifest(path string, manifest *BackupManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal manifest: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("failed to create manifest directory: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write manifest: %w", err)
	}

	return nil
}

func (br *BackupRunner) createFailedManifest(runID, dbID string, startedAt time.Time, err error) (*BackupManifest, error) {
	finishedAt := br.now()
	return &BackupManifest{
		RunID:      runID,
		DatabaseID: dbID,
		StartedAt:  startedAt.Format("2006-01-02T15:04:05Z07:00"),
		FinishedAt: finishedAt.Format("2006-01-02T15:04:05Z07:00"),
		DurationMs: finishedAt.Sub(startedAt).Milliseconds(),
		Status:     "failed",
		Error:      err.Error(),
	}, nil
}

func (br *BackupRunner) now() time.Time {
	return time.Now()
}

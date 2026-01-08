package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mxschmitt/pg-backup-scheduler/internal/api"
	"github.com/mxschmitt/pg-backup-scheduler/internal/config"
	"github.com/mxschmitt/pg-backup-scheduler/internal/service"
	"go.uber.org/zap"
)

func TestIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// Create a context with timeout for the entire test
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	backupDir := t.TempDir()
	dbURL := getTestDBURL(t)
	projectName := "testdb"

	t.Logf("Test setup: backupDir=%s, dbURL=%s", backupDir, maskPassword(dbURL))

	// Setup - use a cron expression that won't trigger during the test
	cfg := &config.Config{
		RetentionDays:  1,
		BackupCron:     "0 0 1 1 *", // Jan 1st at midnight - won't trigger
		TZ:             "UTC",
		LocalBackupDir: backupDir,
		LogLevel:       "INFO", // Enable logging for debugging
		LogFormat:      "text",
		ServicePort:    8080,
		Databases:      map[string]string{projectName: dbURL},
	}

	// Use a real logger for debugging
	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	t.Log("Initializing service...")
	svc, err := service.New(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("Failed to create service: %v", err)
	}

	// Shutdown with timeout
	defer func() {
		t.Log("Shutting down service...")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := svc.Shutdown(shutdownCtx); err != nil {
			t.Logf("Service shutdown error (non-fatal): %v", err)
		}
	}()

	t.Log("Creating API server...")
	apiServer := api.New(cfg, svc, logger)
	server := httptest.NewServer(apiServer.Handler())
	defer func() {
		t.Log("Closing test server...")
		server.Close()
	}()

	// Test health endpoint
	t.Run("health", func(t *testing.T) {
		t.Log("Testing /healthz endpoint...")
		resp := mustGET(t, server.URL+"/healthz")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected 200, got %d", resp.StatusCode)
		}
		t.Log("Health check passed")
	})

	// Test status endpoint
	t.Run("status", func(t *testing.T) {
		t.Log("Testing /status endpoint...")
		var status map[string]interface{}
		mustGETJSON(t, server.URL+"/status", &status)
		count := status["databases_configured"].(float64)
		if count != 1 {
			t.Errorf("Expected 1 database, got %v", count)
		}
		t.Logf("Status check passed: %d databases configured", int(count))
	})

	// Test backup
	t.Run("backup", func(t *testing.T) {
		t.Logf("Triggering backup for project: %s", projectName)
		resp := mustPOST(t, server.URL+"/run/"+projectName)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("Backup trigger failed: %d %s", resp.StatusCode, body)
		}
		t.Log("Backup trigger accepted, waiting for completion...")

		// Wait for backup to complete with context, passing server URL for status checks
		waitForBackupWithContext(t, ctx, server.URL, backupDir, projectName, 2*time.Minute)

		t.Log("Backup completed, verifying files...")
		// Verify files
		verifyBackupFiles(t, backupDir, projectName)
		t.Log("Backup verification passed")
	})

	t.Log("Integration test completed successfully")
}

func waitForBackupWithContext(t *testing.T, ctx context.Context, apiURL, backupDir, projectName string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	lastLog := time.Now()
	startTime := time.Now()

	for {
		// Check context cancellation
		select {
		case <-ctx.Done():
			t.Fatalf("Context cancelled: %v", ctx.Err())
		default:
		}

		// Check timeout
		if time.Now().After(deadline) {
			// Before failing, check if there's a manifest with failure status
			checkForFailure(t, backupDir, projectName)
			t.Fatal("Backup did not complete within timeout")
		}

		// After 10 seconds without files, start checking service status
		elapsed := time.Since(startTime)
		if elapsed > 10*time.Second {
			// Check if service reports backup is done (via status endpoint)
			if status, err := checkServiceStatus(apiURL); err == nil {
				running, _ := status["currently_running"].(bool)
				if !running {
					// Backup is no longer running - check if files exist or if it failed
					projectDir := filepath.Join(backupDir, projectName)
					entries, _ := os.ReadDir(projectDir)
					if len(entries) == 0 {
						// No files and not running = backup failed
						t.Fatalf("Backup failed: service reports backup completed but no files were created. Check logs above for errors.")
					}
				}
			}
		}

		// Log progress every 5 seconds
		if time.Since(lastLog) > 5*time.Second {
			t.Logf("Waiting for backup... (elapsed: %v, timeout in %v)", elapsed.Round(time.Second), time.Until(deadline).Round(time.Second))
			lastLog = time.Now()
		}

		projectDir := filepath.Join(backupDir, projectName)
		entries, err := os.ReadDir(projectDir)
		if err != nil {
			<-ticker.C
			continue
		}
		if len(entries) == 0 {
			<-ticker.C
			continue
		}

		// Check date directory
		dateDir := filepath.Join(projectDir, entries[0].Name())
		files, err := os.ReadDir(dateDir)
		if err != nil || len(files) == 0 {
			<-ticker.C
			continue
		}

		// Check for manifest first to detect failures early
		var manifestPath string
		hasArchive := false
		for _, file := range files {
			name := file.Name()
			if strings.HasPrefix(name, "manifest-") && strings.HasSuffix(name, ".json") {
				manifestPath = filepath.Join(dateDir, name)
				// Check manifest status immediately
				if status, err := checkManifestStatus(manifestPath); err == nil {
					if status == "failed" {
						// Read error from manifest
						data, _ := os.ReadFile(manifestPath)
						var manifest map[string]interface{}
						json.Unmarshal(data, &manifest)
						errorMsg, _ := manifest["error"].(string)
						t.Fatalf("Backup failed: %s", errorMsg)
					}
				}
			}
			if strings.HasPrefix(name, "backup-") && strings.HasSuffix(name, ".tar.gz") {
				hasArchive = true
				if info, err := file.Info(); err == nil && info.Size() == 0 {
					t.Fatal("Archive is empty")
				}
			}
		}

		// If we have both archive and manifest, we're done
		if manifestPath != "" && hasArchive {
			t.Log("Backup files found!")
			return
		}

		<-ticker.C
	}
}

func checkServiceStatus(apiURL string) (map[string]interface{}, error) {
	resp, err := http.Get(apiURL + "/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status endpoint returned %d", resp.StatusCode)
	}

	var status map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, err
	}

	return status, nil
}

func checkForFailure(t *testing.T, backupDir, projectName string) {
	projectDir := filepath.Join(backupDir, projectName)
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		dateDir := filepath.Join(projectDir, entry.Name())
		files, err := os.ReadDir(dateDir)
		if err != nil {
			continue
		}
		for _, file := range files {
			if strings.HasPrefix(file.Name(), "manifest-") && strings.HasSuffix(file.Name(), ".json") {
				manifestPath := filepath.Join(dateDir, file.Name())
				if status, err := checkManifestStatus(manifestPath); err == nil {
					if status == "failed" {
						data, _ := os.ReadFile(manifestPath)
						t.Logf("Found failed backup manifest: %s\n%s", manifestPath, string(data))
					}
				}
			}
		}
	}
}

func checkManifestStatus(manifestPath string) (string, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", err
	}
	var manifest map[string]interface{}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", err
	}
	if status, ok := manifest["status"].(string); ok {
		return status, nil
	}
	return "", fmt.Errorf("no status field in manifest")
}

func maskPassword(url string) string {
	// Simple password masking for logging
	if idx := strings.Index(url, "@"); idx > 0 {
		if userIdx := strings.Index(url[:idx], "://"); userIdx > 0 {
			prefix := url[:userIdx+3]
			suffix := url[idx:]
			return prefix + "***" + suffix
		}
	}
	return url
}

func verifyBackupFiles(t *testing.T, backupDir, projectName string) {
	projectDir := filepath.Join(backupDir, projectName)
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		t.Fatalf("Failed to read project directory: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("No backup directories found")
	}

	dateDir := filepath.Join(projectDir, entries[0].Name())
	files, err := os.ReadDir(dateDir)
	if err != nil {
		t.Fatalf("Failed to read date directory: %v", err)
	}

	var manifestPath string
	for _, file := range files {
		if strings.HasPrefix(file.Name(), "manifest-") {
			manifestPath = filepath.Join(dateDir, file.Name())
			break
		}
	}
	if manifestPath == "" {
		t.Fatal("Manifest file not found")
	}

	// Verify manifest
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("Failed to read manifest: %v", err)
	}

	var manifest map[string]interface{}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("Failed to parse manifest: %v", err)
	}

	if status, ok := manifest["status"].(string); !ok || status != "success" {
		errorMsg, _ := manifest["error"].(string)
		t.Fatalf("Backup failed: %s (error: %s)", status, errorMsg)
	}

	if dbID, ok := manifest["database_identifier"].(string); !ok || dbID != projectName {
		t.Errorf("Manifest database_identifier mismatch: expected %s, got %v", projectName, dbID)
	}
}

func getTestDBURL(t *testing.T) string {
	url := os.Getenv("TEST_DB_URL")
	if url == "" {
		// Use localhost - the backup code will handle macOS by replacing it with host.docker.internal
		// in Docker containers automatically
		host := "localhost"

		// Use current user as PostgreSQL username (Homebrew PostgreSQL default)
		username := os.Getenv("USER")
		if username == "" {
			username = "postgres" // fallback
		}

		url = fmt.Sprintf("postgresql://%s@%s:5432/postgres", username, host)
		t.Logf("Using default TEST_DB_URL: %s", maskPassword(url))
		t.Logf("Note: On macOS, backup code will use host.docker.internal in Docker containers")
	} else {
		t.Logf("Using TEST_DB_URL from environment: %s", maskPassword(url))
	}
	return url
}

func mustGET(t *testing.T, url string) *http.Response {
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s failed: %v", url, err)
	}
	return resp
}

func mustPOST(t *testing.T, url string) *http.Response {
	resp, err := http.Post(url, "application/json", nil)
	if err != nil {
		t.Fatalf("POST %s failed: %v", url, err)
	}
	return resp
}

func mustGETJSON(t *testing.T, url string, v interface{}) {
	resp := mustGET(t, url)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s returned %d: %s", url, resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("Failed to decode JSON: %v", err)
	}
}

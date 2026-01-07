package retention

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func CleanupOldBackups(baseDir, databaseID string, retentionDays int) (int, error) {
	dbDir := filepath.Join(baseDir, databaseID)
	if _, err := os.Stat(dbDir); os.IsNotExist(err) {
		return 0, nil
	}

	cutoffDate := time.Now().AddDate(0, 0, -retentionDays)
	cutoffDateStr := cutoffDate.Format("2006-01-02")

	entries, err := os.ReadDir(dbDir)
	if err != nil {
		return 0, fmt.Errorf("failed to read database directory: %w", err)
	}

	var deleted int
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Parse date from directory name (format: YYYY-MM-DD)
		dirDate := entry.Name()
		if dirDate < cutoffDateStr {
			dirPath := filepath.Join(dbDir, dirDate)
			if err := os.RemoveAll(dirPath); err != nil {
				return deleted, fmt.Errorf("failed to delete directory %s: %w", dirPath, err)
			}
			deleted++
		}
	}

	return deleted, nil
}

func CleanupAllDatabases(baseDir string, databaseIDs []string, retentionDays int) (map[string]int, error) {
	results := make(map[string]int)
	for _, dbID := range databaseIDs {
		count, err := CleanupOldBackups(baseDir, dbID, retentionDays)
		if err != nil {
			return results, err
		}
		if count > 0 {
			results[dbID] = count
		}
	}
	return results, nil
}

package metadata

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const (
	latestRunFile = "latest.json"
	runningFile   = "running.json"
)

type ServiceStatus struct {
	Running bool `json:"running"`
}

func ReadLastRun(baseDir string) (map[string]interface{}, error) {
	filePath := filepath.Join(baseDir, "metadata", latestRunFile)
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read last run: %w", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("failed to parse last run: %w", err)
	}

	return result, nil
}

func WriteLastRun(baseDir string, data map[string]interface{}) error {
	metadataDir := filepath.Join(baseDir, "metadata")
	if err := os.MkdirAll(metadataDir, 0755); err != nil {
		return fmt.Errorf("failed to create metadata directory: %w", err)
	}

	filePath := filepath.Join(metadataDir, latestRunFile)
	dataBytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal last run: %w", err)
	}

	if err := os.WriteFile(filePath, dataBytes, 0644); err != nil {
		return fmt.Errorf("failed to write last run: %w", err)
	}

	return nil
}

func ReadServiceStatus(baseDir string) (*ServiceStatus, error) {
	filePath := filepath.Join(baseDir, "metadata", runningFile)
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return &ServiceStatus{Running: false}, nil
		}
		return nil, fmt.Errorf("failed to read service status: %w", err)
	}

	var status ServiceStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return nil, fmt.Errorf("failed to parse service status: %w", err)
	}

	return &status, nil
}

func WriteServiceStatus(baseDir string, status *ServiceStatus) error {
	metadataDir := filepath.Join(baseDir, "metadata")
	if err := os.MkdirAll(metadataDir, 0755); err != nil {
		return fmt.Errorf("failed to create metadata directory: %w", err)
	}

	filePath := filepath.Join(metadataDir, runningFile)
	dataBytes, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal service status: %w", err)
	}

	if err := os.WriteFile(filePath, dataBytes, 0644); err != nil {
		return fmt.Errorf("failed to write service status: %w", err)
	}

	return nil
}

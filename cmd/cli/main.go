package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/mxschmitt/pg-backup-scheduler/internal/config"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s [status|backup <project>]\n", os.Args[0])
		os.Exit(1)
	}

	command := os.Args[1]

	// Load config for API URL
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	apiURL := os.Getenv("API_URL")
	if apiURL == "" {
		// Use 127.0.0.1 instead of localhost to avoid IPv6 resolution issues
		apiURL = fmt.Sprintf("http://127.0.0.1:%d", cfg.ServicePort)
	}

	switch command {
	case "status":
		if err := handleStatus(apiURL); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "backup":
		if len(os.Args) < 3 {
			fmt.Fprintf(os.Stderr, "Error: project name required\n")
			fmt.Fprintf(os.Stderr, "Usage: %s backup <project>\n", os.Args[0])
			os.Exit(1)
		}
		projectID := os.Args[2]
		if err := handleBackup(apiURL, projectID); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", command)
		fmt.Fprintf(os.Stderr, "Usage: %s [status|backup <project>]\n", os.Args[0])
		os.Exit(1)
	}
}

func makeRequest(apiURL, method, path string) (map[string]interface{}, error) {
	url := fmt.Sprintf("%s%s", apiURL, path)
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to API at %s: %w", apiURL, err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if errMsg, ok := result["error"].(string); ok {
			return nil, fmt.Errorf("HTTP error: %d %s - %s", resp.StatusCode, resp.Status, errMsg)
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP error: %d %s - %s", resp.StatusCode, resp.Status, string(bodyBytes))
	}

	return result, nil
}

func handleStatus(apiURL string) error {
	data, err := makeRequest(apiURL, "GET", "/status")
	if err != nil {
		return err
	}

	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}

	fmt.Println(string(jsonData))
	return nil
}

func handleBackup(apiURL, projectID string) error {
	path := fmt.Sprintf("/run/%s", projectID)
	url := fmt.Sprintf("%s%s", apiURL, path)
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to API at %s: %w", apiURL, err)
	}
	defer resp.Body.Close()

	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if errMsg, ok := data["error"].(string); ok {
			return fmt.Errorf("HTTP error: %d %s - %s", resp.StatusCode, resp.Status, errMsg)
		}
		return fmt.Errorf("HTTP error: %d %s", resp.StatusCode, resp.Status)
	}

	if status, ok := data["status"].(string); ok {
		if status == "accepted" {
			if message, ok := data["message"].(string); ok {
				fmt.Println(message)
			} else {
				fmt.Printf("Backup started for project: %s\n", projectID)
			}
			return nil
		}
	}

	// Print full response if not in expected format
	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(jsonData))
	return nil
}

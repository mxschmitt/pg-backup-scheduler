package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type Config struct {
	// Backup Configuration
	RetentionDays int

	// Scheduling
	BackupCron string
	TZ         string

	// Storage
	LocalBackupDir string

	// Logging
	LogLevel  string
	LogFormat string

	// Service
	ServicePort int

	// Databases (parsed from env)
	Databases map[string]string
}

func Load() (*Config, error) {
	localBackupDir := getEnvString("LOCAL_BACKUP_DIR", "./backups")

	cfg := &Config{
		RetentionDays:  getEnvInt("RETENTION_DAYS", 30),
		BackupCron:     getEnvString("BACKUP_CRON", "30 0 * * *"),
		TZ:             getEnvString("TZ", "Europe/Berlin"),
		LocalBackupDir: localBackupDir,
		LogLevel:       getEnvString("LOG_LEVEL", "INFO"),
		LogFormat:      getEnvString("LOG_FORMAT", "json"),
		ServicePort:    getEnvInt("SERVICE_PORT", 8080),
	}

	// Parse database configurations
	cfg.Databases = getDatabaseConfigs()

	// Resolve absolute path for backup directory
	if !filepath.IsAbs(cfg.LocalBackupDir) {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("failed to get working directory: %w", err)
		}
		cfg.LocalBackupDir = filepath.Join(cwd, cfg.LocalBackupDir)
	}

	return cfg, nil
}

func getEnvString(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return defaultValue
}

func getDatabaseConfigs() map[string]string {
	configs := make(map[string]string)
	for _, env := range os.Environ() {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key, value := parts[0], parts[1]
		if !strings.HasPrefix(strings.ToUpper(key), "BACKUP_") {
			continue
		}
		// Extract project name (everything after BACKUP_)
		projectName := key[7:] // Remove "BACKUP_" prefix
		// Only treat as database URL if value starts with postgresql://
		if projectName != "" && strings.HasPrefix(strings.TrimSpace(value), "postgresql://") {
			projectNameLower := strings.ToLower(projectName)
			configs[projectNameLower] = strings.TrimSpace(value)
		}
	}
	return configs
}

func NewLogger(cfg *Config) (*zap.Logger, error) {
	var level zapcore.Level
	switch strings.ToUpper(cfg.LogLevel) {
	case "DEBUG":
		level = zapcore.DebugLevel
	case "INFO":
		level = zapcore.InfoLevel
	case "WARN":
		level = zapcore.WarnLevel
	case "ERROR":
		level = zapcore.ErrorLevel
	default:
		level = zapcore.InfoLevel
	}

	config := zap.NewProductionConfig()
	if cfg.LogFormat == "text" {
		config = zap.NewDevelopmentConfig()
	}
	config.Level = zap.NewAtomicLevelAt(level)
	config.EncoderConfig.TimeKey = "timestamp"
	config.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder

	return config.Build()
}

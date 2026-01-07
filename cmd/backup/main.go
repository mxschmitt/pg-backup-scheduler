package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/mxschmitt/pg-backup-scheduler/internal/api"
	"github.com/mxschmitt/pg-backup-scheduler/internal/config"
	"github.com/mxschmitt/pg-backup-scheduler/internal/service"
	"go.uber.org/zap"
)

func main() {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Initialize logger
	logger, err := config.NewLogger(cfg)
	if err != nil {
		log.Fatalf("Failed to initialize logger: %v", err)
	}
	defer logger.Sync()

	logger.Info("Starting PostgreSQL Backup Service")

	// Initialize service
	ctx := context.Background()
	backupService, err := service.New(ctx, cfg, logger)
	if err != nil {
		logger.Fatal("Failed to initialize backup service", zap.Error(err))
	}

	// Create and start API server
	apiServer := api.New(cfg, backupService, logger)
	go func() {
		if err := apiServer.Start(); err != nil {
			logger.Fatal("API server failed", zap.Error(err))
		}
	}()

	logger.Info("Service started successfully")

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	logger.Info("Shutting down gracefully...")
	if err := apiServer.Shutdown(ctx); err != nil {
		logger.Error("Error during shutdown", zap.Error(err))
	}
	if err := backupService.Shutdown(ctx); err != nil {
		logger.Error("Error shutting down service", zap.Error(err))
	}
}

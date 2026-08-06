package main

import (
	"errors"
	"fmt"
	"os"

	"go.uber.org/zap"

	"voicx/internal/config"
	"voicx/internal/logging"
	"voicx/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "voicx-migrate: %v\n", err)
		os.Exit(1)
	}
}

func run() (runErr error) {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	logger, err := logging.New(cfg.DevMode, cfg.LogLevel)
	if err != nil {
		return fmt.Errorf("initializing logger: %w", err)
	}
	defer func() {
		if err := logger.Sync(); err != nil {
			// Sync commonly returns EINVAL for console streams even though all
			// bytes were written. Surface it without turning a successful schema
			// migration into a failed command.
			fmt.Fprintf(os.Stderr, "voicx-migrate: syncing logger: %v\n", err)
		}
	}()

	logger.Info("running database migrations", zap.String("database_url", cfg.RedactedDatabaseURL()))

	s, err := store.New(cfg.DatabaseURL, logger,
		cfg.DBMaxOpenConns, cfg.DBMaxIdleConns, cfg.DBConnMaxLifetime)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("closing store: %w", err))
		}
	}()

	if err := s.Migrate(); err != nil {
		return fmt.Errorf("applying migrations: %w", err)
	}

	logger.Info("migrations completed successfully")
	return nil
}

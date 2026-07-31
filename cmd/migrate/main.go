package main

import (
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

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	logger, err := logging.New(cfg.DevMode, cfg.LogLevel)
	if err != nil {
		return fmt.Errorf("initializing logger: %w", err)
	}
	defer logger.Sync()

	logger.Info("running database migrations", zap.String("database_url", cfg.DatabaseURL))

	s, err := store.New(cfg.DatabaseURL, logger,
		cfg.DBMaxOpenConns, cfg.DBMaxIdleConns, cfg.DBConnMaxLifetime)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer s.Close()

	if err := s.Migrate(); err != nil {
		return fmt.Errorf("applying migrations: %w", err)
	}

	logger.Info("migrations completed successfully")
	return nil
}

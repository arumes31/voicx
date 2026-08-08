package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"voicx/internal/config"
	"voicx/internal/logging"
	"voicx/internal/store"
)

func main() {
	timeout := flag.Duration("timeout", 5*time.Minute,
		"maximum time to wait for the migration lock and apply migrations")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "voicx-migrate: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, timeout time.Duration) (runErr error) {
	if ctx == nil {
		return errors.New("migration context is nil")
	}
	if timeout <= 0 {
		return fmt.Errorf("migration timeout must be positive, got %s", timeout)
	}
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

	migrationCtx, cancelMigration := context.WithTimeout(ctx, timeout)
	err = s.MigrateContext(migrationCtx)
	cancelMigration()
	if err != nil {
		return fmt.Errorf("applying migrations: %w", err)
	}

	logger.Info("migrations completed successfully")
	return nil
}

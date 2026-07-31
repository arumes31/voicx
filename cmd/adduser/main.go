// adduser registers a voicx user in the database and prints its unique ID.
//
// Usage:
//
//	adduser -nickname <name> -password <pw> [-admin] [-db <dsn>]
//
// The database DSN comes from -db, then VOICX_DATABASE_URL, then the
// built-in default. Migrations are applied (idempotent). With -admin the
// user gets the is_admin flag (RegisterUser creates non-admin users, so it
// is set with a follow-up UPDATE).
//
// Exit codes: 0 on success, and also 0 when the user already exists
// (idempotent for provisioning scripts); 1 on real errors.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"

	"voicx/internal/auth"
	"voicx/internal/store"
)

const defaultDSN = "postgres://voicx:voicx@localhost:5432/voicx?sslmode=disable"

func main() {
	nickname := flag.String("nickname", "", "user nickname (required)")
	password := flag.String("password", "", "user password (required)")
	admin := flag.Bool("admin", false, "grant server admin (users.is_admin)")
	dsn := flag.String("db", "", "database DSN (default: VOICX_DATABASE_URL or built-in default)")
	flag.Parse()

	if *nickname == "" || *password == "" {
		fmt.Fprintln(os.Stderr, "adduser: -nickname and -password are required")
		os.Exit(1)
	}

	if err := run(*nickname, *password, *admin, *dsn); err != nil {
		if errors.Is(err, auth.ErrUserExists) {
			fmt.Printf("user %q already exists (no changes made)\n", *nickname)
			return
		}
		fmt.Fprintf(os.Stderr, "adduser: %v\n", err)
		os.Exit(1)
	}
}

func run(nickname, password string, admin bool, dsn string) error {
	if dsn == "" {
		dsn = os.Getenv("VOICX_DATABASE_URL")
	}
	if dsn == "" {
		dsn = defaultDSN
	}

	logger, err := zap.NewProduction()
	if err != nil {
		return fmt.Errorf("logger: %w", err)
	}
	defer func() { _ = logger.Sync() }()

	dbStore, err := store.New(dsn, logger, 5, 1, time.Minute)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer dbStore.Close()

	if err := dbStore.Migrate(); err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	authSvc := auth.New(dbStore, logger)
	uniqueID, err := authSvc.RegisterUser(ctx, nickname, password)
	if err != nil {
		return err
	}

	if admin {
		const q = `UPDATE users SET is_admin = TRUE WHERE unique_id = $1`
		if _, err := dbStore.DB().ExecContext(ctx, q, uniqueID); err != nil {
			return fmt.Errorf("granting admin: %w", err)
		}
	}

	fmt.Printf("registered user %q\nunique_id: %s\nadmin: %v\n", nickname, uniqueID, admin)
	return nil
}

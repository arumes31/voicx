package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMigrationHardensAndDiscardsSession(t *testing.T) {
	s := testScratchStore(t)
	s.DB().SetMaxOpenConns(1)
	s.DB().SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	conn, err := s.DB().Conn(ctx)
	if err != nil {
		t.Fatalf("reserving hostile migration connection: %v", err)
	}
	locked := false
	defer func() {
		if locked {
			_ = releaseMigrationLock(conn)
		}
		_ = conn.Close()
	}()
	if _, err := conn.ExecContext(ctx, `CREATE FUNCTION public.now()
		RETURNS TIMESTAMPTZ LANGUAGE SQL IMMUTABLE
		AS $$SELECT '2000-01-01 00:00:00+00'::TIMESTAMPTZ$$;
		CREATE FUNCTION public.voicx_shadow_regex(TEXT, TEXT)
		RETURNS BOOLEAN LANGUAGE SQL IMMUTABLE AS $$SELECT TRUE$$;
		CREATE OPERATOR public.~ (
			LEFTARG = TEXT, RIGHTARG = TEXT, FUNCTION = public.voicx_shadow_regex
		);
		CREATE TEMP TABLE schema_migrations (shadow_marker TEXT);
		CREATE TEMP TABLE chat_messages (shadow_marker TEXT);
		SET search_path = public, pg_catalog;
		SET default_tablespace = pg_global;
		SET standard_conforming_strings = off`); err != nil {
		t.Fatalf("installing hostile migration session state: %v", err)
	}
	var migrationPID int
	if err := conn.QueryRowContext(ctx, `SELECT pg_catalog.pg_backend_pid()`).Scan(&migrationPID); err != nil {
		t.Fatalf("reading migration backend PID: %v", err)
	}
	if err := acquireMigrationLock(ctx, conn, nil); err != nil {
		t.Fatalf("acquiring migration lock: %v", err)
	}
	locked = true
	if err := s.migrateLocked(ctx, conn); err != nil {
		t.Fatalf("migrating with hostile session state: %v", err)
	}

	var searchPath, defaultTablespace, tempTablespaces, conformingStrings string
	if err := conn.QueryRowContext(ctx, `SELECT
		pg_catalog.current_setting('search_path'),
		pg_catalog.current_setting('default_tablespace'),
		pg_catalog.current_setting('temp_tablespaces'),
		pg_catalog.current_setting('standard_conforming_strings')`).Scan(
		&searchPath, &defaultTablespace, &tempTablespaces, &conformingStrings,
	); err != nil {
		t.Fatalf("reading hardened migration settings: %v", err)
	}
	if searchPath != "public, pg_temp" || defaultTablespace != "" || tempTablespaces != "" ||
		conformingStrings != "on" {
		t.Fatalf("migration settings = (%q, %q, %q, %q), want secure normalized values",
			searchPath, defaultTablespace, tempTablespaces, conformingStrings)
	}

	var (
		publicLedger, publicChat, tempLedger, tempChat sql.NullString
		oldestApplied                                  time.Time
	)
	if err := conn.QueryRowContext(ctx, `SELECT
		pg_catalog.to_regclass('public.schema_migrations')::TEXT,
		pg_catalog.to_regclass('public.chat_messages')::TEXT,
		pg_catalog.to_regclass('pg_temp.schema_migrations')::TEXT,
		pg_catalog.to_regclass('pg_temp.chat_messages')::TEXT,
		(SELECT MIN(applied_at) FROM public.schema_migrations)`).Scan(
		&publicLedger, &publicChat, &tempLedger, &tempChat, &oldestApplied,
	); err != nil {
		t.Fatalf("checking exact public and temporary migration objects: %v", err)
	}
	if !publicLedger.Valid || !publicChat.Valid || !tempLedger.Valid || !tempChat.Valid {
		t.Fatalf("migration object visibility = public ledger/chat %v/%v, temp ledger/chat %v/%v",
			publicLedger, publicChat, tempLedger, tempChat)
	}
	if oldestApplied.Equal(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("public.now shadow controlled applied_at: %v", oldestApplied)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE public.schema_migrations
		SET checksum = $1
		WHERE filename = (SELECT MIN(filename) FROM public.schema_migrations)`,
		strings.Repeat("A", 64)); err == nil {
		t.Fatal("public regex-operator shadow weakened the checksum constraint")
	}

	const ambiguousBackslashMigration = `-- voicx:no-transaction
CREATE INDEX CONCURRENTLY voicx_standard_string_probe
    ON public.chat_messages (client_msg_id)
    WHERE client_msg_id = 'ends\';`
	if err := applyNonTransactionalMigration(ctx, conn, ambiguousBackslashMigration); err != nil {
		t.Fatalf("applying standard-conforming ambiguous-backslash migration: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`DROP INDEX CONCURRENTLY public.voicx_standard_string_probe`); err != nil {
		t.Fatalf("dropping ambiguous-backslash probe index: %v", err)
	}

	if err := releaseMigrationLock(conn); err != nil {
		t.Fatalf("releasing migration lock: %v", err)
	}
	locked = false
	if err := conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("closing discarded migration connection: %v", err)
	}

	var (
		applicationPID int
		newSearchPath  string
		newTempLedger  sql.NullString
	)
	if err := s.DB().QueryRowContext(ctx, `SELECT
		pg_catalog.pg_backend_pid(),
		pg_catalog.current_setting('search_path'),
		pg_catalog.to_regclass('pg_temp.schema_migrations')::TEXT`).Scan(
		&applicationPID, &newSearchPath, &newTempLedger,
	); err != nil {
		t.Fatalf("checking post-migration pool connection: %v", err)
	}
	if applicationPID == migrationPID {
		t.Fatalf("migration backend PID %d was returned to the pool", migrationPID)
	}
	if newSearchPath == "public, pg_temp" || newTempLedger.Valid {
		t.Fatalf("migration session state leaked to pool: search_path=%q temp ledger=%v",
			newSearchPath, newTempLedger)
	}
}

func TestMigrationReplacesMismatchedChecksumConstraint(t *testing.T) {
	s := testScratchStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := s.MigrateContext(ctx); err != nil {
		t.Fatalf("initial MigrateContext: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, `ALTER TABLE public.schema_migrations
		DROP CONSTRAINT schema_migrations_checksum_sha256;
		ALTER TABLE public.schema_migrations
		ADD CONSTRAINT schema_migrations_checksum_sha256
		CHECK (pg_catalog.length(checksum) = 64)`); err != nil {
		t.Fatalf("installing mismatched checksum constraint: %v", err)
	}
	if err := s.MigrateContext(ctx); err != nil {
		t.Fatalf("repairing mismatched checksum constraint: %v", err)
	}

	var definition string
	var validated bool
	if err := s.DB().QueryRowContext(ctx, `SELECT
		pg_catalog.pg_get_constraintdef(oid, false), convalidated
		FROM pg_catalog.pg_constraint
		WHERE conrelid = 'public.schema_migrations'::pg_catalog.regclass
		  AND conname = $1`, migrationChecksumConstraint).Scan(&definition, &validated); err != nil {
		t.Fatalf("reading repaired checksum constraint: %v", err)
	}
	if !validated || strings.Contains(definition, "length") || !strings.Contains(definition, "[0-9a-f]") {
		t.Fatalf("repaired constraint = %q (validated=%t)", definition, validated)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE public.schema_migrations
		SET checksum = $1
		WHERE filename = (SELECT MIN(filename) FROM public.schema_migrations)`,
		strings.Repeat("A", 64)); err == nil {
		t.Fatal("repaired checksum constraint accepted uppercase digest")
	}
}

package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

func TestLegacyChatConsistencyDescriptorIsPinned(t *testing.T) {
	if err := verifyLegacyChatConsistencyDescriptor(); err != nil {
		t.Fatal(err)
	}
}

func setupLegacyChatConsistency(
	t *testing.T,
	legacyChecksum any,
	wrongPublicIndex bool,
) *Store {
	t.Helper()
	s := testScratchStore(t)
	s.DB().SetMaxOpenConns(1)
	s.DB().SetMaxIdleConns(1)
	applyPreLedgerMigrations(t, s, "015z_chat_consistency_columns.sql")

	if _, err := s.DB().Exec(`CREATE SCHEMA legacy_shadow;
		CREATE TABLE legacy_shadow.chat_messages
			(LIKE public.chat_messages INCLUDING ALL);
		ALTER TABLE legacy_shadow.chat_messages
			ADD COLUMN version BIGINT NOT NULL DEFAULT 1;
		ALTER TABLE legacy_shadow.chat_messages
			ADD COLUMN client_msg_id TEXT;
		CREATE UNIQUE INDEX idx_chat_messages_client_msg_id
			ON legacy_shadow.chat_messages (channel_id, from_unique_id, client_msg_id)
			WHERE client_msg_id IS NOT NULL AND client_msg_id <> '';
		CREATE INDEX idx_chat_messages_reply_to
			ON legacy_shadow.chat_messages (reply_to_id)
			WHERE reply_to_id IS NOT NULL;
		CREATE TABLE public.schema_migrations (
			filename TEXT PRIMARY KEY,
			checksum TEXT,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT pg_catalog.now()
		)`); err != nil {
		t.Fatalf("creating legacy 016 database shape: %v", err)
	}
	if wrongPublicIndex {
		if _, err := s.DB().Exec(`ALTER TABLE public.chat_messages
			ADD COLUMN version BIGINT NOT NULL DEFAULT 1;
			ALTER TABLE public.chat_messages ADD COLUMN client_msg_id TEXT;
			CREATE INDEX idx_chat_messages_client_msg_id
			ON public.chat_messages (channel_id, from_unique_id, client_msg_id)
			WHERE client_msg_id IS NOT NULL AND client_msg_id <> ''`); err != nil {
			t.Fatalf("creating wrong public legacy index: %v", err)
		}
	}

	for _, name := range migrationNames(t) {
		if name >= "015z_chat_consistency_columns.sql" {
			break
		}
		if _, err := s.DB().Exec(`INSERT INTO public.schema_migrations
			(filename, checksum) VALUES ($1, $2)`, name, migrationChecksum(t, name)); err != nil {
			t.Fatalf("recording applied pre-016 migration %s: %v", name, err)
		}
	}
	if _, err := s.DB().Exec(`INSERT INTO public.schema_migrations
		(filename, checksum) VALUES ($1, $2)`, legacyChatConsistencyFilename, legacyChecksum); err != nil {
		t.Fatalf("recording legacy 016 checksum: %v", err)
	}
	// Persist a historically unsafe path on the sole pooled connection. The
	// current runner must neither trust its indexes nor redirect public repair.
	if _, err := s.DB().Exec(`SET search_path = legacy_shadow, public, pg_catalog`); err != nil {
		t.Fatalf("setting legacy non-public search path: %v", err)
	}
	return s
}

func TestMigrationRepairsPinnedLegacyChatConsistencyInPublic(t *testing.T) {
	s := setupLegacyChatConsistency(t, legacyChatConsistencyChecksum, false)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := s.MigrateContext(ctx); err != nil {
		t.Fatalf("migrating pinned legacy 016: %v", err)
	}

	var currentChecksum string
	if err := s.DB().QueryRowContext(ctx, `SELECT checksum
		FROM public.schema_migrations WHERE filename = $1`, legacyChatConsistencyFilename).
		Scan(&currentChecksum); err != nil {
		t.Fatalf("reading baselined legacy checksum: %v", err)
	}
	if currentChecksum != migrationChecksum(t, legacyChatConsistencyFilename) {
		t.Fatalf("legacy 016 baseline = %q, want current embedded checksum", currentChecksum)
	}
	for _, indexName := range []string{
		"idx_chat_messages_client_msg_id",
		"idx_chat_messages_reply_to",
	} {
		var targets []string
		rows, err := s.DB().QueryContext(ctx, `SELECT n.nspname || '.' || t.relname
			FROM pg_catalog.pg_class AS i
			JOIN pg_catalog.pg_namespace AS inode ON inode.oid = i.relnamespace
			JOIN pg_catalog.pg_index AS x ON x.indexrelid = i.oid
			JOIN pg_catalog.pg_class AS t ON t.oid = x.indrelid
			JOIN pg_catalog.pg_namespace AS n ON n.oid = t.relnamespace
			WHERE i.relname = $1 AND inode.nspname IN ('public', 'legacy_shadow')
			ORDER BY inode.nspname`, indexName)
		if err != nil {
			t.Fatalf("reading exact legacy/public index targets: %v", err)
		}
		for rows.Next() {
			var target string
			if err := rows.Scan(&target); err != nil {
				_ = rows.Close()
				t.Fatalf("scanning exact index target: %v", err)
			}
			targets = append(targets, target)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatalf("iterating exact index targets: %v", err)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("closing exact index targets: %v", err)
		}
		if len(targets) != 2 || targets[0] != "legacy_shadow.chat_messages" ||
			targets[1] != "public.chat_messages" {
			t.Fatalf("%s targets = %v, want preserved shadow plus repaired public", indexName, targets)
		}
	}
	assertLedgerChecksums(t, s, migrationNames(t))
}

func TestMigrationDoesNotBaselineWrongLegacyPublicIndex(t *testing.T) {
	s := setupLegacyChatConsistency(t, legacyChatConsistencyChecksum, true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	err := s.MigrateContext(ctx)
	if err == nil || !strings.Contains(err.Error(), "wrong definition") {
		t.Fatalf("legacy wrong-index error = %v, want exact-definition rejection", err)
	}
	var checksum sql.NullString
	if err := s.DB().QueryRowContext(ctx, `SELECT checksum
		FROM public.schema_migrations WHERE filename = $1`, legacyChatConsistencyFilename).
		Scan(&checksum); err != nil {
		t.Fatalf("reading rejected legacy checksum: %v", err)
	}
	if !checksum.Valid || checksum.String != legacyChatConsistencyChecksum {
		t.Fatalf("wrong legacy index was baselined: checksum = %v", checksum)
	}
	if _, err := s.DB().ExecContext(ctx,
		`DROP INDEX CONCURRENTLY public.idx_chat_messages_client_msg_id`); err != nil {
		t.Fatalf("dropping wrong public legacy index: %v", err)
	}
	if err := s.MigrateContext(ctx); err != nil {
		t.Fatalf("migrating after operator repairs wrong legacy index: %v", err)
	}
}

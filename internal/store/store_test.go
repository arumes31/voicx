package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"sort"
	"testing"
	"time"

	"go.uber.org/zap"
)

func testLogger() *zap.Logger {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}
	return logger
}

// TestNewBadURL verifies that New returns an error when the database URL is
// invalid (i.e. the database cannot be reached).
func TestNewBadURL(t *testing.T) {
	_, err := New("postgres://nobody:nopass@127.0.0.1:1/doesnotexist?sslmode=disable",
		testLogger(), 1, 1, time.Minute)
	if err == nil {
		t.Fatal("expected error for bad database URL, got nil")
	}
}

// TestMigrateSkipsWhenNoDB verifies that Migrate is skipped when the database
// is unavailable (e.g. in CI without a running Postgres).
func TestMigrateSkipsWhenNoDB(t *testing.T) {
	_, err := New("postgres://nobody:nopass@127.0.0.1:1/doesnotexist?sslmode=disable",
		testLogger(), 1, 1, time.Minute)
	if err == nil {
		t.Skip("database unexpectedly available; skipping skip-when-no-DB test")
	}
	t.Skip("no database available; skipping Migrate test")
}

// TestPingSkipsWhenNoDB verifies that Ping is skipped when the database is
// unavailable.
func TestPingSkipsWhenNoDB(t *testing.T) {
	_, err := New("postgres://nobody:nopass@127.0.0.1:1/doesnotexist?sslmode=disable",
		testLogger(), 1, 1, time.Minute)
	if err == nil {
		t.Skip("database unexpectedly available; skipping skip-when-no-DB test")
	}
	t.Skip("no database available; skipping Ping test")
}

// --- Scratch-database scaffolding for the ledger and migration tests ---------

// testDBURL is the dev database URL every DB-backed test connects to.
func testDBURL() string {
	if url := os.Getenv("VOICX_TEST_DATABASE_URL"); url != "" {
		return url
	}
	return "postgres://voicx:voicx@127.0.0.1:55432/voicx?sslmode=disable"
}

// testScratchStore creates an EMPTY throwaway database and returns a Store
// bound to it. The shared dev database is already migrated, so it cannot
// answer the only question these tests ask — did this file run exactly once,
// starting from nothing. Skips (never fails) when the database is missing or
// the test role may not create one.
func testScratchStore(t *testing.T) *Store {
	t.Helper()
	base := testDBURL()
	admin, err := sql.Open("postgres", base)
	if err != nil {
		t.Skipf("no database available (%v); skipping migration test", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		t.Skipf("no database available (%v); skipping migration test", err)
	}
	name := fmt.Sprintf("voicx_scratch_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		_ = admin.Close()
		t.Skipf("cannot create a scratch database (%v); skipping migration test", err)
	}
	u, err := url.Parse(base)
	if err != nil {
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + name)
		_ = admin.Close()
		t.Skipf("database URL is not parseable (%v); skipping migration test", err)
	}
	u.Path = "/" + name
	s, err := New(u.String(), zap.NewNop(), 2, 1, time.Minute)
	if err != nil {
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + name)
		_ = admin.Close()
		t.Fatalf("opening scratch database %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		if _, err := admin.Exec("DROP DATABASE IF EXISTS " + name); err != nil {
			t.Logf("dropping scratch database %s: %v", name, err)
		}
		_ = admin.Close()
	})
	return s
}

// migrationNames lists the embedded migrations in the order Migrate applies
// them.
func migrationNames(t *testing.T) []string {
	t.Helper()
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("reading migrations: %v", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// applyPreLedgerMigrations execs every migration before stopBefore directly,
// the way Migrate did when it had no ledger, so a scratch database looks like
// a deployment upgrading from that release.
func applyPreLedgerMigrations(t *testing.T, s *Store, stopBefore string) {
	t.Helper()
	for _, name := range migrationNames(t) {
		if name >= stopBefore {
			break
		}
		content, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("reading migration %s: %v", name, err)
		}
		if noTransactionMigration(content) {
			err = applyNonTransactionalMigration(s.DB(), string(content))
		} else {
			_, err = s.DB().Exec(string(content))
		}
		if err != nil {
			t.Fatalf("applying migration %s: %v", name, err)
		}
	}
}

// ledgerRows returns the recorded migration filenames, sorted.
func ledgerRows(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.DB().Query(`SELECT filename FROM schema_migrations ORDER BY filename`)
	if err != nil {
		t.Fatalf("reading schema_migrations: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scanning schema_migrations: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading schema_migrations: %v", err)
	}
	return out
}

// seedScratchUser inserts a user so FK-bearing fixtures can reference it.
func seedScratchUser(t *testing.T, s *Store, suffix string) int64 {
	t.Helper()
	var id int64
	err := s.DB().QueryRow(
		`INSERT INTO users (unique_id, nickname, password_hash, created_at)
		 VALUES ($1, $2, 'x', NOW()) RETURNING id`,
		"ledger_"+suffix, "ledger-"+suffix).Scan(&id)
	if err != nil {
		t.Fatalf("seeding user: %v", err)
	}
	return id
}

// spoolSentinel inserts a pre-4b spool row (empty from_unique_id) with an
// empty body, so it satisfies offline_messages_sealed but is still what
// 012's DELETE removes. Its survival across a second Migrate() is the proof
// that no migration is replayed.
func spoolSentinel(t *testing.T, s *Store, userID int64) int64 {
	t.Helper()
	var id int64
	err := s.DB().QueryRow(
		`INSERT INTO offline_messages (from_user_id, to_user_id, from_unique_id, message)
		 VALUES ($1, $1, '', '') RETURNING id`, userID).Scan(&id)
	if err != nil {
		t.Fatalf("seeding spool sentinel: %v", err)
	}
	return id
}

// rowExists reports whether a row with the given id is present.
func rowExists(t *testing.T, s *Store, table string, id int64) bool {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM `+table+` WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n > 0
}

// TestMigrationLedgerRunsEachFileOnce is the guard that makes destructive DML
// in a migration safe at all: without it 012's DELETE fires on every boot
// forever (91-135).
func TestMigrationLedgerRunsEachFileOnce(t *testing.T) {
	s := testScratchStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	files := migrationNames(t)
	if got := ledgerRows(t, s); len(got) != len(files) {
		t.Fatalf("ledger = %v, want one row per file %v", got, files)
	} else {
		for i := range files {
			if got[i] != files[i] {
				t.Fatalf("ledger[%d] = %s, want %s", i, got[i], files[i])
			}
		}
	}

	var appliedAt time.Time
	if err := s.DB().QueryRow(
		`SELECT applied_at FROM schema_migrations WHERE filename = '012_chat_encryption.sql'`).Scan(&appliedAt); err != nil {
		t.Fatalf("reading 012 ledger row: %v", err)
	}

	uid := seedScratchUser(t, s, "once")
	sentinel := spoolSentinel(t, s, uid)

	if err := s.Migrate(); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if !rowExists(t, s, "offline_messages", sentinel) {
		t.Fatal("a migration was replayed: the sentinel spool row was deleted")
	}
	if got := ledgerRows(t, s); len(got) != len(files) {
		t.Fatalf("ledger after second Migrate = %v, want %v", got, files)
	}
	var again time.Time
	if err := s.DB().QueryRow(
		`SELECT applied_at FROM schema_migrations WHERE filename = '012_chat_encryption.sql'`).Scan(&again); err != nil {
		t.Fatalf("re-reading 012 ledger row: %v", err)
	}
	if !again.Equal(appliedAt) {
		t.Fatalf("012 ledger row was rewritten: %v -> %v", appliedAt, again)
	}
}

// TestMigrationLedgerRecordsPreExistingFiles covers the upgrade path: a
// database migrated by the pre-ledger binary must have every old file recorded
// (so none is ever replayed again) while 012 applies exactly once.
func TestMigrationLedgerRecordsPreExistingFiles(t *testing.T) {
	s := testScratchStore(t)
	ctx := context.Background()
	applyPreLedgerMigrations(t, s, "012")

	uid := seedScratchUser(t, s, "pre")
	var legacySpool int64
	if err := s.DB().QueryRowContext(ctx,
		`INSERT INTO offline_messages (from_user_id, to_user_id, from_unique_id, message)
		 VALUES ($1, $1, '', 'pre-4b plaintext') RETURNING id`, uid).Scan(&legacySpool); err != nil {
		t.Fatalf("seeding legacy spool row: %v", err)
	}
	var legacyChat int64
	if err := s.DB().QueryRowContext(ctx,
		`INSERT INTO chat_messages (scope, channel_id, from_unique_id, from_nickname, body)
		 VALUES (0, 0, 'u1', 'u1', 'legacy body') RETURNING id`).Scan(&legacyChat); err != nil {
		t.Fatalf("seeding legacy chat row: %v", err)
	}

	if err := s.Migrate(); err != nil {
		t.Fatalf("Migrate over a pre-ledger database: %v", err)
	}
	files := migrationNames(t)
	if got := ledgerRows(t, s); len(got) != len(files) {
		t.Fatalf("ledger = %v, want every file %v recorded", got, files)
	}
	if rowExists(t, s, "offline_messages", legacySpool) {
		t.Fatal("012 did not drop the undelivered pre-4b spool row")
	}

	// The chat body is NOT touched by the migration: sealing it in place is
	// the Go backfill's job, and the constraint is NOT VALID so the row lives.
	var body, bodyEnc string
	if err := s.DB().QueryRowContext(ctx,
		`SELECT body, body_enc FROM chat_messages WHERE id = $1`, legacyChat).Scan(&body, &bodyEnc); err != nil {
		t.Fatalf("reading legacy chat row: %v", err)
	}
	if body != "legacy body" || bodyEnc != "" {
		t.Fatalf("legacy chat row = (%q, %q), want the plaintext left for the backfill", body, bodyEnc)
	}

	sentinel := spoolSentinel(t, s, uid)
	if err := s.Migrate(); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if !rowExists(t, s, "offline_messages", sentinel) {
		t.Fatal("012 ran twice: the sentinel spool row was deleted")
	}
}

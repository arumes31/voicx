package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
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

// testAdditionalStore opens a distinct pool to the same scratch database,
// modeling a second voicx process for advisory-lock tests.
func testAdditionalStore(t *testing.T, existing *Store) *Store {
	t.Helper()
	var databaseName string
	if err := existing.DB().QueryRow(`SELECT current_database()`).Scan(&databaseName); err != nil {
		t.Fatalf("reading scratch database name: %v", err)
	}
	u, err := url.Parse(testDBURL())
	if err != nil {
		t.Fatalf("parsing test database URL: %v", err)
	}
	u.Path = "/" + databaseName
	u.RawPath = ""
	s, err := New(u.String(), zap.NewNop(), 2, 1, time.Minute)
	if err != nil {
		t.Fatalf("opening an additional scratch store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
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

func migrationChecksum(t *testing.T, name string) string {
	t.Helper()
	content, err := migrationsFS.ReadFile("migrations/" + name)
	if err != nil {
		t.Fatalf("reading migration %s: %v", name, err)
	}
	sum := sha256.Sum256(canonicalMigrationSQL(content))
	return hex.EncodeToString(sum[:])
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
			err = applyNonTransactionalMigration(context.Background(), s.DB(), string(content))
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
	defer func() { _ = rows.Close() }()
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

func assertLedgerChecksums(t *testing.T, s *Store, names []string) {
	t.Helper()
	rows, err := s.DB().Query(
		`SELECT filename, COALESCE(checksum, '') FROM schema_migrations ORDER BY filename`)
	if err != nil {
		t.Fatalf("reading migration checksums: %v", err)
	}
	defer func() { _ = rows.Close() }()

	got := make(map[string]string, len(names))
	for rows.Next() {
		var name, checksum string
		if err := rows.Scan(&name, &checksum); err != nil {
			t.Fatalf("scanning migration checksum: %v", err)
		}
		got[name] = checksum
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading migration checksums: %v", err)
	}
	if len(got) != len(names) {
		t.Fatalf("checksum row count = %d, want %d", len(got), len(names))
	}
	for _, name := range names {
		if got[name] != migrationChecksum(t, name) {
			t.Errorf("checksum for %s = %q, want SHA-256 of embedded bytes", name, got[name])
		}
	}
}

func assertLedgerChecksumInvariant(t *testing.T, s *Store) {
	t.Helper()
	var nullable string
	if err := s.DB().QueryRow(`SELECT is_nullable
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'schema_migrations'
		  AND column_name = 'checksum'`).Scan(&nullable); err != nil {
		t.Fatalf("reading migration checksum nullability: %v", err)
	}
	if nullable != "NO" {
		t.Fatalf("schema_migrations.checksum is_nullable = %q, want NO", nullable)
	}

	var validated bool
	if err := s.DB().QueryRow(`SELECT convalidated
		FROM pg_constraint
		WHERE conrelid = 'schema_migrations'::regclass
		  AND conname = $1
		  AND contype = 'c'`, migrationChecksumConstraint).Scan(&validated); err != nil {
		t.Fatalf("reading migration checksum constraint: %v", err)
	}
	if !validated {
		t.Fatal("migration checksum constraint is not validated")
	}
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
	assertLedgerChecksums(t, s, files)

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

func TestCanonicalMigrationSQLNormalizesLineEndings(t *testing.T) {
	want := []byte("SELECT 1;\nSELECT 2;\n")
	wantHash := sha256.Sum256(want)
	variants := map[string][]byte{
		"lf":      []byte("SELECT 1;\nSELECT 2;\n"),
		"crlf":    []byte("SELECT 1;\r\nSELECT 2;\r\n"),
		"bare-cr": []byte("SELECT 1;\rSELECT 2;\r"),
	}
	for name, input := range variants {
		t.Run(name, func(t *testing.T) {
			got := canonicalMigrationSQL(input)
			if !bytes.Equal(got, want) {
				t.Fatalf("canonical SQL = %q, want %q", got, want)
			}
			if gotHash := sha256.Sum256(got); gotHash != wantHash {
				t.Fatalf("canonical checksum = %x, want %x", gotHash, wantHash)
			}
		})
	}
}

func TestEmbeddedMigrationChecksums(t *testing.T) {
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	names := migrationNames(t)
	if len(migrations) != len(names) {
		t.Fatalf("loaded %d migrations, want %d", len(migrations), len(names))
	}
	for i, migration := range migrations {
		if migration.filename != names[i] {
			t.Fatalf("migration[%d] = %q, want %q", i, migration.filename, names[i])
		}
		if migration.checksum != migrationChecksum(t, migration.filename) {
			t.Errorf("checksum for %s is not the SHA-256 of its embedded bytes", migration.filename)
		}
		if bytes.ContainsRune(migration.content, '\r') {
			t.Errorf("migration %s was not canonicalized to LF", migration.filename)
		}
	}
}

func TestMigrationBackfillsLegacyChecksums(t *testing.T) {
	s := testScratchStore(t)
	// Legacy 016 changed shape when its columns and two concurrent indexes were
	// split. Materialize those historical effects so the compatibility path can
	// verify them before baselining its filename-only row.
	applyPreLedgerMigrations(t, s, "017")
	if _, err := s.DB().Exec(`CREATE TABLE schema_migrations (
		filename TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		t.Fatalf("creating pre-checksum migration ledger: %v", err)
	}

	names := migrationNames(t)
	for _, name := range names {
		if _, err := s.DB().Exec(
			`INSERT INTO schema_migrations (filename) VALUES ($1)`, name); err != nil {
			t.Fatalf("recording legacy migration %s: %v", name, err)
		}
	}

	if err := s.Migrate(); err != nil {
		t.Fatalf("backfilling migration checksums: %v", err)
	}
	assertLedgerChecksums(t, s, names)
	assertLedgerChecksumInvariant(t, s)

	for _, invalid := range []any{
		nil,
		"",
		strings.Repeat("A", 64),
		strings.Repeat("a", 63),
		strings.Repeat("g", 64),
	} {
		if _, err := s.DB().Exec(
			`UPDATE schema_migrations SET checksum = $2 WHERE filename = $1`,
			names[0], invalid); err == nil {
			t.Fatalf("post-upgrade checksum update to %#v was accepted", invalid)
		}
	}
	if _, err := s.DB().Exec(
		`INSERT INTO schema_migrations (filename) VALUES ('old_migrator.sql')`); err == nil {
		t.Fatal("old migrator insert without a checksum was accepted")
	}

	// Once populated, a later startup must validate without rewriting rows.
	if _, err := s.DB().Exec(`CREATE FUNCTION reject_schema_migration_update()
		RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'schema_migrations row was rewritten';
		END
		$$;
		CREATE TRIGGER reject_schema_migration_update
		BEFORE UPDATE ON schema_migrations
		FOR EACH ROW EXECUTE FUNCTION reject_schema_migration_update()`); err != nil {
		t.Fatalf("installing migration ledger update guard: %v", err)
	}
	if err := s.Migrate(); err != nil {
		t.Fatalf("validating already-backfilled checksums: %v", err)
	}
}

func TestMigrationRejectsChecksumDriftBeforeLegacyBackfill(t *testing.T) {
	s := testScratchStore(t)
	if _, err := s.DB().Exec(`CREATE TABLE schema_migrations (
		filename TEXT PRIMARY KEY,
		checksum TEXT,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		t.Fatalf("creating nullable-checksum migration ledger: %v", err)
	}

	names := migrationNames(t)
	nullName := names[0]
	emptyName := names[1]
	driftName := names[len(names)-1]
	const corruptChecksum = "not-the-embedded-sha256"
	for _, name := range names {
		var checksum any = migrationChecksum(t, name)
		switch name {
		case nullName:
			checksum = nil
		case emptyName:
			checksum = ""
		case driftName:
			checksum = corruptChecksum
		}
		if _, err := s.DB().Exec(
			`INSERT INTO schema_migrations (filename, checksum) VALUES ($1, $2)`,
			name, checksum); err != nil {
			t.Fatalf("recording legacy migration %s: %v", name, err)
		}
	}

	err := s.Migrate()
	if err == nil {
		t.Fatal("Migrate accepted checksum drift")
	}
	if !strings.Contains(err.Error(), "checksum drift") || !strings.Contains(err.Error(), driftName) {
		t.Fatalf("checksum drift error = %q, want filename and drift diagnosis", err)
	}
	var nullChecksum sql.NullString
	if err := s.DB().QueryRow(
		`SELECT checksum FROM schema_migrations WHERE filename = $1`, nullName).Scan(&nullChecksum); err != nil {
		t.Fatalf("reading NULL legacy checksum: %v", err)
	}
	if nullChecksum.Valid {
		t.Fatalf("NULL checksum was mutated before drift rejection: %q", nullChecksum.String)
	}
	var emptyChecksum string
	if err := s.DB().QueryRow(
		`SELECT checksum FROM schema_migrations WHERE filename = $1`, emptyName).Scan(&emptyChecksum); err != nil {
		t.Fatalf("reading empty legacy checksum: %v", err)
	}
	if emptyChecksum != "" {
		t.Fatalf("empty checksum was mutated before drift rejection: %q", emptyChecksum)
	}

	if _, err := s.DB().Exec(
		`UPDATE schema_migrations SET checksum = $2 WHERE filename = $1`,
		driftName, migrationChecksum(t, driftName)); err != nil {
		t.Fatalf("repairing migration checksum: %v", err)
	}
	if _, err := s.DB().Exec(`CREATE FUNCTION reject_second_checksum_backfill()
		RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF OLD.filename = '002_channel_security.sql' THEN
				RAISE EXCEPTION 'injected checksum backfill failure';
			END IF;
			RETURN NEW;
		END
		$$;
		CREATE TRIGGER reject_second_checksum_backfill
		BEFORE UPDATE ON schema_migrations
		FOR EACH ROW EXECUTE FUNCTION reject_second_checksum_backfill()`); err != nil {
		t.Fatalf("installing checksum backfill failure fixture: %v", err)
	}
	if err := s.Migrate(); err == nil {
		t.Fatal("migration checksum backfill succeeded despite injected second-row failure")
	}
	if err := s.DB().QueryRow(
		`SELECT checksum FROM schema_migrations WHERE filename = $1`, nullName).Scan(&nullChecksum); err != nil {
		t.Fatalf("re-reading NULL legacy checksum after rollback: %v", err)
	}
	if nullChecksum.Valid {
		t.Fatalf("checksum backfill was not atomic; first row retained %q", nullChecksum.String)
	}
	if _, err := s.DB().Exec(`DROP TRIGGER reject_second_checksum_backfill ON schema_migrations;
		DROP FUNCTION reject_second_checksum_backfill()`); err != nil {
		t.Fatalf("removing checksum backfill failure fixture: %v", err)
	}
	other := testAdditionalStore(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := other.MigrateContext(ctx); err != nil {
		t.Fatalf("migration after checksum failure (lock may be leaked): %v", err)
	}
	assertLedgerChecksums(t, other, names)
	assertLedgerChecksumInvariant(t, other)
}

func TestMigrationRejectsUnknownAppliedFilename(t *testing.T) {
	s := testScratchStore(t)
	if _, err := s.DB().Exec(`CREATE TABLE schema_migrations (
		filename TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		t.Fatalf("creating legacy migration ledger: %v", err)
	}
	const unknown = "999_future_binary.sql"
	known := migrationNames(t)[0]
	if _, err := s.DB().Exec(
		`INSERT INTO schema_migrations (filename) VALUES ($1), ($2)`, known, unknown); err != nil {
		t.Fatalf("recording unknown migration: %v", err)
	}

	err := s.Migrate()
	if err == nil {
		t.Fatal("Migrate accepted a filename absent from the embedded migrations")
	}
	if !strings.Contains(err.Error(), unknown) || !strings.Contains(err.Error(), "not embedded") {
		t.Fatalf("unknown migration error = %q, want filename and compatibility diagnosis", err)
	}
	var usersTable sql.NullString
	if err := s.DB().QueryRow(`SELECT to_regclass('users')::text`).Scan(&usersTable); err != nil {
		t.Fatalf("checking whether migrations ran: %v", err)
	}
	if usersTable.Valid {
		t.Fatalf("migration files ran before the unknown ledger row was rejected: users table = %q", usersTable.String)
	}
	var knownChecksum sql.NullString
	if err := s.DB().QueryRow(
		`SELECT checksum FROM schema_migrations WHERE filename = $1`, known).Scan(&knownChecksum); err != nil {
		t.Fatalf("reading known legacy checksum: %v", err)
	}
	if knownChecksum.Valid {
		t.Fatalf("known checksum was backfilled before unknown migration rejection: %q", knownChecksum.String)
	}

	if _, err := s.DB().Exec(`DELETE FROM schema_migrations`); err != nil {
		t.Fatalf("removing migration ledger fixtures: %v", err)
	}
	other := testAdditionalStore(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := other.MigrateContext(ctx); err != nil {
		t.Fatalf("migration after unknown-file failure (lock may be leaked): %v", err)
	}
}

func TestMigrateSerializesConcurrentStores(t *testing.T) {
	first := testScratchStore(t)
	second := testAdditionalStore(t, first)
	holdCtx, cancelHold := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelHold()
	lockConn, err := first.DB().Conn(holdCtx)
	if err != nil {
		t.Fatalf("reserving lock-holder connection: %v", err)
	}
	locked := false
	defer func() {
		if locked {
			_ = releaseMigrationLock(lockConn)
		}
		_ = lockConn.Close()
	}()
	if err := acquireMigrationLock(holdCtx, lockConn, nil); err != nil {
		t.Fatalf("acquiring explicit migration lock: %v", err)
	}
	locked = true

	waitCtx, cancelWait := context.WithCancel(context.Background())
	waitResult := make(chan error, 1)
	waiting := make(chan struct{})
	var waitingOnce sync.Once
	second.migrationLockWaitHook = func() { waitingOnce.Do(func() { close(waiting) }) }
	go func() { waitResult <- second.MigrateContext(waitCtx) }()

	select {
	case err := <-waitResult:
		t.Fatalf("migration waiter returned before advisory lock release: %v", err)
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("migration waiter did not rendezvous after observing the held advisory lock")
	}
	select {
	case err := <-waitResult:
		t.Fatalf("migration waiter returned while advisory lock remained held: %v", err)
	default:
	}
	cancelWait()
	select {
	case err := <-waitResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled migration waiter error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled migration waiter did not return")
	}

	if err := releaseMigrationLock(lockConn); err != nil {
		t.Fatalf("releasing explicit migration lock: %v", err)
	}
	locked = false
	if err := lockConn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("closing explicit lock connection: %v", err)
	}

	recoveryCtx, cancelRecovery := context.WithTimeout(context.Background(), time.Minute)
	defer cancelRecovery()
	if err := second.MigrateContext(recoveryCtx); err != nil {
		t.Fatalf("MigrateContext after canceled waiter: %v", err)
	}

	names := migrationNames(t)
	if got := ledgerRows(t, second); len(got) != len(names) {
		t.Fatalf("ledger after concurrent migration = %v, want %v", got, names)
	}
	assertLedgerChecksums(t, second, names)
}

func TestMigrationRepairsInvalidConcurrentIndex(t *testing.T) {
	s := testScratchStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := s.MigrateContext(ctx); err != nil {
		t.Fatalf("initial MigrateContext: %v", err)
	}

	const (
		migration = "016_chat_consistency.sql"
		indexName = "idx_chat_messages_client_msg_id"
	)
	if _, err := s.DB().ExecContext(ctx,
		`DELETE FROM schema_migrations WHERE filename = $1`, migration); err != nil {
		t.Fatalf("removing concurrent-index migration ledger row: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx,
		`DROP INDEX CONCURRENTLY `+indexName); err != nil {
		t.Fatalf("dropping existing concurrent index: %v", err)
	}

	var duplicateIDs [2]int64
	for i := range duplicateIDs {
		if err := s.DB().QueryRowContext(ctx, `INSERT INTO chat_messages
			(scope, channel_id, from_unique_id, deleted_at, client_msg_id)
			VALUES (0, 0, 'duplicate-sender', NOW(), 'duplicate-client-id')
			RETURNING id`).Scan(&duplicateIDs[i]); err != nil {
			t.Fatalf("inserting duplicate chat row %d: %v", i, err)
		}
	}

	err := s.MigrateContext(ctx)
	if err == nil {
		t.Fatal("concurrent unique-index migration unexpectedly accepted duplicate rows")
	}
	var ledgerCount int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM schema_migrations WHERE filename = $1`, migration).Scan(&ledgerCount); err != nil {
		t.Fatalf("checking failed migration ledger row: %v", err)
	}
	if ledgerCount != 0 {
		t.Fatalf("failed migration ledger count = %d, want 0", ledgerCount)
	}
	var valid bool
	if err := s.DB().QueryRowContext(ctx, `SELECT indisvalid
		FROM pg_index WHERE indexrelid = $1::regclass`, indexName).Scan(&valid); err != nil {
		t.Fatalf("reading leftover concurrent index: %v", err)
	}
	if valid {
		t.Fatal("failed concurrent unique index is unexpectedly valid")
	}

	if _, err := s.DB().ExecContext(ctx,
		`DELETE FROM chat_messages WHERE id = $1`, duplicateIDs[1]); err != nil {
		t.Fatalf("repairing duplicate chat data: %v", err)
	}
	if err := s.MigrateContext(ctx); err != nil {
		t.Fatalf("rerunning migration with invalid leftover index: %v", err)
	}
	if err := s.DB().QueryRowContext(ctx, `SELECT indisvalid
		FROM pg_index WHERE indexrelid = $1::regclass`, indexName).Scan(&valid); err != nil {
		t.Fatalf("reading repaired concurrent index: %v", err)
	}
	if !valid {
		t.Fatal("repaired concurrent unique index is invalid")
	}
	var checksum string
	if err := s.DB().QueryRowContext(ctx, `SELECT checksum
		FROM schema_migrations WHERE filename = $1`, migration).Scan(&checksum); err != nil {
		t.Fatalf("reading repaired migration ledger row: %v", err)
	}
	if checksum != migrationChecksum(t, migration) {
		t.Fatalf("repaired migration checksum = %q, want canonical embedded checksum", checksum)
	}

	// IF NOT EXISTS may report success when a non-index relation owns the
	// expected name. Verification must still refuse to record the migration.
	if _, err := s.DB().ExecContext(ctx,
		`DELETE FROM schema_migrations WHERE filename = $1`, migration); err != nil {
		t.Fatalf("removing repaired migration ledger row: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx,
		`DROP INDEX CONCURRENTLY `+indexName); err != nil {
		t.Fatalf("dropping repaired concurrent index: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx,
		`CREATE TABLE `+indexName+` (id INTEGER)`); err != nil {
		t.Fatalf("creating expected-index name collision: %v", err)
	}
	err = s.MigrateContext(ctx)
	if err == nil || !strings.Contains(err.Error(), "missing after creation") {
		t.Fatalf("missing concurrent index error = %v, want verification failure", err)
	}
	if err := s.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM schema_migrations WHERE filename = $1`, migration).Scan(&ledgerCount); err != nil {
		t.Fatalf("checking missing-index migration ledger row: %v", err)
	}
	if ledgerCount != 0 {
		t.Fatalf("missing-index migration ledger count = %d, want 0", ledgerCount)
	}
	if _, err := s.DB().ExecContext(ctx, `DROP TABLE `+indexName); err != nil {
		t.Fatalf("removing expected-index name collision: %v", err)
	}
	if err := s.MigrateContext(ctx); err != nil {
		t.Fatalf("rerunning migration after missing-index repair: %v", err)
	}
	if err := s.DB().QueryRowContext(ctx, `SELECT indisvalid
		FROM pg_index WHERE indexrelid = $1::regclass`, indexName).Scan(&valid); err != nil {
		t.Fatalf("reading recreated concurrent index: %v", err)
	}
	if !valid {
		t.Fatal("recreated concurrent unique index is invalid")
	}
}

func TestMigrationRejectsValidConcurrentIndexWithWrongDefinition(t *testing.T) {
	s := testScratchStore(t)
	initialCtx, cancelInitial := context.WithTimeout(context.Background(), time.Minute)
	if err := s.MigrateContext(initialCtx); err != nil {
		cancelInitial()
		t.Fatalf("initial MigrateContext: %v", err)
	}
	cancelInitial()

	tests := []struct {
		name, migration, indexName, create, difference string
	}{
		{
			name:       "uniqueness",
			migration:  "016_chat_consistency.sql",
			indexName:  "idx_chat_messages_client_msg_id",
			difference: "unique",
			create: `CREATE INDEX idx_chat_messages_client_msg_id
				ON public.chat_messages (channel_id, from_unique_id, client_msg_id)
				WHERE client_msg_id IS NOT NULL AND client_msg_id <> ''`,
		},
		{
			name:       "ordered columns",
			migration:  "016_chat_consistency.sql",
			indexName:  "idx_chat_messages_client_msg_id",
			difference: "ordered key/INCLUDE elements",
			create: `CREATE UNIQUE INDEX idx_chat_messages_client_msg_id
				ON public.chat_messages (from_unique_id, channel_id, client_msg_id)
				WHERE client_msg_id IS NOT NULL AND client_msg_id <> ''`,
		},
		{
			name:       "predicate",
			migration:  "016_chat_consistency.sql",
			indexName:  "idx_chat_messages_client_msg_id",
			difference: "predicate",
			create: `CREATE UNIQUE INDEX idx_chat_messages_client_msg_id
				ON public.chat_messages (channel_id, from_unique_id, client_msg_id)
				WHERE client_msg_id IS NOT NULL`,
		},
		{
			name:       "access method",
			migration:  "016z_chat_reply_index.sql",
			indexName:  "idx_chat_messages_reply_to",
			difference: "access method",
			create: `CREATE INDEX idx_chat_messages_reply_to
				ON public.chat_messages USING hash (reply_to_id)
				WHERE reply_to_id IS NOT NULL`,
		},
		{
			name:       "include columns",
			migration:  "016_chat_consistency.sql",
			indexName:  "idx_chat_messages_client_msg_id",
			difference: "ordered key/INCLUDE elements",
			create: `CREATE UNIQUE INDEX idx_chat_messages_client_msg_id
				ON public.chat_messages (channel_id, from_unique_id, client_msg_id)
				INCLUDE (id)
				WHERE client_msg_id IS NOT NULL AND client_msg_id <> ''`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if _, err := s.DB().ExecContext(ctx,
				`DELETE FROM schema_migrations WHERE filename = $1`, tt.migration); err != nil {
				t.Fatalf("removing migration ledger row: %v", err)
			}
			drop := "DROP INDEX CONCURRENTLY public." + tt.indexName
			if _, err := s.DB().ExecContext(ctx, drop); err != nil {
				t.Fatalf("dropping expected index: %v", err)
			}
			if _, err := s.DB().ExecContext(ctx, tt.create); err != nil {
				t.Fatalf("creating valid index with wrong definition: %v", err)
			}
			var wrongOID int64
			if err := s.DB().QueryRowContext(ctx, `SELECT c.oid::bigint
				FROM pg_class AS c
				JOIN pg_namespace AS n ON n.oid = c.relnamespace
				WHERE n.nspname = 'public' AND c.relname = $1`, tt.indexName).Scan(&wrongOID); err != nil {
				t.Fatalf("reading wrong index OID: %v", err)
			}
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
				defer cleanupCancel()
				_, _ = s.DB().ExecContext(cleanupCtx,
					"DROP INDEX CONCURRENTLY IF EXISTS public."+tt.indexName)
				if err := s.MigrateContext(cleanupCtx); err != nil {
					t.Errorf("restoring correct index after case: %v", err)
				}
			})

			err := s.MigrateContext(ctx)
			if err == nil || !strings.Contains(err.Error(), "wrong definition") ||
				!strings.Contains(err.Error(), tt.difference) {
				t.Fatalf("wrong-definition migration error = %v, want %q mismatch", err, tt.difference)
			}
			var (
				ledgerCount int
				afterOID    int64
				valid       bool
			)
			if err := s.DB().QueryRowContext(ctx,
				`SELECT count(*) FROM schema_migrations WHERE filename = $1`, tt.migration).
				Scan(&ledgerCount); err != nil {
				t.Fatalf("checking rejected migration ledger: %v", err)
			}
			if ledgerCount != 0 {
				t.Fatalf("rejected migration ledger count = %d, want 0", ledgerCount)
			}
			if err := s.DB().QueryRowContext(ctx, `SELECT c.oid::bigint, i.indisvalid
				FROM pg_class AS c
				JOIN pg_namespace AS n ON n.oid = c.relnamespace
				JOIN pg_index AS i ON i.indexrelid = c.oid
				WHERE n.nspname = 'public' AND c.relname = $1`, tt.indexName).
				Scan(&afterOID, &valid); err != nil {
				t.Fatalf("reading rejected valid index: %v", err)
			}
			if afterOID != wrongOID || !valid {
				t.Fatalf("wrong valid index was modified: OID/valid = (%d, %t), want (%d, true)",
					afterOID, valid, wrongOID)
			}
		})
	}
}

func TestMigrationIgnoresShadowSchemaIndex(t *testing.T) {
	s := testScratchStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := s.MigrateContext(ctx); err != nil {
		t.Fatalf("initial MigrateContext: %v", err)
	}
	const (
		migration = "016_chat_consistency.sql"
		indexName = "idx_chat_messages_client_msg_id"
	)
	if _, err := s.DB().ExecContext(ctx,
		`DELETE FROM schema_migrations WHERE filename = $1`, migration); err != nil {
		t.Fatalf("removing migration ledger row: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx,
		`DROP INDEX CONCURRENTLY public.idx_chat_messages_client_msg_id`); err != nil {
		t.Fatalf("dropping public index: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, `CREATE SCHEMA shadow;
		CREATE TABLE shadow.holder (client_msg_id TEXT);
		CREATE INDEX idx_chat_messages_client_msg_id ON shadow.holder (client_msg_id)`); err != nil {
		t.Fatalf("creating shadow-schema index: %v", err)
	}

	conn, err := s.DB().Conn(ctx)
	if err != nil {
		t.Fatalf("reserving migration connection: %v", err)
	}
	locked := false
	defer func() {
		if locked {
			_ = releaseMigrationLock(conn)
		}
		_ = conn.Close()
	}()
	if _, err := conn.ExecContext(ctx, `SET search_path = shadow, public`); err != nil {
		t.Fatalf("setting shadowed search_path: %v", err)
	}
	if err := acquireMigrationLock(ctx, conn, nil); err != nil {
		t.Fatalf("acquiring migration advisory lock: %v", err)
	}
	locked = true
	if err := s.migrateLocked(ctx, conn); err != nil {
		t.Fatalf("migrating with shadowed search_path: %v", err)
	}
	if err := releaseMigrationLock(conn); err != nil {
		t.Fatalf("releasing migration advisory lock: %v", err)
	}
	locked = false

	var publicTarget, shadowTarget string
	if err := s.DB().QueryRowContext(ctx, `SELECT n.nspname || '.' || t.relname
		FROM pg_class AS i
		JOIN pg_namespace AS inode ON inode.oid = i.relnamespace
		JOIN pg_index AS x ON x.indexrelid = i.oid
		JOIN pg_class AS t ON t.oid = x.indrelid
		JOIN pg_namespace AS n ON n.oid = t.relnamespace
		WHERE inode.nspname = 'public' AND i.relname = $1`, indexName).Scan(&publicTarget); err != nil {
		t.Fatalf("reading exact public index target: %v", err)
	}
	if err := s.DB().QueryRowContext(ctx, `SELECT n.nspname || '.' || t.relname
		FROM pg_class AS i
		JOIN pg_namespace AS inode ON inode.oid = i.relnamespace
		JOIN pg_index AS x ON x.indexrelid = i.oid
		JOIN pg_class AS t ON t.oid = x.indrelid
		JOIN pg_namespace AS n ON n.oid = t.relnamespace
		WHERE inode.nspname = 'shadow' AND i.relname = $1`, indexName).Scan(&shadowTarget); err != nil {
		t.Fatalf("reading exact shadow index target: %v", err)
	}
	if publicTarget != "public.chat_messages" || shadowTarget != "shadow.holder" {
		t.Fatalf("index targets = (%q, %q), want exact public and untouched shadow targets",
			publicTarget, shadowTarget)
	}
	var checksum string
	if err := s.DB().QueryRowContext(ctx,
		`SELECT checksum FROM public.schema_migrations WHERE filename = $1`, migration).
		Scan(&checksum); err != nil {
		t.Fatalf("reading shadow-safe migration ledger row: %v", err)
	}
	if checksum != migrationChecksum(t, migration) {
		t.Fatalf("shadow-safe migration checksum = %q, want embedded checksum", checksum)
	}
}

// TestMigrationWidensKEKID preserves key-ring ids that older binaries stored
// through signed SMALLINT. It also proves the migrated column accepts every
// value representable by the application's uint16 KEK id.
func TestMigrationWidensKEKID(t *testing.T) {
	s := testScratchStore(t)
	applyPreLedgerMigrations(t, s, "024")
	if _, err := s.DB().Exec(`CREATE TABLE schema_migrations (
		filename TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		t.Fatalf("creating legacy migration ledger: %v", err)
	}
	for _, name := range migrationNames(t) {
		if name >= "024" {
			break
		}
		if _, err := s.DB().Exec(`INSERT INTO schema_migrations (filename) VALUES ($1)`, name); err != nil {
			t.Fatalf("recording legacy migration %s: %v", name, err)
		}
	}

	// Recreate the exact pre-024 type. The current 012 migration uses INTEGER
	// for fresh databases, while existing deployments still arrive as SMALLINT.
	if _, err := s.DB().Exec(`ALTER TABLE chat_scope_keys
		ALTER COLUMN kek_id TYPE SMALLINT USING kek_id::SMALLINT`); err != nil {
		t.Fatalf("restoring legacy kek_id type: %v", err)
	}
	if _, err := s.DB().Exec(`INSERT INTO chat_scope_keys
		(scope_id, key_id, wrapped_key, kek_id) VALUES (9001, 1, '\x01', -1)`); err != nil {
		t.Fatalf("seeding signed legacy kek id: %v", err)
	}

	if err := s.Migrate(); err != nil {
		t.Fatalf("migrating legacy kek id: %v", err)
	}
	var (
		dataType string
		kekID    int64
	)
	if err := s.DB().QueryRow(`SELECT data_type FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'chat_scope_keys' AND column_name = 'kek_id'`).Scan(&dataType); err != nil {
		t.Fatalf("reading kek_id type: %v", err)
	}
	if dataType != "integer" {
		t.Fatalf("kek_id type = %q, want integer", dataType)
	}
	if err := s.DB().QueryRow(`SELECT kek_id FROM chat_scope_keys WHERE scope_id = 9001`).Scan(&kekID); err != nil {
		t.Fatalf("reading migrated kek id: %v", err)
	}
	if kekID != 65535 {
		t.Fatalf("migrated kek id = %d, want 65535", kekID)
	}
	if _, err := s.DB().Exec(`INSERT INTO chat_scope_keys
		(scope_id, key_id, wrapped_key, kek_id) VALUES (9002, 1, '\x02', 65535)`); err != nil {
		t.Fatalf("inserting maximum uint16 kek id: %v", err)
	}
	if _, err := s.DB().Exec(`INSERT INTO chat_scope_keys
		(scope_id, key_id, wrapped_key, kek_id) VALUES (9003, 1, '\x03', 65536)`); err == nil {
		t.Fatal("kek id above uint16 range was accepted")
	}
	assertLedgerChecksums(t, s, migrationNames(t))
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

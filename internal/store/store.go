package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
	"go.uber.org/zap"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationAdvisoryLockID is stable across binaries so every voicx process
// serializes migrations for the same PostgreSQL database.
const migrationAdvisoryLockID int64 = 0x766f6963786d6967 // "voicxmig"

const migrationChecksumConstraint = "schema_migrations_checksum_sha256"

const checksumConstraintProbe = "voicx_migration_checksum_probe_check"

type embeddedMigration struct {
	filename         string
	content          []byte
	checksum         string
	nonTransactional bool
}

type appliedMigration struct {
	filename string
	checksum sql.NullString
}

// Store wraps a *sql.DB connection pool and a logger, providing access to the
// voicx PostgreSQL database.
type Store struct {
	db                *sql.DB
	logger            *zap.Logger
	reactionCache     sync.Map // message id -> *reactionCacheEntry
	reactionCacheMu   sync.Mutex
	reactionCacheSize int
	pii               *PIICipher
	// migrationLockWaitHook is a test seam invoked after an advisory-lock
	// attempt observes the lock held by another session.
	migrationLockWaitHook func()
}

// SetPIICipher installs the process-local field cipher used by PII methods.
func (s *Store) SetPIICipher(cipher *PIICipher) { s.pii = cipher }

// SchemaVersion returns the newest applied migration filename. An empty
// string means the migration ledger exists but has no applied files yet.
func (s *Store) SchemaVersion(ctx context.Context) (string, error) {
	const q = `SELECT COALESCE(MAX(filename), '') FROM public.schema_migrations`
	var version string
	if err := s.db.QueryRowContext(ctx, q).Scan(&version); err != nil {
		return "", fmt.Errorf("reading schema version: %w", err)
	}
	return version, nil
}

// New opens the database at databaseURL using the lib/pq driver, configures the
// connection pool, and pings the database to verify connectivity.
func New(databaseURL string, logger *zap.Logger, maxOpen, maxIdle int, connMaxLifetime time.Duration) (*Store, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	if maxOpen > 0 {
		db.SetMaxOpenConns(maxOpen)
	}
	if maxIdle > 0 {
		db.SetMaxIdleConns(maxIdle)
	}
	if connMaxLifetime > 0 {
		db.SetConnMaxLifetime(connMaxLifetime)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	return &Store{db: db, logger: logger}, nil
}

// Migrate applies every embedded migration exactly once, in lexical order. A
// PostgreSQL advisory lock serializes the complete operation across processes,
// and each ledger row records the SHA-256 checksum of canonical embedded SQL.
// Files applied before this ledger existed are re-applied once and then
// recorded; every migration up to 011 is idempotent DDL, so that single replay
// is safe. From 012 onward a migration MAY contain destructive DML, because the
// ledger guarantees it runs exactly once.
func (s *Store) Migrate() error {
	return s.MigrateContext(context.Background())
}

// MigrateContext applies embedded migrations like Migrate, but also lets a
// caller bound advisory-lock waiting and database work during startup.
func (s *Store) MigrateContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("migrating schema: nil context")
	}
	return s.migrate(ctx)
}

func (s *Store) migrate(ctx context.Context) (retErr error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserving migration connection: %w", err)
	}
	defer func() {
		if err := conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
			retErr = errors.Join(retErr, fmt.Errorf("closing migration connection: %w", err))
		}
	}()

	if err := acquireMigrationLock(ctx, conn, s.migrationLockWaitHook); err != nil {
		return err
	}
	defer func() {
		if err := releaseMigrationLock(conn); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()

	return s.migrateLocked(ctx, conn)
}

func (s *Store) migrateLocked(ctx context.Context, conn *sql.Conn) error {
	// Leave pg_catalog implicit so PostgreSQL searches it before public, and
	// list pg_temp last so temporary relations cannot shadow application tables.
	// Normalize parse/storage settings that otherwise alter embedded SQL.
	if _, err := conn.ExecContext(ctx, `SELECT
		pg_catalog.set_config('search_path', 'public, pg_temp', false),
		pg_catalog.set_config('default_tablespace', '', false),
		pg_catalog.set_config('temp_tablespaces', '', false),
		pg_catalog.set_config('standard_conforming_strings', 'on', false)`); err != nil {
		return fmt.Errorf("securing migration session: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS public.schema_migrations (
	    filename   TEXT PRIMARY KEY,
	    checksum   TEXT NOT NULL,
	    applied_at TIMESTAMPTZ NOT NULL DEFAULT pg_catalog.now(),
	    CONSTRAINT schema_migrations_checksum_sha256
	        CHECK (checksum OPERATOR(pg_catalog.~) '^[0-9a-f]{64}$')
	)`); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}
	if _, err := conn.ExecContext(ctx,
		`ALTER TABLE public.schema_migrations ADD COLUMN IF NOT EXISTS checksum TEXT`); err != nil {
		return fmt.Errorf("adding schema_migrations checksum: %w", err)
	}

	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		return err
	}
	embeddedByName := make(map[string]embeddedMigration, len(migrations))
	for _, migration := range migrations {
		embeddedByName[migration.filename] = migration
	}

	applied, err := appliedMigrations(ctx, conn)
	if err != nil {
		return err
	}
	appliedNames := make(map[string]struct{}, len(applied))
	for _, record := range applied {
		if _, ok := embeddedByName[record.filename]; !ok {
			return fmt.Errorf(
				"applied migration %q is not embedded; the database may have been migrated by a newer binary",
				record.filename,
			)
		}
		appliedNames[record.filename] = struct{}{}
	}

	legacyBaselines := make(map[string]bool)
	for _, record := range applied {
		migration := embeddedByName[record.filename]
		if record.filename == legacyChatConsistencyFilename &&
			(!record.checksum.Valid || record.checksum.String == "" ||
				record.checksum.String == legacyChatConsistencyChecksum) {
			if err := repairLegacyChatConsistency(ctx, conn, embeddedByName); err != nil {
				return fmt.Errorf("establishing legacy %s compatibility: %w", record.filename, err)
			}
			legacyBaselines[record.filename] = true
			continue
		}
		if !record.checksum.Valid || record.checksum.String == "" {
			continue
		}
		if record.checksum.String != migration.checksum {
			return fmt.Errorf(
				"migration %q checksum drift: database has %q, embedded file has %q",
				record.filename, record.checksum.String, migration.checksum,
			)
		}
	}
	if err := finalizeMigrationLedger(ctx, conn, applied, embeddedByName, legacyBaselines); err != nil {
		return err
	}

	for _, migration := range migrations {
		if _, ok := appliedNames[migration.filename]; ok {
			continue
		}
		if migration.nonTransactional {
			if err := applyNonTransactionalMigration(ctx, conn, string(migration.content)); err != nil {
				return fmt.Errorf("applying non-transactional migration %s: %w", migration.filename, err)
			}
			if _, err := conn.ExecContext(ctx,
				`INSERT INTO public.schema_migrations (filename, checksum) VALUES ($1, $2)`,
				migration.filename, migration.checksum); err != nil {
				return fmt.Errorf("recording migration %s: %w", migration.filename, err)
			}
		} else if err := applyTransactionalMigration(ctx, conn, migration); err != nil {
			return err
		}

		if s.logger != nil {
			s.logger.Info("migration applied", zap.String("file", migration.filename))
		}
	}
	return nil
}

func loadEmbeddedMigrations() ([]embeddedMigration, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("reading migrations directory: %w", err)
	}

	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)

	migrations := make([]embeddedMigration, 0, len(files))
	for _, filename := range files {
		rawContent, err := migrationsFS.ReadFile("migrations/" + filename)
		if err != nil {
			return nil, fmt.Errorf("reading migration %s: %w", filename, err)
		}
		content := canonicalMigrationSQL(rawContent)
		migrations = append(migrations, embeddedMigration{
			filename:         filename,
			content:          content,
			checksum:         fmt.Sprintf("%x", sha256.Sum256(content)),
			nonTransactional: noTransactionMigration(content),
		})
	}
	return migrations, nil
}

// canonicalMigrationSQL normalizes all text line endings in one pass. The
// returned bytes are both executed and hashed, keeping checksums identical
// across Git checkouts on different operating systems.
func canonicalMigrationSQL(content []byte) []byte {
	firstCR := -1
	for i, b := range content {
		if b == '\r' {
			firstCR = i
			break
		}
	}
	if firstCR < 0 {
		return content
	}

	canonical := make([]byte, 0, len(content))
	canonical = append(canonical, content[:firstCR]...)
	for i := firstCR; i < len(content); i++ {
		if content[i] != '\r' {
			canonical = append(canonical, content[i])
			continue
		}
		canonical = append(canonical, '\n')
		if i+1 < len(content) && content[i+1] == '\n' {
			i++
		}
	}
	return canonical
}

func applyTransactionalMigration(ctx context.Context, conn *sql.Conn, migration embeddedMigration) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning migration %s: %w", migration.filename, err)
	}
	if _, err := tx.ExecContext(ctx, string(migration.content)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("applying migration %s: %w", migration.filename, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO public.schema_migrations (filename, checksum) VALUES ($1, $2)`,
		migration.filename, migration.checksum); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("recording migration %s: %w", migration.filename, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing migration %s: %w", migration.filename, err)
	}
	return nil
}

// noTransactionMigration reports whether a migration file starts with the
// `-- voicx:no-transaction` marker.
//
// A marked file must contain exactly one idempotent CREATE [UNIQUE] INDEX
// CONCURRENTLY statement. The runner removes an invalid interrupted index only
// when its schema, name, and target table exactly match the embedded statement;
// a valid mismatch fails closed for operator review.
func noTransactionMigration(content []byte) bool {
	remaining := string(content)
	for remaining != "" {
		line := remaining
		if end := strings.IndexByte(remaining, '\n'); end >= 0 {
			line, remaining = remaining[:end], remaining[end+1:]
		} else {
			remaining = ""
		}
		line = strings.TrimSpace(line)
		if line != "" {
			return line == "-- voicx:no-transaction"
		}
	}
	return false
}

type migrationExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// applyNonTransactionalMigration executes one verified concurrent-index
// statement through a dedicated connection, preserving its autocommit
// requirement without giving up the session-level advisory lock.
func applyNonTransactionalMigration(ctx context.Context, execer migrationExecer, content string) error {
	spec, err := parseNonTransactionalMigration(content)
	if err != nil {
		return err
	}
	expected, err := prepareConcurrentIndex(ctx, execer, spec)
	if err != nil {
		return err
	}
	if !expected.alreadyValid {
		if _, err := execer.ExecContext(ctx, spec.statement); err != nil {
			return err
		}
	}
	return verifyConcurrentIndex(ctx, execer, expected)
}

// appliedMigrations reads the ledger through the connection that owns the
// migration advisory lock.
func appliedMigrations(ctx context.Context, conn *sql.Conn) ([]appliedMigration, error) {
	rows, err := conn.QueryContext(ctx,
		`SELECT filename, checksum FROM public.schema_migrations ORDER BY filename`)
	if err != nil {
		return nil, fmt.Errorf("reading schema_migrations: %w", err)
	}
	defer closeRows(rows)
	out := make([]appliedMigration, 0)
	for rows.Next() {
		var record appliedMigration
		if err := rows.Scan(&record.filename, &record.checksum); err != nil {
			return nil, fmt.Errorf("scanning schema_migrations: %w", err)
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating schema_migrations: %w", err)
	}
	return out, nil
}

func finalizeMigrationLedger(
	ctx context.Context,
	conn *sql.Conn,
	applied []appliedMigration,
	embeddedByName map[string]embeddedMigration,
	legacyBaselines map[string]bool,
) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning migration checksum backfill: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, record := range applied {
		legacyBaseline := legacyBaselines[record.filename]
		if record.checksum.Valid && record.checksum.String != "" && !legacyBaseline {
			continue
		}
		query := `UPDATE public.schema_migrations
			SET checksum = $2
			WHERE filename = $1 AND (checksum IS NULL OR checksum = '')`
		arguments := []any{record.filename, embeddedByName[record.filename].checksum}
		if legacyBaseline {
			query = `UPDATE public.schema_migrations
				SET checksum = $2
				WHERE filename = $1
				  AND (checksum IS NULL OR checksum = '' OR checksum = $3)`
			arguments = append(arguments, legacyChatConsistencyChecksum)
		}
		result, err := tx.ExecContext(ctx, query, arguments...)
		if err != nil {
			return fmt.Errorf("backfilling checksum for migration %s: %w", record.filename, err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("checking checksum backfill for migration %s: %w", record.filename, err)
		}
		if updated != 1 {
			return fmt.Errorf(
				"backfilling checksum for migration %s: updated %d rows, want 1",
				record.filename, updated,
			)
		}
	}

	// This intentionally rejects inserts from older migrators that do not
	// populate checksums during a mixed-version rollout.
	if _, err := tx.ExecContext(ctx,
		`ALTER TABLE public.schema_migrations ALTER COLUMN checksum SET NOT NULL`); err != nil {
		return fmt.Errorf("requiring migration checksums: %w", err)
	}

	expectedConstraint, err := expectedChecksumConstraintDefinition(ctx, tx)
	if err != nil {
		return err
	}
	var (
		constraintValidated  bool
		constraintDefinition string
	)
	err = tx.QueryRowContext(ctx, `SELECT convalidated,
		       pg_catalog.pg_get_constraintdef(oid, false)
		FROM pg_catalog.pg_constraint
		WHERE conrelid = 'public.schema_migrations'::pg_catalog.regclass
		  AND conname = $1`, migrationChecksumConstraint).Scan(
		&constraintValidated, &constraintDefinition,
	)
	if err == nil && constraintDefinition != expectedConstraint {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE public.schema_migrations
			DROP CONSTRAINT schema_migrations_checksum_sha256`); err != nil {
			return fmt.Errorf("replacing mismatched migration checksum constraint: %w", err)
		}
		err = sql.ErrNoRows
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `ALTER TABLE public.schema_migrations
			ADD CONSTRAINT schema_migrations_checksum_sha256
			CHECK (checksum OPERATOR(pg_catalog.~) '^[0-9a-f]{64}$') NOT VALID`); err != nil {
			return fmt.Errorf("adding migration checksum constraint: %w", err)
		}
		constraintValidated = false
	case err != nil:
		return fmt.Errorf("checking migration checksum constraint: %w", err)
	}
	if !constraintValidated {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE public.schema_migrations
			VALIDATE CONSTRAINT schema_migrations_checksum_sha256`); err != nil {
			return fmt.Errorf("validating migration checksum constraint: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing migration checksum backfill: %w", err)
	}
	return nil
}

func expectedChecksumConstraintDefinition(ctx context.Context, tx *sql.Tx) (string, error) {
	if _, err := tx.ExecContext(ctx,
		`DROP TABLE IF EXISTS pg_temp.voicx_migration_checksum_probe`); err != nil {
		return "", fmt.Errorf("dropping migration checksum constraint probe: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE pg_temp.voicx_migration_checksum_probe (
		checksum TEXT,
		CONSTRAINT voicx_migration_checksum_probe_check
			CHECK (checksum OPERATOR(pg_catalog.~) '^[0-9a-f]{64}$')
	) ON COMMIT DROP`); err != nil {
		return "", fmt.Errorf("creating migration checksum constraint probe: %w", err)
	}
	var definition string
	if err := tx.QueryRowContext(ctx, `SELECT pg_catalog.pg_get_constraintdef(c.oid, false)
		FROM pg_catalog.pg_constraint AS c
		JOIN pg_catalog.pg_class AS r ON r.oid = c.conrelid
		WHERE r.relnamespace = pg_catalog.pg_my_temp_schema()
		  AND r.relname = 'voicx_migration_checksum_probe'
		  AND c.conname = $1`, checksumConstraintProbe).Scan(&definition); err != nil {
		return "", fmt.Errorf("reading migration checksum constraint probe: %w", err)
	}
	return definition, nil
}

func acquireMigrationLock(ctx context.Context, conn *sql.Conn, onWait func()) error {
	const retryInterval = 100 * time.Millisecond
	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()

	for {
		var acquired bool
		if err := conn.QueryRowContext(ctx,
			`SELECT pg_catalog.pg_try_advisory_lock($1)`, migrationAdvisoryLockID).Scan(&acquired); err != nil {
			// Cancellation can race with PostgreSQL granting the lock. Discarding
			// the physical session makes either outcome safe.
			discardConnection(conn)
			return fmt.Errorf("acquiring migration advisory lock: %w", err)
		}
		if acquired {
			return nil
		}
		if onWait != nil {
			onWait()
		}

		// A blocking pg_advisory_lock call leaves its statement transaction
		// open while waiting. CREATE INDEX CONCURRENTLY in the lock holder can
		// then wait on that transaction and deadlock. Polling lets every failed
		// attempt finish before the holder advances.
		select {
		case <-ctx.Done():
			return fmt.Errorf("acquiring migration advisory lock: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func releaseMigrationLock(conn *sql.Conn) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var unlocked bool
	if err := conn.QueryRowContext(ctx,
		`SELECT pg_catalog.pg_advisory_unlock($1)`, migrationAdvisoryLockID).Scan(&unlocked); err != nil {
		discardConnection(conn)
		return fmt.Errorf("releasing migration advisory lock: %w", err)
	}
	if !unlocked {
		discardConnection(conn)
		return errors.New("releasing migration advisory lock: lock was not held by the migration connection")
	}
	// The migration session carries hardened GUCs and may have temporary probe
	// objects. Never return its physical connection to the application pool.
	discardConnection(conn)
	return nil
}

// discardConnection forces database/sql to close the physical session. It is
// used when lock ownership is uncertain, because returning that session to the
// pool could retain a session-level advisory lock indefinitely.
func discardConnection(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
}

// Ping verifies the database is reachable.
func (s *Store) Ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.db.PingContext(ctx)
}

// Close releases the database connection pool.
func (s *Store) Close() error {
	return s.db.Close()
}

// closeRows releases query resources. Callers return iteration failures via
// Rows.Err, while Close itself has no additional actionable error here.
func closeRows(rows *sql.Rows) {
	_ = rows.Close()
}

// DB returns the underlying *sql.DB for direct access by other packages.
func (s *Store) DB() *sql.DB {
	return s.db
}

// SpooledMessage is an offline chat message awaiting delivery to a user.
type SpooledMessage struct {
	ID           int64
	FromUserID   int64
	FromUniqueID string // sender unique ID (needed to open E2EE DMs)
	FromName     string // sender nickname at send time, empty if unknown
	Message      string
	SentAt       time.Time
}

// SpoolMessage stores an offline message for later delivery. For E2EE direct
// messages, message is base64 ciphertext the server cannot read and
// fromUniqueID lets the recipient fetch the sender's public key.
func (s *Store) SpoolMessage(ctx context.Context, fromUserID, toUserID int64, fromUniqueID, message string) error {
	const q = `INSERT INTO offline_messages (from_user_id, to_user_id, from_unique_id, message)
	          VALUES ($1, $2, $3, $4)`
	if _, err := s.db.ExecContext(ctx, q, fromUserID, toUserID, fromUniqueID, message); err != nil {
		return fmt.Errorf("spooling offline message: %w", err)
	}
	return nil
}

// PendingMessages returns all undelivered offline messages for a user, oldest
// first.
func (s *Store) PendingMessages(ctx context.Context, toUserID int64) ([]SpooledMessage, error) {
	const q = `SELECT om.id, om.from_user_id, om.from_unique_id, COALESCE(u.nickname, ''), om.message, om.sent_at
	          FROM offline_messages om
	          LEFT JOIN users u ON u.id = om.from_user_id
	          WHERE om.to_user_id = $1 AND om.delivered_at IS NULL
	          ORDER BY om.sent_at`
	rows, err := s.db.QueryContext(ctx, q, toUserID)
	if err != nil {
		return nil, fmt.Errorf("querying offline messages: %w", err)
	}
	defer closeRows(rows)

	var out []SpooledMessage
	for rows.Next() {
		var m SpooledMessage
		if err := rows.Scan(&m.ID, &m.FromUserID, &m.FromUniqueID, &m.FromName, &m.Message, &m.SentAt); err != nil {
			return nil, fmt.Errorf("scanning offline message: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetE2EPublicKey stores a user's X25519 public key (base64) for E2EE.
func (s *Store) SetE2EPublicKey(ctx context.Context, userID int64, publicKey string) error {
	const q = `UPDATE users SET e2e_public_key = $1 WHERE id = $2`
	if _, err := s.db.ExecContext(ctx, q, publicKey, userID); err != nil {
		return fmt.Errorf("storing e2e public key: %w", err)
	}
	return nil
}

// GetE2EPublicKey returns a user's X25519 public key ("" when never
// published).
func (s *Store) GetE2EPublicKey(ctx context.Context, uniqueID string) (string, error) {
	const q = `SELECT COALESCE(e2e_public_key, '') FROM users WHERE unique_id = $1`
	var key string
	err := s.db.QueryRowContext(ctx, q, uniqueID).Scan(&key)
	if err != nil {
		return "", fmt.Errorf("loading e2e public key: %w", err)
	}
	return key, nil
}

// MarkMessagesDelivered marks the given offline messages as delivered. It is
// a no-op for an empty id list.
func (s *Store) MarkMessagesDelivered(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	const q = `UPDATE offline_messages SET delivered_at = NOW() WHERE id = ANY($1)`
	if _, err := s.db.ExecContext(ctx, q, pq.Array(ids)); err != nil {
		return fmt.Errorf("marking offline messages delivered: %w", err)
	}
	return nil
}

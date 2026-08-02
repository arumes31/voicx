package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/lib/pq"
	"go.uber.org/zap"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store wraps a *sql.DB connection pool and a logger, providing access to the
// voicx PostgreSQL database.
type Store struct {
	db            *sql.DB
	logger        *zap.Logger
	reactionCache sync.Map // message id -> *reactionCacheEntry
	pii           *PIICipher
}

// SetPIICipher installs the process-local field cipher used by PII methods.
func (s *Store) SetPIICipher(cipher *PIICipher) { s.pii = cipher }

// SchemaVersion returns the newest applied migration filename. An empty
// string means the migration ledger exists but has no applied files yet.
func (s *Store) SchemaVersion(ctx context.Context) (string, error) {
	const q = `SELECT COALESCE(MAX(filename), '') FROM schema_migrations`
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

// Migrate applies every embedded migration exactly once, in lexical order,
// recording each in schema_migrations. Files applied before this ledger
// existed are re-applied once and then recorded; every migration up to 011 is
// idempotent DDL, so that single replay is safe. From 012 onward a migration
// MAY contain destructive DML, because the ledger guarantees it runs exactly
// once (91-135: without it, a DELETE in a migration is a permanent booby trap
// that fires on every boot).
func (s *Store) Migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
	    filename   TEXT PRIMARY KEY,
	    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}

	applied, err := s.appliedMigrations()
	if err != nil {
		return err
	}

	names, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("reading migrations directory: %w", err)
	}

	files := make([]string, 0, len(names))
	for _, f := range names {
		if f.IsDir() {
			continue
		}
		files = append(files, f.Name())
	}
	sort.Strings(files)

	for _, name := range files {
		if applied[name] {
			continue
		}
		content, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("reading migration %s: %w", name, err)
		}
		// The ledger INSERT must commit with the file itself; a crash between
		// the two would re-run a destructive migration.
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("beginning migration %s: %w", name, err)
		}
		if _, err := tx.Exec(string(content)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("applying migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (filename) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("recording migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing migration %s: %w", name, err)
		}
		if s.logger != nil {
			s.logger.Info("migration applied", zap.String("file", name))
		}
	}
	return nil
}

// appliedMigrations reads the ledger.
func (s *Store) appliedMigrations() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT filename FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("reading schema_migrations: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scanning schema_migrations: %w", err)
		}
		out[name] = true
	}
	return out, rows.Err()
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
	defer rows.Close()

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

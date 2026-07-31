package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"time"

	"github.com/lib/pq"
	"go.uber.org/zap"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store wraps a *sql.DB connection pool and a logger, providing access to the
// voicx PostgreSQL database.
type Store struct {
	db     *sql.DB
	logger *zap.Logger
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

// Migrate reads all embedded .sql migration files in lexical order and executes
// them against the database. Migrations are idempotent (use CREATE TABLE IF NOT
// EXISTS) so re-running is safe. A proper migration tool should replace this in
// a later phase.
func (s *Store) Migrate() error {
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
		content, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("reading migration %s: %w", name, err)
		}
		if _, err := s.db.Exec(string(content)); err != nil {
			return fmt.Errorf("applying migration %s: %w", name, err)
		}
		if s.logger != nil {
			s.logger.Info("migration applied", zap.String("file", name))
		}
	}
	return nil
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
	ID         int64
	FromUserID int64
	FromName   string // sender nickname at send time, empty if unknown
	Message    string
	SentAt     time.Time
}

// SpoolMessage stores an offline message for later delivery.
func (s *Store) SpoolMessage(ctx context.Context, fromUserID, toUserID int64, message string) error {
	const q = `INSERT INTO offline_messages (from_user_id, to_user_id, message)
	          VALUES ($1, $2, $3)`
	if _, err := s.db.ExecContext(ctx, q, fromUserID, toUserID, message); err != nil {
		return fmt.Errorf("spooling offline message: %w", err)
	}
	return nil
}

// PendingMessages returns all undelivered offline messages for a user, oldest
// first.
func (s *Store) PendingMessages(ctx context.Context, toUserID int64) ([]SpooledMessage, error) {
	const q = `SELECT om.id, om.from_user_id, COALESCE(u.nickname, ''), om.message, om.sent_at
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
		if err := rows.Scan(&m.ID, &m.FromUserID, &m.FromName, &m.Message, &m.SentAt); err != nil {
			return nil, fmt.Errorf("scanning offline message: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
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

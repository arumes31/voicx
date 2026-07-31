// tokens.go implements privilege-token queries against the tokens table
// (migration 001_init.sql). Token semantics: token_type 0 = server group
// token (tokenuse grants membership in group_id); a group_id of 0/NULL means
// the token grants server admin (used by the first-run bootstrap token).
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ErrTokenNotFound is returned when a token key does not exist.
var ErrTokenNotFound = errors.New("token not found")

// ErrTokenExhausted is returned when a token has no uses left.
var ErrTokenExhausted = errors.New("token exhausted")

// Token describes one privilege token.
type Token struct {
	ID        int64
	Key       string
	Type      int // 0=server group, 1=channel group (unused for now)
	GroupID   int64
	Uses      int
	MaxUses   int
	CreatedAt time.Time
}

// CreateToken generates a random token key and inserts a token row.
// groupID 0 means the token grants server admin on use.
func (s *Store) CreateToken(ctx context.Context, tokenType int, groupID int64, maxUses int) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating token: %w", err)
	}
	key := hex.EncodeToString(b)
	if maxUses <= 0 {
		maxUses = 1
	}

	var gid any
	if groupID != 0 {
		gid = groupID
	}
	const q = `INSERT INTO tokens (token_key, token_type, group_id, max_uses)
	          VALUES ($1, $2, $3, $4)`
	if _, err := s.db.ExecContext(ctx, q, key, tokenType, gid, maxUses); err != nil {
		return "", fmt.Errorf("inserting token: %w", err)
	}
	return key, nil
}

// ListTokens returns all tokens, oldest first.
func (s *Store) ListTokens(ctx context.Context) ([]Token, error) {
	const q = `SELECT id, token_key, token_type, COALESCE(group_id, 0), uses, max_uses, created_at
	          FROM tokens ORDER BY id`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("querying tokens: %w", err)
	}
	defer rows.Close()

	var out []Token
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.ID, &t.Key, &t.Type, &t.GroupID, &t.Uses, &t.MaxUses, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning token: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteToken removes a token by key. It returns ErrTokenNotFound when no
// such token exists.
func (s *Store) DeleteToken(ctx context.Context, key string) error {
	const q = `DELETE FROM tokens WHERE token_key = $1`
	res, err := s.db.ExecContext(ctx, q, key)
	if err != nil {
		return fmt.Errorf("deleting token: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrTokenNotFound
	}
	return nil
}

// UseToken redeems a token for a user: it consumes one use and applies the
// grant (server-group membership, or server admin when group_id is 0). It
// returns the granted group ID (0 = server admin grant).
func (s *Store) UseToken(ctx context.Context, key string, userID int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("beginning token tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		tokenID int64
		groupID sql.NullInt64
		uses    int
		maxUses int
	)
	const sel = `SELECT id, group_id, uses, max_uses FROM tokens WHERE token_key = $1 FOR UPDATE`
	err = tx.QueryRowContext(ctx, sel, key).Scan(&tokenID, &groupID, &uses, &maxUses)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrTokenNotFound
		}
		return 0, fmt.Errorf("querying token: %w", err)
	}
	if uses >= maxUses {
		return 0, ErrTokenExhausted
	}

	if _, err := tx.ExecContext(ctx, `UPDATE tokens SET uses = uses + 1 WHERE id = $1`, tokenID); err != nil {
		return 0, fmt.Errorf("consuming token: %w", err)
	}

	granted := int64(0)
	if groupID.Valid && groupID.Int64 != 0 {
		const ins = `INSERT INTO server_group_members (user_id, server_group_id)
		            VALUES ($1, $2) ON CONFLICT DO NOTHING`
		if _, err := tx.ExecContext(ctx, ins, userID, groupID.Int64); err != nil {
			return 0, fmt.Errorf("assigning server group: %w", err)
		}
		granted = groupID.Int64
	} else {
		// Group-less token: grant server admin.
		if _, err := tx.ExecContext(ctx, `UPDATE users SET is_admin = TRUE WHERE id = $1`, userID); err != nil {
			return 0, fmt.Errorf("granting admin: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing token use: %w", err)
	}
	return granted, nil
}

// HasAdminUser reports whether any user has the admin flag.
func (s *Store) HasAdminUser(ctx context.Context) (bool, error) {
	var exists bool
	const q = `SELECT EXISTS(SELECT 1 FROM users WHERE is_admin)`
	if err := s.db.QueryRowContext(ctx, q).Scan(&exists); err != nil {
		return false, fmt.Errorf("querying admin user: %w", err)
	}
	return exists, nil
}

// CountTokens returns the number of tokens in the table.
func (s *Store) CountTokens(ctx context.Context) (int, error) {
	var n int
	const q = `SELECT COUNT(*) FROM tokens`
	if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting tokens: %w", err)
	}
	return n, nil
}

// BootstrapAdminToken returns a fresh one-time admin token when the server
// has no admin user and no tokens (first-run bootstrap, TS3-style initial
// privilege key). It returns "" when no bootstrap is needed.
func (s *Store) BootstrapAdminToken(ctx context.Context) (string, error) {
	admin, err := s.HasAdminUser(ctx)
	if err != nil {
		return "", err
	}
	if admin {
		return "", nil
	}
	n, err := s.CountTokens(ctx)
	if err != nil {
		return "", err
	}
	if n > 0 {
		return "", nil
	}
	return s.CreateToken(ctx, 0, 0, 1)
}

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
	ChannelID int64
	Uses      int
	MaxUses   int
	CreatedAt time.Time
	// Description is the operator's note shown in the token manager, UsedBy
	// the unique ID of the last redeemer ("" while unredeemed) (174).
	Description string
	UsedBy      string
}

// TokenGrant is the result of a successful privilege-token redemption.
// Promoted is true when the redeemer had no users row and one was created in
// the same transaction as the grant.
type TokenGrant struct {
	UserID   int64
	GroupID  int64
	Admin    bool
	Promoted bool
}

// CreateToken generates a random token key and inserts a token row.
// groupID 0 means the token grants server admin on use.
func (s *Store) CreateToken(ctx context.Context, tokenType int, groupID int64, maxUses int) (string, error) {
	return s.CreateTokenWithMeta(ctx, tokenType, groupID, 0, "", maxUses)
}

// CreateTokenWithMeta is CreateToken with the channel scope and the
// description the token manager displays (174). The key is always generated
// here so a caller can never choose a guessable one.
func (s *Store) CreateTokenWithMeta(ctx context.Context, tokenType int, groupID, channelID int64, description string, maxUses int) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating token: %w", err)
	}
	key := hex.EncodeToString(b)
	if maxUses <= 0 {
		maxUses = 1
	}

	// group_id and channel_id are nullable FKs: 0 means "not scoped", which
	// must go in as NULL rather than a reference to a nonexistent row.
	var gid, cid any
	if groupID != 0 {
		gid = groupID
	}
	if channelID != 0 {
		cid = channelID
	}
	const q = `INSERT INTO tokens (token_key, token_type, group_id, channel_id, description, max_uses)
	          VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := s.db.ExecContext(ctx, q, key, tokenType, gid, cid, description, maxUses); err != nil {
		return "", fmt.Errorf("inserting token: %w", err)
	}
	return key, nil
}

// ListTokens returns all tokens, oldest first.
func (s *Store) ListTokens(ctx context.Context) ([]Token, error) {
	const q = `SELECT id, token_key, token_type, COALESCE(group_id, 0), COALESCE(channel_id, 0),
	                 uses, max_uses, created_at, description, used_by
	          FROM tokens ORDER BY id`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("querying tokens: %w", err)
	}
	defer closeRows(rows)

	var out []Token
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.ID, &t.Key, &t.Type, &t.GroupID, &t.ChannelID,
			&t.Uses, &t.MaxUses, &t.CreatedAt, &t.Description, &t.UsedBy); err != nil {
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
	grant, err := s.UseTokenForIdentity(ctx, key, userID, "", "")
	return grant.GroupID, err
}

// UseTokenForIdentity redeems a token for either an existing user or a guest.
// A guest is promoted to a passwordless identity account inside the same
// transaction, so an invalid/exhausted token cannot leave an orphan account.
func (s *Store) UseTokenForIdentity(ctx context.Context, key string, userID int64, uniqueID, nickname string) (TokenGrant, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TokenGrant{}, fmt.Errorf("beginning token tx: %w", err)
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
			return TokenGrant{}, ErrTokenNotFound
		}
		return TokenGrant{}, fmt.Errorf("querying token: %w", err)
	}
	if uses >= maxUses {
		return TokenGrant{}, ErrTokenExhausted
	}

	grant := TokenGrant{UserID: userID}
	if grant.UserID == 0 {
		if uniqueID == "" {
			return TokenGrant{}, errors.New("guest identity is required")
		}
		const ensureUser = `INSERT INTO users (unique_id, nickname, created_at)
		                    VALUES ($1, $2, NOW())
		                    ON CONFLICT (unique_id) DO UPDATE
		                    SET nickname = CASE WHEN users.nickname = '' THEN EXCLUDED.nickname ELSE users.nickname END
		                    RETURNING id, (xmax = 0) AS inserted, COALESCE(password_hash, '') AS password_hash`
		var (
			inserted     bool
			passwordHash string
		)
		if err := tx.QueryRowContext(ctx, ensureUser, uniqueID, nickname).Scan(&grant.UserID, &inserted, &passwordHash); err != nil {
			return TokenGrant{}, fmt.Errorf("promoting guest identity: %w", err)
		}
		if !inserted && passwordHash != "" {
			return TokenGrant{}, fmt.Errorf("user %s already exists with credentials", uniqueID)
		}
		if inserted {
			grant.Promoted = true
		}
	}

	// used_by records the redeemer for the token manager (174). It is resolved
	// from the users row here rather than passed in, so the caller cannot
	// claim a redemption under someone else's unique ID.
	const upd = `UPDATE tokens SET uses = uses + 1,
	                              used_by = COALESCE((SELECT unique_id FROM users WHERE id = $2), '')
	            WHERE id = $1`
	if _, err := tx.ExecContext(ctx, upd, tokenID, grant.UserID); err != nil {
		return TokenGrant{}, fmt.Errorf("consuming token: %w", err)
	}

	if groupID.Valid && groupID.Int64 != 0 {
		const ins = `INSERT INTO server_group_members (user_id, server_group_id)
		            VALUES ($1, $2) ON CONFLICT DO NOTHING`
		if _, err := tx.ExecContext(ctx, ins, grant.UserID, groupID.Int64); err != nil {
			return TokenGrant{}, fmt.Errorf("assigning server group: %w", err)
		}
		grant.GroupID = groupID.Int64
	} else {
		// Group-less token: grant server admin.
		if _, err := tx.ExecContext(ctx, `UPDATE users SET is_admin = TRUE WHERE id = $1`, grant.UserID); err != nil {
			return TokenGrant{}, fmt.Errorf("granting admin: %w", err)
		}
		grant.Admin = true
	}

	if err := tx.Commit(); err != nil {
		return TokenGrant{}, fmt.Errorf("committing token use: %w", err)
	}
	return grant, nil
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

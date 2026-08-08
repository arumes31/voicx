// chatkeys.go persists the per-scope chat key generations introduced by
// migration 012 (91-135). Rows hold the scope key WRAPPED under a KEK that
// lives outside the database (internal/chatcrypto), so a database dump alone
// cannot open any stored message.
//
// Two invariants are enforced here rather than in the caller: generation ids
// come from chat_scope_seq and are never derived from RAM or MAX(key_id)+1,
// and a primary-key conflict is a hard error — it means two different key
// materials are competing for one generation id, which is corruption and
// must be loud.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"voicx/internal/safecast"
)

// ScopeKey is one persisted generation of a scope's chat key. Wrapped is
// nonce[24] || secretbox(key32, KEK). RetiredAt nil means "current".
type ScopeKey struct {
	KeyID     uint32
	Wrapped   []byte
	KEKID     uint16
	CreatedAt time.Time
	RetiredAt *time.Time
}

// CountScopeKeys reports how many generations exist across all scopes. The
// server uses it to decide whether a missing KEK ring may be created or must
// be a fatal startup error.
func (s *Store) CountScopeKeys(ctx context.Context) (int64, error) {
	const q = `SELECT count(*) FROM chat_scope_keys`
	var n int64
	if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting scope keys: %w", err)
	}
	return n, nil
}

// AllocScopeKeyID allocates the next generation id for a scope. Ids are
// allocated here so an id is physically unreusable even if its row is later
// deleted; a re-minted id with different key material would orphan every
// message that referenced the original.
func (s *Store) AllocScopeKeyID(ctx context.Context, scope int64) (uint32, error) {
	const q = `INSERT INTO chat_scope_seq (scope_id, next_key_id) VALUES ($1, 2)
	          ON CONFLICT (scope_id) DO UPDATE SET next_key_id = chat_scope_seq.next_key_id + 1
	          RETURNING next_key_id - 1`
	var id int64
	if err := s.db.QueryRowContext(ctx, q, scope).Scan(&id); err != nil {
		return 0, fmt.Errorf("allocating scope key id for %d: %w", scope, err)
	}
	keyID, err := safecast.Int64ToUint32(id)
	if err != nil {
		return 0, fmt.Errorf("allocating scope key id for %d returned invalid id %d: %w", scope, id, err)
	}
	return keyID, nil
}

// CurrentScopeKey returns the scope's live generation, or (nil, nil) when the
// scope has none.
func (s *Store) CurrentScopeKey(ctx context.Context, scope int64) (*ScopeKey, error) {
	const q = `SELECT key_id, wrapped_key, kek_id, created_at, retired_at
	          FROM chat_scope_keys WHERE scope_id = $1 AND retired_at IS NULL`
	return s.scanScopeKey(s.db.QueryRowContext(ctx, q, scope), scope)
}

// GetScopeKey returns one archival generation, or (nil, nil) when unknown.
func (s *Store) GetScopeKey(ctx context.Context, scope int64, keyID uint32) (*ScopeKey, error) {
	const q = `SELECT key_id, wrapped_key, kek_id, created_at, retired_at
	          FROM chat_scope_keys WHERE scope_id = $1 AND key_id = $2`
	return s.scanScopeKey(s.db.QueryRowContext(ctx, q, scope, int64(keyID)), scope)
}

func (s *Store) scanScopeKey(row *sql.Row, scope int64) (*ScopeKey, error) {
	var (
		k     ScopeKey
		keyID int64
		kekID int64
	)
	err := row.Scan(&keyID, &k.Wrapped, &kekID, &k.CreatedAt, &k.RetiredAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("loading scope key for %d: %w", scope, err)
	}
	k.KeyID, err = safecast.Int64ToUint32(keyID)
	if err != nil {
		return nil, fmt.Errorf("loading scope key for %d has invalid key id %d: %w", scope, keyID, err)
	}
	k.KEKID, err = safecast.Int64ToUint16(kekID)
	if err != nil {
		return nil, fmt.Errorf("loading scope key for %d has invalid kek id %d: %w", scope, kekID, err)
	}
	return &k, nil
}

// InsertScopeKey stores a generation as the scope's current key. A primary-key
// conflict is returned as an error, never swallowed: it signals two different
// key materials competing for one generation id.
func (s *Store) InsertScopeKey(ctx context.Context, scope int64, keyID uint32, wrapped []byte, kekID uint16) error {
	const q = `INSERT INTO chat_scope_keys (scope_id, key_id, wrapped_key, kek_id) VALUES ($1, $2, $3, $4)`
	if _, err := s.db.ExecContext(ctx, q, scope, int64(keyID), wrapped, int32(kekID)); err != nil {
		return fmt.Errorf("inserting scope key %d/%d: %w", scope, keyID, err)
	}
	return nil
}

// RotateScopeKey retires the scope's current generation and installs a new one
// in ONE transaction, so the partial unique index can never see two live
// generations. Generations are retired, never deleted: a row referencing a
// deleted generation would decrypt to nothing, undetectably and forever.
func (s *Store) RotateScopeKey(ctx context.Context, scope int64, newKeyID uint32, wrapped []byte, kekID uint16) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning scope key rotation for %d: %w", scope, err)
	}
	const retire = `UPDATE chat_scope_keys SET retired_at = NOW() WHERE scope_id = $1 AND retired_at IS NULL`
	if _, err := tx.ExecContext(ctx, retire, scope); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("retiring scope key for %d: %w", scope, err)
	}
	const ins = `INSERT INTO chat_scope_keys (scope_id, key_id, wrapped_key, kek_id) VALUES ($1, $2, $3, $4)`
	if _, err := tx.ExecContext(ctx, ins, scope, int64(newKeyID), wrapped, int32(kekID)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("inserting scope key %d/%d: %w", scope, newKeyID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing scope key rotation for %d: %w", scope, err)
	}
	return nil
}

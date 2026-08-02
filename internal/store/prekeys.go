package store

import (
	"context"
	"database/sql"
	"fmt"
)

type PreKey struct {
	KeyID     uint32
	PublicKey []byte
	Signature []byte
	OneTime   bool
}

type PreKeyBundle struct {
	IdentityDH     []byte
	SigningPublic  []byte
	SignedPreKeyID uint32
	SignedPreKey   []byte
	Signature      []byte
	OneTimePreKey  *PreKey
}

// PublishPreKeyBundle atomically replaces the device's signed bundle and
// replenishes its one-time prekeys.
func (s *Store) PublishPreKeyBundle(ctx context.Context, userID int64, bundle PreKeyBundle, oneTime []PreKey) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	const upsert = `INSERT INTO e2ee_prekey_bundles
	              (user_id, identity_dh, signing_public, signed_prekey_id, signed_prekey, signature)
	              VALUES ($1, $2, $3, $4, $5, $6)
	              ON CONFLICT (user_id) DO UPDATE SET identity_dh=EXCLUDED.identity_dh,
	              signing_public=EXCLUDED.signing_public, signed_prekey_id=EXCLUDED.signed_prekey_id,
	              signed_prekey=EXCLUDED.signed_prekey, signature=EXCLUDED.signature, updated_at=NOW()`
	if _, err := tx.ExecContext(ctx, upsert, userID, bundle.IdentityDH, bundle.SigningPublic,
		int64(bundle.SignedPreKeyID), bundle.SignedPreKey, bundle.Signature); err != nil {
		return fmt.Errorf("publishing prekey bundle: %w", err)
	}
	const keyUpsert = `INSERT INTO e2ee_prekeys (user_id, key_id, public_key, signature, one_time)
	                  VALUES ($1, $2, $3, $4, TRUE)
	                  ON CONFLICT (user_id, key_id) DO UPDATE SET public_key=EXCLUDED.public_key,
	                  signature=EXCLUDED.signature, one_time=TRUE, consumed_at=NULL`
	for _, key := range oneTime {
		if len(key.PublicKey) != 32 {
			return fmt.Errorf("one-time prekey %d has invalid size", key.KeyID)
		}
		if _, err := tx.ExecContext(ctx, keyUpsert, userID, int64(key.KeyID), key.PublicKey, key.Signature); err != nil {
			return fmt.Errorf("publishing one-time prekey: %w", err)
		}
	}
	return tx.Commit()
}

// ConsumePreKeyBundle returns the current signed bundle and consumes at most
// one one-time prekey exactly once across concurrent replicas.
func (s *Store) ConsumePreKeyBundle(ctx context.Context, userID int64) (*PreKeyBundle, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	const bundleQuery = `SELECT identity_dh, signing_public, signed_prekey_id, signed_prekey, signature
	                     FROM e2ee_prekey_bundles WHERE user_id=$1`
	var bundle PreKeyBundle
	var signedID int64
	if err := tx.QueryRowContext(ctx, bundleQuery, userID).Scan(&bundle.IdentityDH, &bundle.SigningPublic,
		&signedID, &bundle.SignedPreKey, &bundle.Signature); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("loading prekey bundle: %w", err)
	}
	bundle.SignedPreKeyID = uint32(signedID)
	const oneTimeQuery = `SELECT key_id, public_key, COALESCE(signature, ''::bytea)
	                      FROM e2ee_prekeys WHERE user_id=$1 AND one_time AND consumed_at IS NULL
	                      ORDER BY key_id LIMIT 1 FOR UPDATE SKIP LOCKED`
	var key PreKey
	var keyID int64
	err = tx.QueryRowContext(ctx, oneTimeQuery, userID).Scan(&keyID, &key.PublicKey, &key.Signature)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("loading one-time prekey: %w", err)
	}
	if err == nil {
		key.KeyID, key.OneTime = uint32(keyID), true
		if _, err := tx.ExecContext(ctx, `UPDATE e2ee_prekeys SET consumed_at=NOW() WHERE user_id=$1 AND key_id=$2`, userID, keyID); err != nil {
			return nil, fmt.Errorf("consuming one-time prekey: %w", err)
		}
		bundle.OneTimePreKey = &key
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &bundle, nil
}

func (s *Store) PublishPreKeys(ctx context.Context, userID int64, keys []PreKey) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	const q = `INSERT INTO e2ee_prekeys (user_id, key_id, public_key, signature, one_time)
	          VALUES ($1, $2, $3, $4, $5)
	          ON CONFLICT (user_id, key_id) DO UPDATE SET public_key = EXCLUDED.public_key,
	          signature = EXCLUDED.signature, one_time = EXCLUDED.one_time, consumed_at = NULL`
	for _, key := range keys {
		if len(key.PublicKey) != 32 {
			return fmt.Errorf("prekey %d has %d bytes, want 32", key.KeyID, len(key.PublicKey))
		}
		if _, err := tx.ExecContext(ctx, q, userID, key.KeyID, key.PublicKey, key.Signature, key.OneTime); err != nil {
			return fmt.Errorf("publishing prekey: %w", err)
		}
	}
	return tx.Commit()
}

// ConsumePreKey locks and consumes one one-time prekey exactly once.
func (s *Store) ConsumePreKey(ctx context.Context, userID int64) (*PreKey, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	const q = `SELECT key_id, public_key, COALESCE(signature, ''::bytea)
	          FROM e2ee_prekeys WHERE user_id = $1 AND one_time AND consumed_at IS NULL
	          ORDER BY key_id LIMIT 1 FOR UPDATE SKIP LOCKED`
	var key PreKey
	if err := tx.QueryRowContext(ctx, q, userID).Scan(&key.KeyID, &key.PublicKey, &key.Signature); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	key.OneTime = true
	if _, err := tx.ExecContext(ctx, `UPDATE e2ee_prekeys SET consumed_at = NOW() WHERE user_id = $1 AND key_id = $2`, userID, key.KeyID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &key, nil
}

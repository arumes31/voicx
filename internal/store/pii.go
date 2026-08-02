package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

var ErrNoPII = errors.New("no PII record")

// PIICipher encrypts individual columns with AES-256-GCM. The user id and
// column name are authenticated as associated data, preventing ciphertext
// from being copied to another row or field.
type PIICipher struct{ aead cipher.AEAD }

func NewPIICipher(key []byte) (*PIICipher, error) {
	if len(key) != 32 {
		return nil, errors.New("PII key must be exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &PIICipher{aead: aead}, nil
}

// LoadOrCreatePIICipher keeps the master key outside PostgreSQL. Creation is
// atomic and refuses permissive replacement of an existing key.
func LoadOrCreatePIICipher(path string) (*PIICipher, error) {
	rf, err := os.Open(path)
	if err == nil {
		defer rf.Close()
		info, statErr := rf.Stat()
		if statErr != nil {
			return nil, fmt.Errorf("checking PII key permissions: %w", statErr)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("PII key permissions %o are too broad; want 0600", info.Mode().Perm())
		}
		key, readErr := io.ReadAll(rf)
		if readErr != nil {
			return nil, fmt.Errorf("reading PII key: %w", readErr)
		}
		return NewPIICipher(key)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("reading PII key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating PII key directory: %w", err)
	}
	newKey := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, newKey); err != nil {
		return nil, fmt.Errorf("generating PII key: %w", err)
	}
	wf, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return LoadOrCreatePIICipher(path)
		}
		return nil, fmt.Errorf("creating PII key: %w", err)
	}
	if _, err := wf.Write(newKey); err != nil {
		_ = wf.Close()
		return nil, fmt.Errorf("writing PII key: %w", err)
	}
	if err := wf.Sync(); err != nil {
		_ = wf.Close()
		return nil, fmt.Errorf("syncing PII key: %w", err)
	}
	if err := wf.Close(); err != nil {
		return nil, fmt.Errorf("closing PII key: %w", err)
	}
	if runtime.GOOS != "windows" {
		dir, err := os.Open(filepath.Dir(path))
		if err != nil {
			return nil, fmt.Errorf("opening PII key directory for sync: %w", err)
		}
		if err := dir.Sync(); err != nil {
			_ = dir.Close()
			return nil, fmt.Errorf("syncing PII key directory: %w", err)
		}
		if err := dir.Close(); err != nil {
			return nil, fmt.Errorf("closing PII key directory: %w", err)
		}
	}
	return NewPIICipher(newKey)
}

func (c *PIICipher) seal(plain string, aad []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return append(nonce, c.aead.Seal(nil, nonce, []byte(plain), aad)...), nil
}

func (c *PIICipher) open(blob, aad []byte) (string, error) {
	n := c.aead.NonceSize()
	if len(blob) < n+c.aead.Overhead() {
		return "", errors.New("invalid encrypted PII")
	}
	plain, err := c.aead.Open(nil, blob[:n], blob[n:], aad)
	if err != nil {
		return "", errors.New("PII authentication failed")
	}
	return string(plain), nil
}

func piiAAD(userID int64, field string) []byte {
	return []byte(fmt.Sprintf("voicx:pii:%d:%s", userID, field))
}

func (s *Store) SetUserPII(ctx context.Context, userID int64, email, lastIP *string) error {
	if s.pii == nil {
		return errors.New("PII cipher is not configured")
	}
	var emailEnc, ipEnc []byte
	var err error
	if email != nil && *email != "" {
		emailEnc, err = s.pii.seal(*email, piiAAD(userID, "email"))
		if err != nil {
			return fmt.Errorf("encrypting email: %w", err)
		}
	}
	if lastIP != nil && *lastIP != "" {
		ipEnc, err = s.pii.seal(*lastIP, piiAAD(userID, "last_ip"))
		if err != nil {
			return fmt.Errorf("encrypting IP: %w", err)
		}
	}
	const q = `INSERT INTO user_private_data (user_id, email_enc, last_ip_enc)
	          VALUES ($1, CASE WHEN $2 THEN $3::bytea ELSE NULL END,
	                  CASE WHEN $4 THEN $5::bytea ELSE NULL END)
	          ON CONFLICT (user_id) DO UPDATE SET
	          email_enc = CASE WHEN $2 THEN EXCLUDED.email_enc ELSE user_private_data.email_enc END,
	          last_ip_enc = CASE WHEN $4 THEN EXCLUDED.last_ip_enc ELSE user_private_data.last_ip_enc END,
	          updated_at = NOW()`
	if _, err := s.db.ExecContext(ctx, q, userID, email != nil, emailEnc, lastIP != nil, ipEnc); err != nil {
		return fmt.Errorf("storing encrypted PII: %w", err)
	}
	return nil
}

func (s *Store) UserPII(ctx context.Context, userID int64) (email, lastIP string, err error) {
	if s.pii == nil {
		return "", "", errors.New("PII cipher is not configured")
	}
	var emailEnc, ipEnc []byte
	const q = `SELECT COALESCE(email_enc, ''::bytea), COALESCE(last_ip_enc, ''::bytea)
	          FROM user_private_data WHERE user_id = $1`
	if err := s.db.QueryRowContext(ctx, q, userID).Scan(&emailEnc, &ipEnc); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", ErrNoPII
		}
		return "", "", err
	}
	if len(emailEnc) > 0 {
		email, err = s.pii.open(emailEnc, piiAAD(userID, "email"))
		if err != nil {
			return "", "", err
		}
	}
	if len(ipEnc) > 0 {
		lastIP, err = s.pii.open(ipEnc, piiAAD(userID, "last_ip"))
		if err != nil {
			return "", "", err
		}
	}
	return email, lastIP, nil
}

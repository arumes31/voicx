// Package rules stores the operator-defined server rules and records who has
// accepted them (215), so the client dialog is shown once per user and again
// whenever the wording changes.
//
// Storage: the rules TEXT lives in server_settings under "server_rules". It is
// operator content published to everyone who joins, not chat, so it is stored
// in the clear (unlike the MOTD, which is sealed under the global chat scope
// key). Readers are therefore: every joining client, any server admin over
// ServerQuery, and anyone with database access.
//
// Acceptance is keyed by the sha256 of the rules text, so an edit re-asks
// everyone rather than inheriting consent to different wording.
package rules

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
)

// SettingKey is the server_settings row holding the rules text.
const SettingKey = "server_rules"

// Settings is the subset of the store the service needs. It is satisfied by
// *store.Store.
type Settings interface {
	GetServerSetting(ctx context.Context, key string) (string, uint32, error)
	SetServerSetting(ctx context.Context, key, value string, keyID uint32) error
}

// Service reads and writes the rules and their per-user acceptance.
type Service struct {
	settings Settings
	db       *sql.DB
}

// New constructs a Service. db is the same handle the store uses.
func New(settings Settings, db *sql.DB) *Service {
	return &Service{settings: settings, db: db}
}

// Hash is the acceptance key for a rules text. Empty text has no hash: rules
// that are not configured are never asked about.
func Hash(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Text returns the configured rules and their hash; both are empty when no
// rules are configured.
func (s *Service) Text(ctx context.Context) (string, string, error) {
	value, _, err := s.settings.GetServerSetting(ctx, SettingKey)
	if err != nil {
		return "", "", err
	}
	return value, Hash(value), nil
}

// Set stores the rules text. Storing an empty text clears the rules, which
// stops the dialog from being asked at all.
func (s *Service) Set(ctx context.Context, text string) error {
	return s.settings.SetServerSetting(ctx, SettingKey, text, 0)
}

// Pending returns the rules a user still has to accept. pending is false when
// no rules are configured or the user already accepted this exact wording,
// which is what makes the dialog a once-per-user prompt.
func (s *Service) Pending(ctx context.Context, userID int64) (text, hash string, pending bool, err error) {
	text, hash, err = s.Text(ctx)
	if err != nil || hash == "" {
		return "", "", false, err
	}
	var accepted string
	err = s.db.QueryRowContext(ctx,
		`SELECT rules_hash FROM server_rules_acceptance WHERE user_id = $1`, userID).Scan(&accepted)
	switch {
	case err == sql.ErrNoRows:
		return text, hash, true, nil
	case err != nil:
		return "", "", false, fmt.Errorf("reading rules acceptance: %w", err)
	}
	return text, hash, accepted != hash, nil
}

// Accept records that a user accepted a specific wording. A stale hash (the
// rules changed while the dialog was open) is refused, so the user is asked
// again for the wording actually in force.
func (s *Service) Accept(ctx context.Context, userID int64, hash string) error {
	_, current, err := s.Text(ctx)
	if err != nil {
		return err
	}
	if current == "" {
		return fmt.Errorf("no server rules are configured")
	}
	if hash != current {
		return fmt.Errorf("rules changed since they were shown")
	}
	const q = `INSERT INTO server_rules_acceptance (user_id, rules_hash, accepted_at)
	           VALUES ($1, $2, NOW())
	           ON CONFLICT (user_id) DO UPDATE SET rules_hash = EXCLUDED.rules_hash, accepted_at = NOW()`
	if _, err := s.db.ExecContext(ctx, q, userID, current); err != nil {
		return fmt.Errorf("recording rules acceptance: %w", err)
	}
	return nil
}

// AcceptedCount reports how many users accepted the wording in force.
func (s *Service) AcceptedCount(ctx context.Context) (int, error) {
	_, hash, err := s.Text(ctx)
	if err != nil || hash == "" {
		return 0, err
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM server_rules_acceptance WHERE rules_hash = $1`, hash).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting rules acceptance: %w", err)
	}
	return n, nil
}

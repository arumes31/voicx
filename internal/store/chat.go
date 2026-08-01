// chat.go implements the store layer for server-side chat infrastructure
// (wave 5a): channel/global message history, pins, reactions, and server
// settings (MOTD/announcement). Bodies are stored as CIPHERTEXT under a
// persisted scope key generation (91-135, migration 012) — the plaintext
// `body` column is pinned empty by a CHECK constraint and disappears in 013.
// DMs are true E2EE and are never stored here.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ChatMessage is one stored channel/global chat message. BodyEnc is
// base64(nonce[24] || secretbox(plain, scopeKey[KeyID])); it is empty (with
// KeyID 0) for tombstoned (deleted) messages. There is deliberately no
// plaintext field: every stale reader fails to compile.
type ChatMessage struct {
	ID           int64
	ChannelID    int64 // 0 = global
	FromUniqueID string
	FromNickname string
	BodyEnc      string
	KeyID        uint32
	SentAt       time.Time
	EditedAt     *time.Time
	DeletedAt    *time.Time
}

// LegacyChatRow is one pre-012 plaintext row, read by the one-shot backfill.
type LegacyChatRow struct {
	ID        int64
	ChannelID int64
	Body      string
}

// StoreChatMessage inserts a channel/global message and returns its ID.
// channelID 0 is the global scope. bodyEnc is the sender's ciphertext stored
// verbatim; keyID names the generation it was sealed under.
func (s *Store) StoreChatMessage(ctx context.Context, channelID int64, fromUniqueID, fromNickname, bodyEnc string, keyID uint32) (int64, error) {
	scope := int16(1)
	if channelID == 0 {
		scope = 0
	}
	const q = `INSERT INTO chat_messages (scope, channel_id, from_unique_id, from_nickname, body_enc, key_id)
	          VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, sent_at`
	var (
		id     int64
		sentAt time.Time
	)
	if err := s.db.QueryRowContext(ctx, q, scope, channelID, fromUniqueID, fromNickname, bodyEnc, keyID).Scan(&id, &sentAt); err != nil {
		return 0, fmt.Errorf("storing chat message: %w", err)
	}
	return id, nil
}

// ChatHistory returns up to limit messages of a scope, newest first, with id
// strictly below beforeID (beforeID 0 = latest page). Tombstoned messages
// are included with an empty BodyEnc and DeletedAt set.
func (s *Store) ChatHistory(ctx context.Context, channelID, beforeID int64, limit int) ([]ChatMessage, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT id, from_unique_id, from_nickname, body_enc, key_id, sent_at, edited_at, deleted_at
	      FROM chat_messages
	      WHERE channel_id = $1`
	args := []any{channelID}
	if beforeID > 0 {
		q += ` AND id < $2`
		args = append(args, beforeID)
	}
	q += fmt.Sprintf(` ORDER BY id DESC LIMIT %d`, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("querying chat history: %w", err)
	}
	defer rows.Close()

	var out []ChatMessage
	for rows.Next() {
		var m ChatMessage
		var keyID int64
		m.ChannelID = channelID
		if err := rows.Scan(&m.ID, &m.FromUniqueID, &m.FromNickname, &m.BodyEnc, &keyID, &m.SentAt, &m.EditedAt, &m.DeletedAt); err != nil {
			return nil, fmt.Errorf("scanning chat message: %w", err)
		}
		m.KeyID = uint32(keyID)
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetChatMessage returns one message (for edit/delete/react authorization
// and rendering).
func (s *Store) GetChatMessage(ctx context.Context, id int64) (*ChatMessage, error) {
	const q = `SELECT channel_id, from_unique_id, from_nickname, body_enc, key_id, sent_at, edited_at, deleted_at
	          FROM chat_messages WHERE id = $1`
	var m ChatMessage
	var keyID int64
	m.ID = id
	err := s.db.QueryRowContext(ctx, q, id).Scan(
		&m.ChannelID, &m.FromUniqueID, &m.FromNickname, &m.BodyEnc, &keyID, &m.SentAt, &m.EditedAt, &m.DeletedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("querying chat message: %w", err)
	}
	m.KeyID = uint32(keyID)
	return &m, nil
}

// EditChatMessage replaces the ciphertext and stamps edited_at.
func (s *Store) EditChatMessage(ctx context.Context, id int64, bodyEnc string, keyID uint32) error {
	const q = `UPDATE chat_messages SET body_enc = $1, key_id = $2, edited_at = NOW()
	          WHERE id = $3 AND deleted_at IS NULL`
	res, err := s.db.ExecContext(ctx, q, bodyEnc, keyID, id)
	if err != nil {
		return fmt.Errorf("editing chat message: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("chat message not found or deleted")
	}
	return nil
}

// DeleteChatMessage tombstones a message: ciphertext and generation cleared,
// deleted_at stamped.
func (s *Store) DeleteChatMessage(ctx context.Context, id int64) error {
	const q = `UPDATE chat_messages SET body = '', body_enc = '', key_id = 0, deleted_at = NOW()
	          WHERE id = $1 AND deleted_at IS NULL`
	res, err := s.db.ExecContext(ctx, q, id)
	if err != nil {
		return fmt.Errorf("deleting chat message: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("chat message not found or already deleted")
	}
	return nil
}

// PinChatMessage pins a message in a channel (idempotent).
func (s *Store) PinChatMessage(ctx context.Context, channelID, messageID int64, pinnedBy string) error {
	const q = `INSERT INTO pinned_messages (channel_id, message_id, pinned_by)
	          VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`
	if _, err := s.db.ExecContext(ctx, q, channelID, messageID, pinnedBy); err != nil {
		return fmt.Errorf("pinning message: %w", err)
	}
	return nil
}

// UnpinChatMessage removes a pin.
func (s *Store) UnpinChatMessage(ctx context.Context, channelID, messageID int64) error {
	const q = `DELETE FROM pinned_messages WHERE channel_id = $1 AND message_id = $2`
	if _, err := s.db.ExecContext(ctx, q, channelID, messageID); err != nil {
		return fmt.Errorf("unpinning message: %w", err)
	}
	return nil
}

// PinnedMessage describes one pinned message with its (possibly tombstoned)
// content.
type PinnedMessage struct {
	MessageID int64
	PinnedBy  string
	PinnedAt  time.Time
	Message   *ChatMessage
}

// ChatPins lists a channel's pins, oldest first.
func (s *Store) ChatPins(ctx context.Context, channelID int64) ([]PinnedMessage, error) {
	const q = `SELECT message_id, pinned_by, pinned_at FROM pinned_messages
	          WHERE channel_id = $1 ORDER BY pinned_at`
	rows, err := s.db.QueryContext(ctx, q, channelID)
	if err != nil {
		return nil, fmt.Errorf("querying pins: %w", err)
	}
	defer rows.Close()

	var out []PinnedMessage
	for rows.Next() {
		var p PinnedMessage
		if err := rows.Scan(&p.MessageID, &p.PinnedBy, &p.PinnedAt); err != nil {
			return nil, fmt.Errorf("scanning pin: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		m, err := s.GetChatMessage(ctx, out[i].MessageID)
		if err != nil {
			return nil, err
		}
		out[i].Message = m
	}
	return out, nil
}

// ToggleReaction adds the (message, user, emoji) reaction if absent, removes
// it if present. It returns the full updated reaction map (emoji -> count)
// and whether the reaction is now present for this user.
func (s *Store) ToggleReaction(ctx context.Context, messageID int64, uniqueID, emoji string) (map[string]int, bool, error) {
	const del = `DELETE FROM chat_reactions WHERE message_id = $1 AND unique_id = $2 AND emoji = $3`
	res, err := s.db.ExecContext(ctx, del, messageID, uniqueID, emoji)
	if err != nil {
		return nil, false, fmt.Errorf("removing reaction: %w", err)
	}
	added := false
	if n, _ := res.RowsAffected(); n == 0 {
		const ins = `INSERT INTO chat_reactions (message_id, unique_id, emoji) VALUES ($1, $2, $3)`
		if _, err := s.db.ExecContext(ctx, ins, messageID, uniqueID, emoji); err != nil {
			return nil, false, fmt.Errorf("adding reaction: %w", err)
		}
		added = true
	}
	counts, err := s.Reactions(ctx, messageID)
	return counts, added, err
}

// Reactions returns the reaction counts (emoji -> count) for a message.
func (s *Store) Reactions(ctx context.Context, messageID int64) (map[string]int, error) {
	const q = `SELECT emoji, COUNT(*) FROM chat_reactions WHERE message_id = $1 GROUP BY emoji`
	rows, err := s.db.QueryContext(ctx, q, messageID)
	if err != nil {
		return nil, fmt.Errorf("querying reactions: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var emoji string
		var n int
		if err := rows.Scan(&emoji, &n); err != nil {
			return nil, fmt.Errorf("scanning reaction: %w", err)
		}
		out[emoji] = n
	}
	return out, rows.Err()
}

// ReactionsFor returns reaction counts for several messages at once
// (history pages).
func (s *Store) ReactionsFor(ctx context.Context, ids []int64) (map[int64]map[string]int, error) {
	out := make(map[int64]map[string]int, len(ids))
	for _, id := range ids {
		r, err := s.Reactions(ctx, id)
		if err != nil {
			return nil, err
		}
		out[id] = r
	}
	return out, nil
}

// SetServerSetting upserts a server setting (motd, announcement, ...).
// keyID names the global scope generation value is sealed under; 0 means the
// value is not sealed (server_name and other non-broadcast settings).
func (s *Store) SetServerSetting(ctx context.Context, key, value string, keyID uint32) error {
	const q = `INSERT INTO server_settings (key, value, key_id) VALUES ($1, $2, $3)
	          ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, key_id = EXCLUDED.key_id`
	if _, err := s.db.ExecContext(ctx, q, key, value, keyID); err != nil {
		return fmt.Errorf("setting server setting: %w", err)
	}
	return nil
}

// GetServerSetting returns a server setting and the generation it is sealed
// under ("" / 0 when unset or unsealed).
func (s *Store) GetServerSetting(ctx context.Context, key string) (string, uint32, error) {
	const q = `SELECT value, key_id FROM server_settings WHERE key = $1`
	var (
		v     string
		keyID int64
	)
	err := s.db.QueryRowContext(ctx, q, key).Scan(&v, &keyID)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", 0, nil
		}
		return "", 0, fmt.Errorf("loading server setting: %w", err)
	}
	return v, uint32(keyID), nil
}

// --- One-shot legacy backfill (migration 012) --------------------------------

// LegacyPlaintextPage returns up to limit pre-012 rows that still need
// sealing, ordered by id so the backfill is resumable. The predicate is
// body_enc = '' rather than body <> '': an encrypted message whose plaintext
// was the empty string stores exactly such a row, and it still has to be
// sealed to satisfy chat_messages_sealed. Tombstones are excluded — they
// carry no body (012 normalises them once).
func (s *Store) LegacyPlaintextPage(ctx context.Context, afterID int64, limit int) ([]LegacyChatRow, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	const q = `SELECT id, channel_id, body FROM chat_messages
	          WHERE body_enc = '' AND deleted_at IS NULL AND id > $1
	          ORDER BY id LIMIT $2`
	rows, err := s.db.QueryContext(ctx, q, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("querying legacy chat rows: %w", err)
	}
	defer rows.Close()
	var out []LegacyChatRow
	for rows.Next() {
		var r LegacyChatRow
		if err := rows.Scan(&r.ID, &r.ChannelID, &r.Body); err != nil {
			return nil, fmt.Errorf("scanning legacy chat row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetChatCiphertext installs the sealed body and blanks the legacy column.
func (s *Store) SetChatCiphertext(ctx context.Context, id int64, bodyEnc string, keyID uint32) error {
	const q = `UPDATE chat_messages SET body_enc = $1, key_id = $2, body = '' WHERE id = $3`
	if _, err := s.db.ExecContext(ctx, q, bodyEnc, keyID, id); err != nil {
		return fmt.Errorf("sealing legacy chat row %d: %w", id, err)
	}
	return nil
}

// PurgeLegacyPlaintext destroys all chat history instead of sealing it
// (chat_legacy_history=purge), for operators who would rather lose history
// than have it re-sealed under a server-minted key. It returns the row count.
func (s *Store) PurgeLegacyPlaintext(ctx context.Context) (int64, error) {
	const q = `UPDATE chat_messages
	          SET body = '', body_enc = '', key_id = 0, deleted_at = COALESCE(deleted_at, NOW())`
	res, err := s.db.ExecContext(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("purging legacy chat plaintext: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CountPlaintextBodies is the one-line ops audit: it is provably 0 after the
// backfill and stays 0, because chat_messages_no_plaintext enforces it.
func (s *Store) CountPlaintextBodies(ctx context.Context) (int64, error) {
	const q = `SELECT count(*) FROM chat_messages WHERE body <> ''`
	var n int64
	if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting plaintext chat bodies: %w", err)
	}
	return n, nil
}

// ValidateChatNoPlaintext promotes both 012 constraints from NOT VALID to
// validated once the backfill has finished. Each VALIDATE is a full table
// scan under an ACCESS EXCLUSIVE lock.
func (s *Store) ValidateChatNoPlaintext(ctx context.Context) error {
	for _, name := range []string{"chat_messages_no_plaintext", "chat_messages_sealed"} {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE chat_messages VALIDATE CONSTRAINT `+name); err != nil {
			return fmt.Errorf("validating %s: %w", name, err)
		}
	}
	return nil
}

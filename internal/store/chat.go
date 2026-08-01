// chat.go implements the store layer for server-side chat infrastructure
// (wave 5a): channel/global message history, pins, reactions, and server
// settings (MOTD/announcement). Bodies are stored decrypted — the server
// holds the scope keys (wave 4b); DMs are true E2EE and never stored here.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ChatMessage is one stored channel/global chat message. Body is empty for
// tombstoned (deleted) messages.
type ChatMessage struct {
	ID           int64
	ChannelID    int64 // 0 = global
	FromUniqueID string
	FromNickname string
	Body         string
	SentAt       time.Time
	EditedAt     *time.Time
	DeletedAt    *time.Time
}

// StoreChatMessage inserts a channel/global message and returns its ID.
// channelID 0 is the global scope.
func (s *Store) StoreChatMessage(ctx context.Context, channelID int64, fromUniqueID, fromNickname, body string) (int64, error) {
	scope := int16(1)
	if channelID == 0 {
		scope = 0
	}
	const q = `INSERT INTO chat_messages (scope, channel_id, from_unique_id, from_nickname, body)
	          VALUES ($1, $2, $3, $4, $5) RETURNING id, sent_at`
	var (
		id     int64
		sentAt time.Time
	)
	if err := s.db.QueryRowContext(ctx, q, scope, channelID, fromUniqueID, fromNickname, body).Scan(&id, &sentAt); err != nil {
		return 0, fmt.Errorf("storing chat message: %w", err)
	}
	return id, nil
}

// ChatHistory returns up to limit messages of a scope, newest first, with id
// strictly below beforeID (beforeID 0 = latest page). Tombstoned messages
// are included with an empty body and DeletedAt set.
func (s *Store) ChatHistory(ctx context.Context, channelID, beforeID int64, limit int) ([]ChatMessage, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT id, from_unique_id, from_nickname, body, sent_at, edited_at, deleted_at
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
		m.ChannelID = channelID
		if err := rows.Scan(&m.ID, &m.FromUniqueID, &m.FromNickname, &m.Body, &m.SentAt, &m.EditedAt, &m.DeletedAt); err != nil {
			return nil, fmt.Errorf("scanning chat message: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetChatMessage returns one message (for edit/delete/react authorization
// and rendering).
func (s *Store) GetChatMessage(ctx context.Context, id int64) (*ChatMessage, error) {
	const q = `SELECT channel_id, from_unique_id, from_nickname, body, sent_at, edited_at, deleted_at
	          FROM chat_messages WHERE id = $1`
	var m ChatMessage
	m.ID = id
	err := s.db.QueryRowContext(ctx, q, id).Scan(
		&m.ChannelID, &m.FromUniqueID, &m.FromNickname, &m.Body, &m.SentAt, &m.EditedAt, &m.DeletedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("querying chat message: %w", err)
	}
	return &m, nil
}

// EditChatMessage replaces the body and stamps edited_at.
func (s *Store) EditChatMessage(ctx context.Context, id int64, newBody string) error {
	const q = `UPDATE chat_messages SET body = $1, edited_at = NOW() WHERE id = $2 AND deleted_at IS NULL`
	res, err := s.db.ExecContext(ctx, q, newBody, id)
	if err != nil {
		return fmt.Errorf("editing chat message: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("chat message not found or deleted")
	}
	return nil
}

// DeleteChatMessage tombstones a message: body nulled, deleted_at stamped.
func (s *Store) DeleteChatMessage(ctx context.Context, id int64) error {
	const q = `UPDATE chat_messages SET body = '', deleted_at = NOW() WHERE id = $1 AND deleted_at IS NULL`
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
func (s *Store) SetServerSetting(ctx context.Context, key, value string) error {
	const q = `INSERT INTO server_settings (key, value) VALUES ($1, $2)
	          ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`
	if _, err := s.db.ExecContext(ctx, q, key, value); err != nil {
		return fmt.Errorf("setting server setting: %w", err)
	}
	return nil
}

// GetServerSetting returns a server setting ("" when unset).
func (s *Store) GetServerSetting(ctx context.Context, key string) (string, error) {
	const q = `SELECT value FROM server_settings WHERE key = $1`
	var v string
	err := s.db.QueryRowContext(ctx, q, key).Scan(&v)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("loading server setting: %w", err)
	}
	return v, nil
}

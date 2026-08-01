// bans.go implements the store layer for ban administration (wave 6b):
// listing and lifting bans. Ban enforcement at authenticate time lives in
// internal/auth.
package store

import (
	"context"
	"fmt"
	"time"
)

// BanRecord describes one row of the bans table.
type BanRecord struct {
	ID        int64
	Type      int // 0=IP, 1=unique_id, 2=nickname
	Value     string
	Reason    string
	BannedBy  string // unique ID of the issuer ("" when unknown)
	CreatedAt time.Time
	ExpiresAt *time.Time // nil = permanent
}

// ListBans returns all bans, newest first.
func (s *Store) ListBans(ctx context.Context) ([]BanRecord, error) {
	const q = `SELECT b.id, b.ban_type, b.value, COALESCE(b.reason, ''),
		          COALESCE(u.unique_id, ''), b.banned_at, b.expires_at
		FROM bans b LEFT JOIN users u ON u.id = b.banned_by
		ORDER BY b.id DESC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("listing bans: %w", err)
	}
	defer rows.Close()
	var out []BanRecord
	for rows.Next() {
		var r BanRecord
		if err := rows.Scan(&r.ID, &r.Type, &r.Value, &r.Reason, &r.BannedBy, &r.CreatedAt, &r.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteBan lifts one ban by ID.
func (s *Store) DeleteBan(ctx context.Context, id int64) error {
	const q = `DELETE FROM bans WHERE id = $1`
	if _, err := s.db.ExecContext(ctx, q, id); err != nil {
		return fmt.Errorf("deleting ban: %w", err)
	}
	return nil
}

// files.go implements the file-transfer metadata queries against the files
// table (migration 003_files.sql).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrFileNotFound is returned when a file record does not exist.
var ErrFileNotFound = errors.New("file not found")

// FileRecord describes one uploaded file.
type FileRecord struct {
	ID         int64
	ChannelID  int64
	Name       string
	Size       int64
	SHA256     string
	Uploader   string
	UploadedAt time.Time
}

// AddFile inserts or replaces a file record. (channel_id, name) is unique,
// so uploading the same name again replaces the previous record.
func (s *Store) AddFile(ctx context.Context, rec FileRecord) error {
	const q = `INSERT INTO files (channel_id, name, size, sha256, uploader)
	          VALUES ($1, $2, $3, $4, $5)
	          ON CONFLICT (channel_id, name) DO UPDATE
	          SET size = EXCLUDED.size, sha256 = EXCLUDED.sha256,
	              uploader = EXCLUDED.uploader, uploaded_at = NOW()`
	if _, err := s.db.ExecContext(ctx, q, rec.ChannelID, rec.Name, rec.Size, rec.SHA256, rec.Uploader); err != nil {
		return fmt.Errorf("upserting file: %w", err)
	}
	return nil
}

// GetFile returns the file record for a channel/name pair, or
// ErrFileNotFound.
func (s *Store) GetFile(ctx context.Context, channelID int64, name string) (*FileRecord, error) {
	var rec FileRecord
	const q = `SELECT id, channel_id, name, size, sha256, COALESCE(uploader, ''), uploaded_at
	          FROM files WHERE channel_id = $1 AND name = $2`
	err := s.db.QueryRowContext(ctx, q, channelID, name).
		Scan(&rec.ID, &rec.ChannelID, &rec.Name, &rec.Size, &rec.SHA256, &rec.Uploader, &rec.UploadedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFileNotFound
		}
		return nil, fmt.Errorf("querying file: %w", err)
	}
	return &rec, nil
}

// ListFiles returns all files in a channel, ordered by name.
func (s *Store) ListFiles(ctx context.Context, channelID int64) ([]FileRecord, error) {
	const q = `SELECT id, channel_id, name, size, sha256, COALESCE(uploader, ''), uploaded_at
	          FROM files WHERE channel_id = $1 ORDER BY name`
	rows, err := s.db.QueryContext(ctx, q, channelID)
	if err != nil {
		return nil, fmt.Errorf("querying files: %w", err)
	}
	defer rows.Close()

	var out []FileRecord
	for rows.Next() {
		var rec FileRecord
		if err := rows.Scan(&rec.ID, &rec.ChannelID, &rec.Name, &rec.Size,
			&rec.SHA256, &rec.Uploader, &rec.UploadedAt); err != nil {
			return nil, fmt.Errorf("scanning file: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// ChannelFileUsage returns the total bytes of all files in a channel (used
// for quota enforcement).
func (s *Store) ChannelFileUsage(ctx context.Context, channelID int64) (int64, error) {
	var total int64
	const q = `SELECT COALESCE(SUM(size), 0) FROM files WHERE channel_id = $1`
	if err := s.db.QueryRowContext(ctx, q, channelID).Scan(&total); err != nil {
		return 0, fmt.Errorf("querying channel file usage: %w", err)
	}
	return total, nil
}

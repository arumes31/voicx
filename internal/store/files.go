// files.go implements the file-transfer metadata queries against the files
// table (migration 003_files.sql, folders from 010_file_folders.sql).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

// ErrFileNotFound is returned when a file record does not exist.
var ErrFileNotFound = errors.New("file not found")

// ErrFileExists is returned when a move targets an occupied channel/folder/name.
var ErrFileExists = errors.New("file already exists")

// FileRecord describes one uploaded file.
type FileRecord struct {
	ID        int64
	ChannelID int64
	Folder    string // virtual folder ('' = channel root), migration 010
	Name      string
	Size      int64
	SHA256    string
	Uploader  string
	// Encrypted marks a client-encrypted chat attachment (91-135, migration
	// 012): the blob is ciphertext whose key exists only inside the encrypted
	// chat message that references it, so the file browser and download links
	// must refuse to serve it.
	Encrypted  bool
	UploadedAt time.Time
}

// fileCols is the shared SELECT column list for file rows.
const fileCols = `id, channel_id, folder, name, size, sha256, COALESCE(uploader, ''), encrypted, uploaded_at`

// scanFile scans one file row.
func scanFile(rows interface{ Scan(...any) error }, rec *FileRecord) error {
	return rows.Scan(&rec.ID, &rec.ChannelID, &rec.Folder, &rec.Name, &rec.Size,
		&rec.SHA256, &rec.Uploader, &rec.Encrypted, &rec.UploadedAt)
}

// AddFile inserts or replaces a file record. (channel_id, folder, name) is
// unique, so uploading the same name again in the same folder replaces the
// previous record.
func (s *Store) AddFile(ctx context.Context, rec FileRecord) error {
	const q = `INSERT INTO files (channel_id, folder, name, size, sha256, uploader, encrypted)
	          VALUES ($1, $2, $3, $4, $5, $6, $7)
	          ON CONFLICT (channel_id, folder, name) DO UPDATE
	          SET size = EXCLUDED.size, sha256 = EXCLUDED.sha256,
	              uploader = EXCLUDED.uploader, encrypted = EXCLUDED.encrypted,
	              uploaded_at = NOW()`
	if _, err := s.db.ExecContext(ctx, q, rec.ChannelID, rec.Folder, rec.Name, rec.Size, rec.SHA256, rec.Uploader, rec.Encrypted); err != nil {
		return fmt.Errorf("upserting file: %w", err)
	}
	return nil
}

// GetFile returns the file record for a channel/folder/name triple, or
// ErrFileNotFound.
func (s *Store) GetFile(ctx context.Context, channelID int64, folder, name string) (*FileRecord, error) {
	var rec FileRecord
	q := `SELECT ` + fileCols + ` FROM files WHERE channel_id = $1 AND folder = $2 AND name = $3`
	err := s.db.QueryRowContext(ctx, q, channelID, folder, name).Scan(
		&rec.ID, &rec.ChannelID, &rec.Folder, &rec.Name, &rec.Size, &rec.SHA256, &rec.Uploader,
		&rec.Encrypted, &rec.UploadedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFileNotFound
		}
		return nil, fmt.Errorf("querying file: %w", err)
	}
	return &rec, nil
}

// ListFiles returns the files in one folder of a channel, ordered by name.
func (s *Store) ListFiles(ctx context.Context, channelID int64, folder string) ([]FileRecord, error) {
	q := `SELECT ` + fileCols + ` FROM files WHERE channel_id = $1 AND folder = $2 ORDER BY name`
	rows, err := s.db.QueryContext(ctx, q, channelID, folder)
	if err != nil {
		return nil, fmt.Errorf("querying files: %w", err)
	}
	defer closeRows(rows)

	var out []FileRecord
	for rows.Next() {
		var rec FileRecord
		if err := scanFile(rows, &rec); err != nil {
			return nil, fmt.Errorf("scanning file: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// ListFileFolders returns the distinct non-empty folders of a channel
// (virtual folders, derived from file rows).
func (s *Store) ListFileFolders(ctx context.Context, channelID int64) ([]string, error) {
	const q = `SELECT DISTINCT folder FROM files WHERE channel_id = $1 AND folder <> '' ORDER BY folder`
	rows, err := s.db.QueryContext(ctx, q, channelID)
	if err != nil {
		return nil, fmt.Errorf("querying file folders: %w", err)
	}
	defer closeRows(rows)
	var out []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ListFileVersions returns the rotated old versions (<name>.v1..v3) of a
// file, newest first (264).
func (s *Store) ListFileVersions(ctx context.Context, channelID int64, folder, baseName string) ([]FileRecord, error) {
	q := `SELECT ` + fileCols + ` FROM files
		WHERE channel_id = $1 AND folder = $2 AND name LIKE $3 ESCAPE '\' ORDER BY name`
	pattern := likeEscape(baseName) + `.v%`
	rows, err := s.db.QueryContext(ctx, q, channelID, folder, pattern)
	if err != nil {
		return nil, fmt.Errorf("querying file versions: %w", err)
	}
	defer closeRows(rows)
	var out []FileRecord
	for rows.Next() {
		var rec FileRecord
		if err := scanFile(rows, &rec); err != nil {
			return nil, fmt.Errorf("scanning file: %w", err)
		}
		// LIKE also matches oddballs like "x.vfoo"; keep only .v<N> shapes.
		rest := strings.TrimPrefix(rec.Name, baseName+".v")
		if rest == rec.Name || len(rest) != 1 || rest[0] < '1' || rest[0] > '9' {
			continue
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// likeEscape escapes LIKE metacharacters for use with ESCAPE '\'.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// RenameFile moves/renames a file record within its channel (262).
func (s *Store) RenameFile(ctx context.Context, channelID int64, folder, name, newFolder, newName string) error {
	return s.MoveFile(ctx, channelID, folder, name, channelID, newFolder, newName)
}

// MoveFile relocates a file record, possibly into another channel (262). The
// uploader travels with the row, so the file keeps counting against the
// person who put it there (266) rather than against whoever moved it.
func (s *Store) MoveFile(ctx context.Context, channelID int64, folder, name string, newChannelID int64, newFolder, newName string) error {
	const q = `UPDATE files SET channel_id = $4, folder = $5, name = $6
	          WHERE channel_id = $1 AND folder = $2 AND name = $3`
	res, err := s.db.ExecContext(ctx, q, channelID, folder, name, newChannelID, newFolder, newName)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			return fmt.Errorf("%w: %s", ErrFileExists, newName)
		}
		return fmt.Errorf("moving file: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrFileNotFound
	}
	return nil
}

// DeleteFile removes a file record (263).
func (s *Store) DeleteFile(ctx context.Context, channelID int64, folder, name string) error {
	const q = `DELETE FROM files WHERE channel_id = $1 AND folder = $2 AND name = $3`
	res, err := s.db.ExecContext(ctx, q, channelID, folder, name)
	if err != nil {
		return fmt.Errorf("deleting file: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrFileNotFound
	}
	return nil
}

// FindFileBySHA returns another file in the channel with the same content
// hash (275 dedup), excluding the given folder/name. Returns (nil, nil) when
// there is none.
func (s *Store) FindFileBySHA(ctx context.Context, channelID int64, sha256, exclFolder, exclName string) (*FileRecord, error) {
	var rec FileRecord
	q := `SELECT ` + fileCols + ` FROM files
		WHERE channel_id = $1 AND sha256 = $2 AND NOT (folder = $3 AND name = $4)
		ORDER BY uploaded_at DESC LIMIT 1`
	err := s.db.QueryRowContext(ctx, q, channelID, sha256, exclFolder, exclName).Scan(
		&rec.ID, &rec.ChannelID, &rec.Folder, &rec.Name, &rec.Size, &rec.SHA256, &rec.Uploader,
		&rec.Encrypted, &rec.UploadedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying file by sha256: %w", err)
	}
	return &rec, nil
}

// ChannelFileUsage returns the bytes a channel's files physically occupy
// (265). Identical blobs inside a channel are hard-linked rather than stored
// twice (275 dedup), so summing logical row sizes charges a deduped copy
// against the quota even though it costs no disk: count each distinct content
// hash once.
func (s *Store) ChannelFileUsage(ctx context.Context, channelID int64) (int64, error) {
	var total int64
	const q = `SELECT COALESCE(SUM(sz), 0) FROM
	          (SELECT MAX(size) AS sz FROM files WHERE channel_id = $1 GROUP BY sha256) t`
	if err := s.db.QueryRowContext(ctx, q, channelID).Scan(&total); err != nil {
		return 0, fmt.Errorf("querying channel file usage: %w", err)
	}
	return total, nil
}

// UploaderFileUsage returns the bytes one uploader's files physically occupy
// across every channel (266). Dedup only hard-links within a channel, so the
// same blob uploaded to two channels really is stored twice and is charged
// twice here.
func (s *Store) UploaderFileUsage(ctx context.Context, uploader string) (int64, error) {
	if uploader == "" {
		return 0, nil
	}
	var total int64
	const q = `SELECT COALESCE(SUM(sz), 0) FROM
	          (SELECT MAX(size) AS sz FROM files WHERE uploader = $1 GROUP BY channel_id, sha256) t`
	if err := s.db.QueryRowContext(ctx, q, uploader).Scan(&total); err != nil {
		return 0, fmt.Errorf("querying uploader file usage: %w", err)
	}
	return total, nil
}

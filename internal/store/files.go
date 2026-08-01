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
)

// ErrFileNotFound is returned when a file record does not exist.
var ErrFileNotFound = errors.New("file not found")

// FileRecord describes one uploaded file.
type FileRecord struct {
	ID         int64
	ChannelID  int64
	Folder     string // virtual folder ('' = channel root), migration 010
	Name       string
	Size       int64
	SHA256     string
	Uploader   string
	UploadedAt time.Time
}

// fileCols is the shared SELECT column list for file rows.
const fileCols = `id, channel_id, folder, name, size, sha256, COALESCE(uploader, ''), uploaded_at`

// scanFile scans one file row.
func scanFile(rows interface{ Scan(...any) error }, rec *FileRecord) error {
	return rows.Scan(&rec.ID, &rec.ChannelID, &rec.Folder, &rec.Name, &rec.Size,
		&rec.SHA256, &rec.Uploader, &rec.UploadedAt)
}

// AddFile inserts or replaces a file record. (channel_id, folder, name) is
// unique, so uploading the same name again in the same folder replaces the
// previous record.
func (s *Store) AddFile(ctx context.Context, rec FileRecord) error {
	const q = `INSERT INTO files (channel_id, folder, name, size, sha256, uploader)
	          VALUES ($1, $2, $3, $4, $5, $6)
	          ON CONFLICT (channel_id, folder, name) DO UPDATE
	          SET size = EXCLUDED.size, sha256 = EXCLUDED.sha256,
	              uploader = EXCLUDED.uploader, uploaded_at = NOW()`
	if _, err := s.db.ExecContext(ctx, q, rec.ChannelID, rec.Folder, rec.Name, rec.Size, rec.SHA256, rec.Uploader); err != nil {
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
		&rec.ID, &rec.ChannelID, &rec.Folder, &rec.Name, &rec.Size, &rec.SHA256, &rec.Uploader, &rec.UploadedAt)
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
	defer rows.Close()

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
	defer rows.Close()
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
	defer rows.Close()
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

// RenameFile moves/renames a file record (262).
func (s *Store) RenameFile(ctx context.Context, channelID int64, folder, name, newFolder, newName string) error {
	const q = `UPDATE files SET folder = $4, name = $5 WHERE channel_id = $1 AND folder = $2 AND name = $3`
	res, err := s.db.ExecContext(ctx, q, channelID, folder, name, newFolder, newName)
	if err != nil {
		return fmt.Errorf("renaming file: %w", err)
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
		&rec.ID, &rec.ChannelID, &rec.Folder, &rec.Name, &rec.Size, &rec.SHA256, &rec.Uploader, &rec.UploadedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying file by sha256: %w", err)
	}
	return &rec, nil
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

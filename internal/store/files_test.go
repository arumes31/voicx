// files_test.go DB-backed tests for the wave-7 file metadata (folders,
// rename/move, versions, dedup lookup). Skip pattern like the other store
// tests: without a reachable Postgres they skip.
package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestFilesDB covers the folder-aware file metadata: add/list per folder,
// rename/move, version listing, dedup lookup, and delete.
func TestFilesDB(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())

	var channelID int64
	if err := s.DB().QueryRowContext(ctx,
		`INSERT INTO channels (name, channel_type, created_at) VALUES ($1, 2, NOW()) RETURNING id`,
		"w7_files_"+suffix).Scan(&channelID); err != nil {
		t.Fatalf("seeding channel: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.DB().ExecContext(ctx, `DELETE FROM channels WHERE id = $1`, channelID)
	})

	add := func(folder, name, sha string, size int64) {
		t.Helper()
		if err := s.AddFile(ctx, FileRecord{ChannelID: channelID, Folder: folder, Name: name, Size: size, SHA256: sha, Uploader: "u"}); err != nil {
			t.Fatalf("AddFile %s/%s: %v", folder, name, err)
		}
	}

	add("", "a.txt", "sha-a", 10)
	add("docs", "a.txt", "sha-a2", 20)
	add("docs/2024", "b.txt", "sha-b", 30)

	// Same name in different folders coexists; listing is folder-scoped.
	root, err := s.ListFiles(ctx, channelID, "")
	if err != nil || len(root) != 1 || root[0].Size != 10 {
		t.Fatalf("root files = %+v, err=%v", root, err)
	}
	docs, err := s.ListFiles(ctx, channelID, "docs")
	if err != nil || len(docs) != 1 || docs[0].Folder != "docs" {
		t.Fatalf("docs files = %+v, err=%v", docs, err)
	}

	// Overwrite in one folder leaves the other folder's row alone.
	add("", "a.txt", "sha-a-new", 11)
	docs, _ = s.ListFiles(ctx, channelID, "docs")
	if len(docs) != 1 || docs[0].Size != 20 {
		t.Fatalf("docs files after root overwrite = %+v", docs)
	}

	// Folders are derived from rows.
	folders, err := s.ListFileFolders(ctx, channelID)
	if err != nil || len(folders) != 2 {
		t.Fatalf("folders = %v, err=%v", folders, err)
	}

	// Rename + move.
	if err := s.RenameFile(ctx, channelID, "docs/2024", "b.txt", "docs", "b-renamed.txt"); err != nil {
		t.Fatalf("RenameFile: %v", err)
	}
	rec, err := s.GetFile(ctx, channelID, "docs", "b-renamed.txt")
	if err != nil || rec.Size != 30 {
		t.Fatalf("moved record = %+v, err=%v", rec, err)
	}
	if _, err := s.GetFile(ctx, channelID, "docs/2024", "b.txt"); err != ErrFileNotFound {
		t.Fatalf("old record still present: %v", err)
	}

	// Versions.
	add("", "v.txt", "sha-v3", 3)
	add("", "v.txt.v1", "sha-v2", 2)
	add("", "v.txt.v2", "sha-v1", 1)
	versions, err := s.ListFileVersions(ctx, channelID, "", "v.txt")
	if err != nil || len(versions) != 2 || versions[0].Name != "v.txt.v1" {
		t.Fatalf("versions = %+v, err=%v", versions, err)
	}

	// Dedup lookup finds the identical blob elsewhere in the channel.
	dup, err := s.FindFileBySHA(ctx, channelID, "sha-a-new", "docs", "other.txt")
	if err != nil || dup == nil || dup.Name != "a.txt" {
		t.Fatalf("FindFileBySHA = %+v, err=%v", dup, err)
	}
	if dup, _ := s.FindFileBySHA(ctx, channelID, "sha-nope", "", "x"); dup != nil {
		t.Fatalf("unexpected dedup hit: %+v", dup)
	}

	// Delete.
	if err := s.DeleteFile(ctx, channelID, "", "a.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if _, err := s.GetFile(ctx, channelID, "", "a.txt"); err != ErrFileNotFound {
		t.Fatalf("record after delete: %v", err)
	}
	if err := s.DeleteFile(ctx, channelID, "", "a.txt"); err != ErrFileNotFound {
		t.Fatalf("second delete = %v, want ErrFileNotFound", err)
	}
}

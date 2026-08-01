// files_test.go exercises the file-transfer control handlers (token issuance
// and file listing) over real TCP with a fake FileTransferBackend.
package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"voicx/internal/auth"
	"voicx/internal/netproto"
	"voicx/internal/permissions"
	"voicx/internal/store"
)

// fakeFileTransfer implements FileTransferBackend, recording calls.
type fakeFileTransfer struct {
	mu          sync.Mutex
	uploads     []ftCall
	downloads   []ftCall
	files       []store.FileRecord
	deleted     []string
	renamed     [][2]string
	fingerprint string
}

type ftCall struct {
	channelID int64
	folder    string
	name      string
	size      int64
	uploader  string
}

func (f *fakeFileTransfer) InitUpload(_ context.Context, channelID int64, folder, name string, size int64, uploader string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, ftCall{channelID, folder, name, size, uploader})
	return "tid-1", "tok-1", nil
}

func (f *fakeFileTransfer) InitDownload(_ context.Context, channelID int64, folder, name string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downloads = append(f.downloads, ftCall{channelID: channelID, folder: folder, name: name})
	return "tid-2", "tok-2", nil
}

func (f *fakeFileTransfer) ListFiles(_ context.Context, channelID int64, folder string) ([]store.FileRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.FileRecord
	for _, rec := range f.files {
		if rec.Folder == folder {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (f *fakeFileTransfer) ListFileFolders(context.Context, int64) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, rec := range f.files {
		if rec.Folder != "" && !seen[rec.Folder] {
			seen[rec.Folder] = true
			out = append(out, rec.Folder)
		}
	}
	return out, nil
}

func (f *fakeFileTransfer) ListFileVersions(_ context.Context, channelID int64, folder, name string) ([]store.FileRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.FileRecord
	for _, rec := range f.files {
		if len(rec.Name) > len(name)+2 && rec.Name[:len(name)+2] == name+".v" {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (f *fakeFileTransfer) DeleteFile(_ context.Context, channelID int64, folder, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, folder+"/"+name)
	keep := f.files[:0]
	for _, rec := range f.files {
		if !(rec.Folder == folder && rec.Name == name) {
			keep = append(keep, rec)
		}
	}
	f.files = keep
	return nil
}

func (f *fakeFileTransfer) RenameFile(_ context.Context, channelID int64, folder, name, newFolder, newName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renamed = append(f.renamed, [2]string{folder + "/" + name, newFolder + "/" + newName})
	for i, rec := range f.files {
		if rec.Folder == folder && rec.Name == name {
			f.files[i].Folder = newFolder
			f.files[i].Name = newName
		}
	}
	return nil
}

func (f *fakeFileTransfer) ChannelQuota(context.Context, int64) (int64, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var used int64
	for _, rec := range f.files {
		used += rec.Size
	}
	return used, 10 << 20, nil
}

func (f *fakeFileTransfer) CreateLink(_ context.Context, channelID int64, folder, name string) (string, time.Time, error) {
	return "deadbeef", time.Now().Add(15 * time.Minute), nil
}

func (f *fakeFileTransfer) Port() int { return 30033 }

// fingerprint is the data port's certificate fingerprint ("" = TLS off).
func (f *fakeFileTransfer) Fingerprint() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fingerprint
}

func (f *fakeFileTransfer) uploadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.uploads)
}

// TestFileTransferInitUpload verifies an upload token is issued and the
// response carries id/token/port.
func TestFileTransferInitUpload(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgFileTransferInit, netproto.FileTransferInit{
		ChannelID: 1, Direction: "upload", Name: "a.txt", Size: 42,
	})
	f := readOfType(t, conn, netproto.MsgFileTransferInitResponse)
	var resp netproto.FileTransferInitResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.TransferID != "tid-1" || resp.Token != "tok-1" || resp.Port != 30033 {
		t.Fatalf("response = %+v", resp)
	}

	env.ft.mu.Lock()
	defer env.ft.mu.Unlock()
	if len(env.ft.uploads) != 1 || env.ft.uploads[0].name != "a.txt" || env.ft.uploads[0].size != 42 || env.ft.uploads[0].uploader != "user-uid" {
		t.Fatalf("uploads = %+v", env.ft.uploads)
	}
}

// TestFileTransferInitDenied verifies a negated upload power denies token
// issuance.
func TestFileTransferInitDenied(t *testing.T) {
	perms := tieredWith(&permissions.Permission{
		Key:    permissions.PermissionKeyFTFileUploadPower,
		Type:   permissions.PermissionTypeInteger,
		Value:  0,
		Negate: true,
	})
	env := startTestEnv(t, &perms)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgFileTransferInit, netproto.FileTransferInit{
		ChannelID: 1, Direction: "upload", Name: "a.txt", Size: 42,
	})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("error code = %d, want %d", e.Code, errCodePermissionDenied)
	}
	if got := env.ft.uploadCount(); got != 0 {
		t.Fatalf("uploads issued = %d, want 0", got)
	}
}

// TestFileTransferInitDownload verifies a download token is issued.
func TestFileTransferInitDownload(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgFileTransferInit, netproto.FileTransferInit{
		ChannelID: 1, Direction: "download", Name: "a.txt",
	})
	f := readOfType(t, conn, netproto.MsgFileTransferInitResponse)
	var resp netproto.FileTransferInitResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.TransferID != "tid-2" || resp.Token != "tok-2" {
		t.Fatalf("response = %+v", resp)
	}
}

// TestFileList verifies the channel file listing.
func TestFileList(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	env.ft.files = []store.FileRecord{
		{ChannelID: 1, Name: "a.txt", Size: 42, SHA256: "abc", Uploader: "user-uid"},
	}

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgFileList, netproto.FileList{ChannelID: 1})
	f := readOfType(t, conn, netproto.MsgFileListResponse)
	var resp netproto.FileListResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Entries) != 1 || resp.Entries[0].Name != "a.txt" || resp.Entries[0].Size != 42 || resp.Entries[0].SHA256 != "abc" {
		t.Fatalf("entries = %+v", resp.Entries)
	}
}

// --- wave 7: file management handlers -----------------------------------------

// TestFileListQuota verifies the quota fields on the list response (265).
func TestFileListQuota(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	env.ft.files = []store.FileRecord{
		{ChannelID: 1, Name: "a.txt", Size: 42, SHA256: "abc", Uploader: "user-uid"},
		{ChannelID: 1, Name: "b.txt", Size: 58, SHA256: "def", Uploader: "admin-uid"},
	}

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgFileList, netproto.FileList{ChannelID: 1})
	f := readOfType(t, conn, netproto.MsgFileListResponse)
	var resp netproto.FileListResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.UsedBytes != 100 || resp.QuotaBytes != 10<<20 {
		t.Fatalf("quota = %d/%d, want 100/%d", resp.UsedBytes, resp.QuotaBytes, 10<<20)
	}
}

// TestFileDeleteGates verifies delete: uploader may, others may not, admins
// bypass (263).
func TestFileDeleteGates(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	env.ft.files = []store.FileRecord{
		{ChannelID: 1, Name: "owned.txt", Size: 1, Uploader: "user-uid"},
		{ChannelID: 1, Name: "foreign.txt", Size: 1, Uploader: "admin-uid"},
	}

	userConn, _ := dialAuthed(t, env.addr, "user-uid")
	defer userConn.Close()

	// Uploader deletes their own file.
	send(t, userConn, netproto.MsgFileDelete, netproto.FileDelete{ChannelID: 1, Name: "owned.txt"})
	waitFor(t, "own file deleted", func() bool {
		env.ft.mu.Lock()
		defer env.ft.mu.Unlock()
		return len(env.ft.deleted) == 1
	})

	// ...but not someone else's.
	send(t, userConn, netproto.MsgFileDelete, netproto.FileDelete{ChannelID: 1, Name: "foreign.txt"})
	if e := readError(t, userConn); e.Code != errCodePermissionDenied {
		t.Fatalf("error = %+v, want permission denied", e)
	}

	// Admin bypasses.
	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer adminConn.Close()
	send(t, adminConn, netproto.MsgFileDelete, netproto.FileDelete{ChannelID: 1, Name: "foreign.txt"})
	waitFor(t, "admin delete", func() bool {
		env.ft.mu.Lock()
		defer env.ft.mu.Unlock()
		return len(env.ft.deleted) == 2
	})
	if got := env.groups.auditActions(); len(got) == 0 || got[len(got)-1] != "file_delete" {
		t.Fatalf("audit = %v", got)
	}
}

// TestFileRename verifies rename/move with the uploader gate (262).
func TestFileRename(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	env.ft.files = []store.FileRecord{
		{ChannelID: 1, Name: "old.txt", Size: 1, Uploader: "user-uid"},
	}

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgFileRename, netproto.FileRename{
		ChannelID: 1, Name: "old.txt", NewName: "new.txt", NewFolder: "docs",
	})
	waitFor(t, "file renamed", func() bool {
		env.ft.mu.Lock()
		defer env.ft.mu.Unlock()
		return len(env.ft.renamed) == 1
	})
	env.ft.mu.Lock()
	r := env.ft.renamed[0]
	env.ft.mu.Unlock()
	if r != [2]string{"/old.txt", "docs/new.txt"} {
		t.Fatalf("rename = %v", r)
	}
}

// TestFileVersions verifies the version listing handler (264).
func TestFileVersions(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	env.ft.files = []store.FileRecord{
		{ChannelID: 1, Name: "a.txt", Size: 3},
		{ChannelID: 1, Name: "a.txt.v1", Size: 2},
		{ChannelID: 1, Name: "a.txt.v2", Size: 1},
	}

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgFileVersions, netproto.FileVersions{ChannelID: 1, Name: "a.txt"})
	f := readOfType(t, conn, netproto.MsgFileVersionsResponse)
	var resp netproto.FileVersionsResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Entries) != 2 || resp.Entries[0].Name != "a.txt.v1" {
		t.Fatalf("versions = %+v", resp.Entries)
	}
}

// TestFileLink verifies link creation is uploader/admin only (267) and the
// response carries a /dl/ URL.
func TestFileLink(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	env.ft.files = []store.FileRecord{
		{ChannelID: 1, Name: "a.txt", Size: 1, Uploader: "user-uid"},
	}

	// Another user: denied.
	env.auth.users["other-uid"] = &authUser3
	env.auth.passwords["other-uid"] = "pw"
	otherConn, _ := dialAuthed(t, env.addr, "other-uid")
	defer otherConn.Close()
	send(t, otherConn, netproto.MsgFileLink, netproto.FileLink{ChannelID: 1, Name: "a.txt"})
	if e := readError(t, otherConn); e.Code != errCodePermissionDenied {
		t.Fatalf("error = %+v, want permission denied", e)
	}

	// The uploader gets a link.
	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()
	send(t, conn, netproto.MsgFileLink, netproto.FileLink{ChannelID: 1, Name: "a.txt"})
	f := readOfType(t, conn, netproto.MsgFileLinkResponse)
	var resp netproto.FileLinkResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Path == "" || resp.ExpiresAt == 0 || resp.HealthPort == 0 {
		t.Fatalf("link response = %+v", resp)
	}
	if resp.Path != "/dl/deadbeef" {
		t.Fatalf("path = %q, want /dl/deadbeef", resp.Path)
	}
}

// authUser3 is a third fake user for gate tests.
var authUser3 = auth.User{ID: 3, UniqueID: "other-uid", Nickname: "other", IsAdmin: false}

// TestServerIconSetGet verifies the server icon round trip (270).
func TestServerIconSetGet(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	// Non-admin: denied.
	userConn, _ := dialAuthed(t, env.addr, "user-uid")
	defer userConn.Close()
	send(t, userConn, netproto.MsgServerIconSet, netproto.ServerIconSet{DataBase64: b64(tinyPNG)})
	if e := readError(t, userConn); e.Code != errCodePermissionDenied {
		t.Fatalf("error = %+v, want permission denied", e)
	}

	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer adminConn.Close()
	send(t, adminConn, netproto.MsgServerIconSet, netproto.ServerIconSet{DataBase64: b64(tinyPNG)})
	deadline := time.Now().Add(3 * time.Second)
	for {
		send(t, adminConn, netproto.MsgServerIconGet, netproto.ServerIconGet{})
		f := readOfType(t, adminConn, netproto.MsgServerIconData)
		var data netproto.ServerIconData
		if err := netproto.Decode(f, &data); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if data.DataBase64 == b64(tinyPNG) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server icon not readable after set")
		}
	}
}

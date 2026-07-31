// files_test.go exercises the file-transfer control handlers (token issuance
// and file listing) over real TCP with a fake FileTransferBackend.
package server

import (
	"context"
	"sync"
	"testing"

	"voicx/internal/netproto"
	"voicx/internal/permissions"
	"voicx/internal/store"
)

// fakeFileTransfer implements FileTransferBackend, recording calls.
type fakeFileTransfer struct {
	mu        sync.Mutex
	uploads   []ftCall
	downloads []ftCall
	files     []store.FileRecord
}

type ftCall struct {
	channelID int64
	name      string
	size      int64
	uploader  string
}

func (f *fakeFileTransfer) InitUpload(_ context.Context, channelID int64, name string, size int64, uploader string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, ftCall{channelID, name, size, uploader})
	return "tid-1", "tok-1", nil
}

func (f *fakeFileTransfer) InitDownload(_ context.Context, channelID int64, name string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downloads = append(f.downloads, ftCall{channelID: channelID, name: name})
	return "tid-2", "tok-2", nil
}

func (f *fakeFileTransfer) ListFiles(_ context.Context, channelID int64) ([]store.FileRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.files, nil
}

func (f *fakeFileTransfer) Port() int { return 30033 }

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

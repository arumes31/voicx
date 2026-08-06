// transfer_test.go exercises the file-transfer protocol over real TCP:
// upload/download round-trips, abort paths, and init-frame validation.
package filetransfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"voicx/internal/netproto"
	"voicx/internal/store"
)

// startServer starts a file-transfer server on an ephemeral port and returns
// its address and the server.
func startServer(t *testing.T, fs FileStore) (string, *Server) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	s := New(Config{Addr: addr, RootDir: t.TempDir()}, fs, nil)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			_ = conn.Close()
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		cancel()
		_ = s.Close()
		startErr := <-errCh
		t.Fatalf("file-transfer server did not become ready: %v", startErr)
	}
	t.Cleanup(func() {
		cancel()
		_ = s.Close()
		<-errCh
	})
	return addr, s
}

// dialTransfer connects and sends the init frame.
func dialTransfer(t *testing.T, addr, transferID, token string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := writeJSON(conn, frameInit, initMsg{Token: token, TransferID: transferID}); err != nil {
		t.Fatalf("write init: %v", err)
	}
	return conn
}

// readStatus reads the final status frame.
func readStatus(t *testing.T, conn net.Conn) statusMsg {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	f, err := netproto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if f.Type != frameStatus {
		t.Fatalf("frame type = %d, want status (%d)", f.Type, frameStatus)
	}
	var st statusMsg
	if err := json.Unmarshal(f.Payload, &st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return st
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestUploadRoundTrip uploads a file end-to-end and verifies the on-disk
// content, the store record, and the ok status.
func TestUploadRoundTrip(t *testing.T) {
	fs := newFakeFileStore()
	addr, s := startServer(t, fs)

	content := []byte("hello world")
	id, token, err := s.InitUpload(context.Background(), 7, "", "hello.txt", int64(len(content)), "uid-1", 0)
	if err != nil {
		t.Fatalf("InitUpload: %v", err)
	}

	conn := dialTransfer(t, addr, id, token)
	defer func() { _ = conn.Close() }()

	// Two chunks, then the digest.
	for _, chunk := range [][]byte{content[:6], content[6:]} {
		if err := netproto.WriteFrame(conn, &netproto.Frame{Type: frameChunk, Payload: chunk}); err != nil {
			t.Fatalf("write chunk: %v", err)
		}
	}
	if err := writeJSON(conn, frameDigest, digestMsg{SHA256: sha256Hex(content)}); err != nil {
		t.Fatalf("write digest: %v", err)
	}

	st := readStatus(t, conn)
	if !st.OK {
		t.Fatalf("status = %+v, want ok", st)
	}

	onDisk, err := os.ReadFile(filepath.Join(s.cfg.RootDir, "7", "hello.txt"))
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if string(onDisk) != string(content) {
		t.Fatalf("file content = %q, want %q", onDisk, content)
	}
	if runtime.GOOS != "windows" {
		fileInfo, err := os.Stat(filepath.Join(s.cfg.RootDir, "7", "hello.txt"))
		if err != nil {
			t.Fatalf("stat uploaded file: %v", err)
		}
		if got := fileInfo.Mode().Perm(); got != 0o600 {
			t.Errorf("uploaded file mode = %o, want 600", got)
		}
		dirInfo, err := os.Stat(filepath.Join(s.cfg.RootDir, "7"))
		if err != nil {
			t.Fatalf("stat channel directory: %v", err)
		}
		if got := dirInfo.Mode().Perm(); got != 0o700 {
			t.Errorf("channel directory mode = %o, want 700", got)
		}
	}

	rec, err := fs.GetFile(context.Background(), 7, "", "hello.txt")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if rec.Size != int64(len(content)) || rec.SHA256 != sha256Hex(content) || rec.Uploader != "uid-1" {
		t.Fatalf("store record = %+v", rec)
	}
}

func TestDownloadRejectsNonRegularBlob(t *testing.T) {
	rootDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootDir, "7", "directory.txt"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := New(Config{Addr: ":0", RootDir: rootDir}, newFakeFileStore(), nil)
	serverConn, clientConn := net.Pipe()
	defer func() { _ = serverConn.Close() }()
	defer func() { _ = clientConn.Close() }()

	err := s.sendDownload(serverConn, &transfer{ChannelID: 7, Name: "directory.txt"}, 0)
	if err == nil {
		t.Fatal("sendDownload accepted a directory as a blob")
	}
}

func TestDownloadRejectsSymlinkEscape(t *testing.T) {
	rootDir := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	channelDir := filepath.Join(rootDir, "7")
	if err := os.Mkdir(channelDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(channelDir, "link.txt")); err != nil {
		t.Skipf("symlink creation is not supported: %v", err)
	}
	s := New(Config{Addr: ":0", RootDir: rootDir}, newFakeFileStore(), nil)
	serverConn, clientConn := net.Pipe()
	defer func() { _ = serverConn.Close() }()
	defer func() { _ = clientConn.Close() }()

	err := s.sendDownload(serverConn, &transfer{ChannelID: 7, Name: "link.txt", Size: 6}, 0)
	if err == nil {
		t.Fatal("sendDownload followed a symlink outside the blob root")
	}
}

func TestUploadRejectsSymlinkEscape(t *testing.T) {
	rootDir := t.TempDir()
	outsideDir := t.TempDir()
	if err := os.Symlink(outsideDir, filepath.Join(rootDir, "7")); err != nil {
		t.Skipf("directory symlink creation is not supported: %v", err)
	}
	s := New(Config{Addr: ":0", RootDir: rootDir}, newFakeFileStore(), nil)

	err := s.receiveUpload(context.Background(), nil, &transfer{ChannelID: 7, Name: "escape.txt", Size: 1})
	if err == nil {
		t.Fatal("receiveUpload followed a directory symlink outside the blob root")
	}
	entries, err := os.ReadDir(outsideDir)
	if err != nil {
		t.Fatalf("read outside directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("upload created files outside the root: %v", entries)
	}
}

// TestUploadSizeMismatch verifies sending more bytes than declared aborts
// the transfer without leaving a file behind.
func TestUploadSizeMismatch(t *testing.T) {
	fs := newFakeFileStore()
	addr, s := startServer(t, fs)

	id, token, err := s.InitUpload(context.Background(), 7, "", "m.txt", 4, "uid-1", 0)
	if err != nil {
		t.Fatalf("InitUpload: %v", err)
	}

	conn := dialTransfer(t, addr, id, token)
	defer func() { _ = conn.Close() }()

	if err := netproto.WriteFrame(conn, &netproto.Frame{Type: frameChunk, Payload: []byte("hello")}); err != nil {
		t.Fatalf("write chunk: %v", err)
	}
	st := readStatus(t, conn)
	if st.OK {
		t.Fatal("size mismatch accepted")
	}

	if _, err := os.Stat(filepath.Join(s.cfg.RootDir, "7", "m.txt")); !os.IsNotExist(err) {
		t.Fatal("file left behind after abort")
	}
	if fs.addedCount() != 0 {
		t.Fatal("store record created after abort")
	}
}

// TestUploadChecksumMismatch verifies a wrong digest aborts the transfer.
func TestUploadChecksumMismatch(t *testing.T) {
	fs := newFakeFileStore()
	addr, s := startServer(t, fs)

	content := []byte("abcd")
	id, token, err := s.InitUpload(context.Background(), 7, "", "c.txt", 4, "uid-1", 0)
	if err != nil {
		t.Fatalf("InitUpload: %v", err)
	}

	conn := dialTransfer(t, addr, id, token)
	defer func() { _ = conn.Close() }()
	if err := netproto.WriteFrame(conn, &netproto.Frame{Type: frameChunk, Payload: content}); err != nil {
		t.Fatalf("write chunk: %v", err)
	}
	if err := writeJSON(conn, frameDigest, digestMsg{SHA256: sha256Hex([]byte("wxyz"))}); err != nil {
		t.Fatalf("write digest: %v", err)
	}

	st := readStatus(t, conn)
	if st.OK {
		t.Fatal("checksum mismatch accepted")
	}
	if _, err := os.Stat(filepath.Join(s.cfg.RootDir, "7", "c.txt")); !os.IsNotExist(err) {
		t.Fatal("file left behind after checksum failure")
	}
}

// TestDownloadRoundTrip downloads a file end-to-end.
func TestDownloadRoundTrip(t *testing.T) {
	fs := newFakeFileStore()
	addr, s := startServer(t, fs)

	content := []byte("download me, please")
	if err := os.MkdirAll(filepath.Join(s.cfg.RootDir, "7"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(s.cfg.RootDir, "7", "doc.txt"), content, 0o600); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	if err := fs.AddFile(context.Background(), store.FileRecord{
		ChannelID: 7, Name: "doc.txt", Size: int64(len(content)), SHA256: sha256Hex(content),
	}); err != nil {
		t.Fatalf("AddFile: %v", err)
	}

	id, token, err := s.InitDownload(context.Background(), 7, "", "doc.txt")
	if err != nil {
		t.Fatalf("InitDownload: %v", err)
	}

	conn := dialTransfer(t, addr, id, token)
	defer func() { _ = conn.Close() }()

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var got []byte
	var digest digestMsg
	for {
		f, err := netproto.ReadFrame(conn)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		switch f.Type {
		case frameChunk:
			got = append(got, f.Payload...)
		case frameDigest:
			if err := json.Unmarshal(f.Payload, &digest); err != nil {
				t.Fatalf("decode digest: %v", err)
			}
			goto done
		default:
			t.Fatalf("unexpected frame type %d", f.Type)
		}
	}
done:
	if string(got) != string(content) {
		t.Fatalf("download = %q, want %q", got, content)
	}
	if digest.SHA256 != sha256Hex(content) {
		t.Fatalf("digest = %q, want %q", digest.SHA256, sha256Hex(content))
	}

	st := readStatus(t, conn)
	if !st.OK {
		t.Fatalf("status = %+v, want ok", st)
	}
}

// TestDownloadResume verifies an offset download sends only the tail while
// the digest still covers the whole file (259), and that an offset past the
// end is refused.
func TestDownloadResume(t *testing.T) {
	fs := newFakeFileStore()
	addr, s := startServer(t, fs)

	content := []byte("resume me from the middle, please")
	const offset = 11
	if err := os.MkdirAll(filepath.Join(s.cfg.RootDir, "7"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(s.cfg.RootDir, "7", "part.txt"), content, 0o600); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	if err := fs.AddFile(context.Background(), store.FileRecord{
		ChannelID: 7, Name: "part.txt", Size: int64(len(content)), SHA256: sha256Hex(content),
	}); err != nil {
		t.Fatalf("AddFile: %v", err)
	}

	read := func(off int64) ([]byte, digestMsg, statusMsg) {
		t.Helper()
		id, token, err := s.InitDownload(context.Background(), 7, "", "part.txt")
		if err != nil {
			t.Fatalf("InitDownload: %v", err)
		}
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer func() { _ = conn.Close() }()
		if err := writeJSON(conn, frameInit, initMsg{Token: token, TransferID: id, Offset: off}); err != nil {
			t.Fatalf("write init: %v", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var got []byte
		var digest digestMsg
		for {
			f, err := netproto.ReadFrame(conn)
			if err != nil {
				t.Fatalf("read frame: %v", err)
			}
			if f.Type == frameStatus {
				var st statusMsg
				if err := json.Unmarshal(f.Payload, &st); err != nil {
					t.Fatalf("decode status: %v", err)
				}
				return got, digest, st
			}
			switch f.Type {
			case frameChunk:
				got = append(got, f.Payload...)
			case frameDigest:
				if err := json.Unmarshal(f.Payload, &digest); err != nil {
					t.Fatalf("decode digest: %v", err)
				}
			default:
				t.Fatalf("unexpected frame type %d", f.Type)
			}
		}
	}

	got, digest, st := read(offset)
	if !st.OK {
		t.Fatalf("status = %+v, want ok", st)
	}
	if string(got) != string(content[offset:]) {
		t.Fatalf("resumed body = %q, want %q", got, content[offset:])
	}
	// The prefix the client already holds is hashed but not re-sent, so the
	// digest must still be the whole file's.
	if digest.SHA256 != sha256Hex(content) {
		t.Fatalf("digest = %q, want whole-file %q", digest.SHA256, sha256Hex(content))
	}

	if _, _, st := read(int64(len(content)) + 1); st.OK {
		t.Fatal("offset past the end accepted")
	}
}

// TestDownloadMissing verifies downloading an unknown file fails at init.
func TestDownloadMissing(t *testing.T) {
	fs := newFakeFileStore()
	_, s := startServer(t, fs)
	if _, _, err := s.InitDownload(context.Background(), 7, "", "nope.txt"); err == nil {
		t.Fatal("InitDownload for missing file succeeded")
	}
}

// TestInvalidInitFrame verifies a connection not starting with an init frame
// is rejected.
func TestInvalidInitFrame(t *testing.T) {
	fs := newFakeFileStore()
	addr, _ := startServer(t, fs)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := netproto.WriteFrame(conn, &netproto.Frame{Type: frameChunk, Payload: []byte("x")}); err != nil {
		t.Fatalf("write: %v", err)
	}
	st := readStatus(t, conn)
	if st.OK {
		t.Fatal("non-init first frame accepted")
	}
}

// TestInvalidToken verifies a bad token is rejected.
func TestInvalidToken(t *testing.T) {
	fs := newFakeFileStore()
	addr, _ := startServer(t, fs)

	conn := dialTransfer(t, addr, "no-such-id", "no-such-token")
	defer func() { _ = conn.Close() }()
	st := readStatus(t, conn)
	if st.OK {
		t.Fatal("invalid token accepted")
	}
}

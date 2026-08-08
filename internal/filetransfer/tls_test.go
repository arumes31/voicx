// tls_test.go covers the encrypted data port (91-135): the listener refuses
// plaintext peers, transfers survive a fingerprint-pinned TLS dial, a wrong
// pin aborts, expiring links refuse client-encrypted chat attachments, and a
// ".vcx" name is flagged and must match the digest of the bytes it carries.
package filetransfer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"voicx/internal/netproto"
	"voicx/internal/tlscert"
)

// startTLSServer starts a TLS file-transfer server on an ephemeral port and
// returns its address, its certificate fingerprint, and the server.
func startTLSServer(t *testing.T, fs FileStore) (string, string, *Server) {
	t.Helper()
	cert, fp, err := tlscert.Ensure(t.TempDir(), "", "", nil)
	if err != nil {
		t.Fatalf("tlscert.Ensure: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	s := New(Config{
		Addr:        addr,
		RootDir:     t.TempDir(),
		TLSEnabled:  true,
		Cert:        cert,
		Fingerprint: fp,
	}, fs, nil)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		_ = s.Close()
		<-errCh
	})
	return addr, fp, s
}

// storageName mirrors the client's attachment naming rule: the first 32 hex
// characters of the ciphertext digest plus ".vcx" (91-135). The truncation is
// the part the server must not second-guess.
func storageName(content []byte) string {
	return sha256Hex(content)[:32] + encryptedSuffix
}

// pinnedTLSConfig dials with TOFU semantics: no CA chain, the presented
// certificate must match the fingerprint the control channel handed out.
func pinnedTLSConfig(wantFP string) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, // pinned below, same model as the control channel
		MinVersion:         tls.VersionTLS13,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no certificate presented")
			}
			if got := tlscert.FingerprintDER(raw[0]); got != wantFP {
				return fmt.Errorf("fingerprint mismatch: %s != %s", got, wantFP)
			}
			return nil
		},
	}
}

// dialTransferTLS connects over pinned TLS and sends the init frame.
func dialTransferTLS(t *testing.T, addr, fp, transferID, token string) net.Conn {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, pinnedTLSConfig(fp))
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	if err := writeJSON(conn, frameInit, initMsg{Token: token, TransferID: transferID}); err != nil {
		t.Fatalf("write init: %v", err)
	}
	return conn
}

// TestFileTransferRequiresTLS verifies a bare TCP peer cannot complete a
// transfer against the TLS listener: the token it holds is worthless without
// the handshake.
func TestFileTransferRequiresTLS(t *testing.T) {
	fs := newFakeFileStore()
	addr, _, s := startTLSServer(t, fs)

	content := []byte("plaintext peer")
	id, token, err := s.InitUpload(context.Background(), 7, "", "nope.txt", int64(len(content)), "uid-1", 0)
	if err != nil {
		t.Fatalf("InitUpload: %v", err)
	}

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	// The write may succeed (it is just bytes into a socket); the server sees
	// a failed handshake, never an init frame.
	_ = writeJSON(conn, frameInit, initMsg{Token: token, TransferID: id})

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if f, err := netproto.ReadFrame(conn); err == nil {
		t.Fatalf("plaintext peer got a frame (type %d) from the TLS listener", f.Type)
	}
	if fs.addedCount() != 0 {
		t.Fatal("plaintext peer stored a file")
	}
}

// TestFileTransferTLSRoundTrip verifies upload and download both work through
// a fingerprint-pinned TLS dial.
func TestFileTransferTLSRoundTrip(t *testing.T) {
	fs := newFakeFileStore()
	addr, fp, s := startTLSServer(t, fs)

	if s.Fingerprint() != fp {
		t.Fatalf("Fingerprint() = %q, want %q", s.Fingerprint(), fp)
	}

	content := []byte("sealed in transit")
	id, token, err := s.InitUpload(context.Background(), 7, "", "tls.txt", int64(len(content)), "uid-1", 0)
	if err != nil {
		t.Fatalf("InitUpload: %v", err)
	}
	up := dialTransferTLS(t, addr, fp, id, token)
	defer func() { _ = up.Close() }()
	if err := netproto.WriteFrame(up, &netproto.Frame{Type: frameChunk, Payload: content}); err != nil {
		t.Fatalf("write chunk: %v", err)
	}
	if err := writeJSON(up, frameDigest, digestMsg{SHA256: sha256Hex(content)}); err != nil {
		t.Fatalf("write digest: %v", err)
	}
	if st := readStatus(t, up); !st.OK {
		t.Fatalf("upload status = %+v, want ok", st)
	}

	onDisk, err := os.ReadFile(filepath.Join(s.cfg.RootDir, "7", "tls.txt"))
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if string(onDisk) != string(content) {
		t.Fatalf("file content = %q, want %q", onDisk, content)
	}

	id, token, err = s.InitDownload(context.Background(), 7, "", "tls.txt")
	if err != nil {
		t.Fatalf("InitDownload: %v", err)
	}
	down := dialTransferTLS(t, addr, fp, id, token)
	defer func() { _ = down.Close() }()

	_ = down.SetReadDeadline(time.Now().Add(3 * time.Second))
	var got []byte
	var digest digestMsg
	for digest.SHA256 == "" {
		f, err := netproto.ReadFrame(down)
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
		default:
			t.Fatalf("unexpected frame type %d", f.Type)
		}
	}
	if string(got) != string(content) {
		t.Fatalf("download = %q, want %q", got, content)
	}
	if digest.SHA256 != sha256Hex(content) {
		t.Fatalf("digest = %q, want %q", digest.SHA256, sha256Hex(content))
	}
	if st := readStatus(t, down); !st.OK {
		t.Fatalf("download status = %+v, want ok", st)
	}
}

// TestFileTransferRequiresTLS13 pins the data listener's documented minimum.
func TestFileTransferRequiresTLS13(t *testing.T) {
	fs := newFakeFileStore()
	addr, _, _ := startTLSServer(t, fs)

	conn, err := tls.Dial("tcp", addr, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // protocol-version test
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tls.VersionTLS12,
	})
	if err == nil {
		_ = conn.Close()
		t.Fatal("TLS 1.2 handshake succeeded, want rejection")
	}
}

// TestFingerprintMismatchRejected verifies a client pinning a different
// fingerprint aborts the handshake, so a hostile server cannot redirect
// transfers to a third-party host.
func TestFingerprintMismatchRejected(t *testing.T) {
	fs := newFakeFileStore()
	addr, fp, _ := startTLSServer(t, fs)

	_, otherFP, err := tlscert.Ensure(t.TempDir(), "", "", nil)
	if err != nil {
		t.Fatalf("tlscert.Ensure: %v", err)
	}
	if otherFP == fp {
		t.Fatal("two generated certificates share a fingerprint")
	}

	conn, err := tls.Dial("tcp", addr, pinnedTLSConfig(otherFP))
	if err == nil {
		_ = conn.Close()
		t.Fatal("handshake succeeded against a mismatched pin")
	}
}

// TestPlaintextPortStillWorks verifies file_tls_enabled=false keeps the
// dev-only plaintext path usable (the existing suite dials it directly).
func TestPlaintextPortStillWorks(t *testing.T) {
	fs := newFakeFileStore()
	addr, s := startServer(t, fs)
	if s.Fingerprint() != "" {
		t.Fatalf("Fingerprint() = %q on a plaintext port, want empty", s.Fingerprint())
	}
	uploadOne(t, addr, s, "", "plain.txt", []byte("dev only"))
}

// TestCreateLinkRefusesEncryptedAttachment verifies expiring links (267)
// refuse .vcx blobs: /dl/<token> has no key and would serve ciphertext that
// looks like a corrupt download.
func TestCreateLinkRefusesEncryptedAttachment(t *testing.T) {
	fs := newFakeFileStore()
	addr, s := startServer(t, fs)

	content := []byte("sealed bytes")
	name := storageName(content)
	uploadOne(t, addr, s, "", name, content)

	if _, _, err := s.CreateLink(context.Background(), 7, "", name); !errors.Is(err, ErrEncryptedAttachment) {
		t.Fatalf("CreateLink(%s) = %v, want ErrEncryptedAttachment", name, err)
	}
	// A plain file in the same channel is unaffected.
	uploadOne(t, addr, s, "", "notes.txt", []byte("readable"))
	if _, _, err := s.CreateLink(context.Background(), 7, "", "notes.txt"); err != nil {
		t.Fatalf("CreateLink for a plain file: %v", err)
	}
}

// TestEncryptedAttachmentFlagged verifies a .vcx upload is recorded as
// encrypted so the file browser can refuse to hand out a blob it has no key
// for, while ordinary uploads are not.
func TestEncryptedAttachmentFlagged(t *testing.T) {
	fs := newFakeFileStore()
	addr, s := startServer(t, fs)

	content := []byte("ciphertext")
	sealed := storageName(content)
	uploadOne(t, addr, s, "", sealed, content)
	uploadOne(t, addr, s, "", "readme.md", []byte("plain"))

	rec, err := fs.GetFile(context.Background(), 7, "", sealed)
	if err != nil {
		t.Fatalf("GetFile(%s): %v", sealed, err)
	}
	if !rec.Encrypted {
		t.Fatal("chat attachment not flagged encrypted")
	}
	rec, err = fs.GetFile(context.Background(), 7, "", "readme.md")
	if err != nil {
		t.Fatalf("GetFile(readme.md): %v", err)
	}
	if rec.Encrypted {
		t.Fatal("ordinary upload flagged encrypted")
	}
}

// TestEncryptedAttachmentNameMustMatchDigest guards the contract in both
// directions: the truncated content-derived name a conformant client sends is
// stored, and a .vcx name unrelated to the bytes is refused. Accepting the
// latter would let an upload displace the blob an older message points at,
// since .vcx names deliberately skip version rotation.
func TestEncryptedAttachmentNameMustMatchDigest(t *testing.T) {
	fs := newFakeFileStore()
	addr, s := startServer(t, fs)

	content := []byte("ciphertext")
	good := storageName(content)
	uploadOne(t, addr, s, "", good, content)
	rec, err := fs.GetFile(context.Background(), 7, "", good)
	if err != nil {
		t.Fatalf("GetFile(%s): %v", good, err)
	}
	if !rec.Encrypted {
		t.Fatalf("%s not flagged encrypted", good)
	}

	const forged = "holiday-photos.vcx"
	id, token, err := s.InitUpload(context.Background(), 7, "", forged, int64(len(content)), "uid-1", 0)
	if err != nil {
		t.Fatalf("InitUpload %s: %v", forged, err)
	}
	conn := dialTransfer(t, addr, id, token)
	defer func() { _ = conn.Close() }()
	if err := netproto.WriteFrame(conn, &netproto.Frame{Type: frameChunk, Payload: content}); err != nil {
		t.Fatalf("write chunk: %v", err)
	}
	if err := writeJSON(conn, frameDigest, digestMsg{SHA256: sha256Hex(content)}); err != nil {
		t.Fatalf("write digest: %v", err)
	}
	if st := readStatus(t, conn); st.OK {
		t.Fatal("forged .vcx name accepted")
	}
	if _, err := fs.GetFile(context.Background(), 7, "", forged); err == nil {
		t.Fatal("forged .vcx name was stored")
	}
}

// TestEncryptedAttachmentSkipsVersionRotation verifies .vcx uploads never
// enter the .v1..v3 path (264): content-derived names cannot collide, so
// rotation would only be a guaranteed store miss.
func TestEncryptedAttachmentSkipsVersionRotation(t *testing.T) {
	fs := newFakeFileStore()
	addr, s := startServer(t, fs)

	first := []byte("sealed one")
	name := storageName(first)
	uploadOne(t, addr, s, "", name, first)
	// The name is the digest, so a same-name upload is by definition the same
	// bytes: re-sending the identical blob must refresh the row, not version it.
	uploadOne(t, addr, s, "", name, first)

	if _, err := fs.GetFile(context.Background(), 7, "", name+".v1"); err == nil {
		t.Fatal("chat attachment rotated into .v1")
	}
	// The plain path still rotates (264 is unchanged).
	uploadOne(t, addr, s, "", "notes.txt", []byte("one"))
	uploadOne(t, addr, s, "", "notes.txt", []byte("two"))
	if _, err := fs.GetFile(context.Background(), 7, "", "notes.txt.v1"); err != nil {
		t.Fatalf("plain upload did not rotate: %v", err)
	}
}

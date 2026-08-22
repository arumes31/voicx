package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"voicx/internal/netproto"
)

type capturedAttachmentUpload struct {
	data []byte
	err  error
}

// captureAttachmentUpload is a minimal file-transfer server used to exercise
// the production upload path before the same encrypted bytes are downloaded.
func captureAttachmentUpload(conn net.Conn) <-chan capturedAttachmentUpload {
	result := make(chan capturedAttachmentUpload, 1)
	go func() {
		defer close(result)
		defer func() { _ = conn.Close() }()

		init, err := netproto.ReadFrame(conn)
		if err != nil || init.Type != ftInit {
			result <- capturedAttachmentUpload{err: errors.New("invalid upload init")}
			return
		}
		var data bytes.Buffer
		for {
			frame, err := netproto.ReadFrame(conn)
			if err != nil {
				result <- capturedAttachmentUpload{err: err}
				return
			}
			switch frame.Type {
			case ftChunk:
				_, _ = data.Write(frame.Payload)
			case ftDigest:
				var digest struct {
					SHA256 string `json:"sha256"`
				}
				if err := json.Unmarshal(frame.Payload, &digest); err != nil {
					result <- capturedAttachmentUpload{err: err}
					return
				}
				sum := sha256.Sum256(data.Bytes())
				if digest.SHA256 != hex.EncodeToString(sum[:]) {
					result <- capturedAttachmentUpload{err: errFileDigestMismatch}
					return
				}
				if err := ftWriteFrame(conn, &netproto.Frame{Type: ftStatus, Payload: []byte(`{"ok":true}`)}); err != nil {
					result <- capturedAttachmentUpload{err: err}
					return
				}
				result <- capturedAttachmentUpload{data: data.Bytes()}
				return
			default:
				result <- capturedAttachmentUpload{err: errors.New("unexpected upload frame")}
				return
			}
		}
	}()
	return result
}

func transferChunks(data []byte) [][]byte {
	chunks := make([][]byte, 0, (len(data)+netproto.MaxPayloadSize-1)/netproto.MaxPayloadSize)
	for len(data) > 0 {
		n := min(len(data), netproto.MaxPayloadSize)
		chunks = append(chunks, data[:n])
		data = data[n:]
	}
	return chunks
}

func attachmentSaveApp(path string, blob []byte) (*App, *int) {
	a := appWithCM(newConnManager(context.Background()))
	fetches := 0
	a.chatAttachmentSaveDialog = func(context.Context, wailsRuntime.SaveDialogOptions) (string, error) {
		return path, nil
	}
	a.chatAttachmentFetch = func(*connManager, int64, string) ([]byte, error) {
		fetches++
		return append([]byte(nil), blob...), nil
	}
	return a, &fetches
}

func TestSaveChatAttachment(t *testing.T) {
	key := randKey(t)
	plain := []byte("attachment plaintext")
	blob, err := sealFile(plain, key)
	if err != nil {
		t.Fatal(err)
	}
	keyB64 := base64.StdEncoding.EncodeToString(key[:])
	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 0xff

	t.Run("cancel does not fetch and sanitizes the dialog filename", func(t *testing.T) {
		a, fetches := attachmentSaveApp("", blob)
		var options wailsRuntime.SaveDialogOptions
		a.chatAttachmentSaveDialog = func(_ context.Context, got wailsRuntime.SaveDialogOptions) (string, error) {
			options = got
			return "", nil
		}
		path, err := a.SaveChatAttachment(7, "blob.vcx", keyB64, "../unsafe\\photo\x00.png")
		if err != nil || path != "" {
			t.Fatalf("cancel = %q, %v", path, err)
		}
		if *fetches != 0 {
			t.Fatalf("fetches after cancel = %d, want 0", *fetches)
		}
		if options.DefaultFilename != "photo.png" {
			t.Fatalf("dialog filename = %q", options.DefaultFilename)
		}
	})

	t.Run("writes a verified new file without returning plaintext to JavaScript", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "saved.txt")
		a, fetches := attachmentSaveApp(dest, blob)
		path, err := a.SaveChatAttachment(7, "blob.vcx", keyB64, "saved.txt")
		if err != nil || path != dest {
			t.Fatalf("save = %q, %v", path, err)
		}
		got, err := os.ReadFile(dest)
		if err != nil || string(got) != string(plain) {
			t.Fatalf("saved contents = %q, %v", got, err)
		}
		if *fetches != 1 {
			t.Fatalf("fetches = %d, want 1", *fetches)
		}
	})

	t.Run("preexisting destination is untouched before download", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "existing.txt")
		if err := os.WriteFile(dest, []byte("keep me"), 0o600); err != nil {
			t.Fatal(err)
		}
		a, fetches := attachmentSaveApp(dest, blob)
		if _, err := a.SaveChatAttachment(7, "blob.vcx", keyB64, "existing.txt"); !errors.Is(err, errChatAttachmentDestinationExists) {
			t.Fatalf("save error = %v, want destination conflict", err)
		}
		got, err := os.ReadFile(dest)
		if err != nil || string(got) != "keep me" {
			t.Fatalf("existing contents = %q, %v", got, err)
		}
		if *fetches != 0 {
			t.Fatalf("fetches after existing destination = %d, want 0", *fetches)
		}
	})

	t.Run("late destination conflict is untouched", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "raced.txt")
		a, fetches := attachmentSaveApp(dest, blob)
		originalLink := chatAttachmentLink
		chatAttachmentLink = func(oldname, newname string) error {
			if err := os.WriteFile(newname, []byte("racer"), 0o600); err != nil {
				return err
			}
			return os.Link(oldname, newname)
		}
		t.Cleanup(func() { chatAttachmentLink = originalLink })
		if _, err := a.SaveChatAttachment(7, "blob.vcx", keyB64, "raced.txt"); !errors.Is(err, errChatAttachmentDestinationExists) {
			t.Fatalf("save error = %v, want destination conflict", err)
		}
		got, err := os.ReadFile(dest)
		if err != nil || string(got) != "racer" {
			t.Fatalf("late destination contents = %q, %v", got, err)
		}
		if *fetches != 1 {
			t.Fatalf("fetches before late conflict = %d, want 1", *fetches)
		}
	})

	for _, test := range []struct {
		name string
		blob []byte
		key  string
		want error
	}{
		{name: "download failure", blob: nil, key: keyB64, want: errors.New("download failed")},
		{name: "tampered ciphertext", blob: tampered, key: keyB64},
		{name: "over cap", blob: make([]byte, maxChatAttachmentBytes+1), key: ""},
	} {
		t.Run(test.name+" preserves an absent destination", func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "failed.txt")
			a, _ := attachmentSaveApp(dest, test.blob)
			if test.want != nil {
				a.chatAttachmentFetch = func(*connManager, int64, string) ([]byte, error) { return nil, test.want }
			}
			if _, err := a.SaveChatAttachment(7, "blob.vcx", test.key, "failed.txt"); err == nil {
				t.Fatal("save unexpectedly succeeded")
			}
			if _, err := os.Lstat(dest); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("destination after failure = %v, want absent", err)
			}
		})
	}

	t.Run("write failure leaves destination absent", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "write-failed.txt")
		a, _ := attachmentSaveApp(dest, blob)
		a.chatAttachmentWrite = func(string, []byte) error { return errors.New("write failed") }
		if _, err := a.SaveChatAttachment(7, "blob.vcx", keyB64, "write-failed.txt"); err == nil {
			t.Fatal("save unexpectedly succeeded")
		}
		if _, err := os.Lstat(dest); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("destination after write failure = %v, want absent", err)
		}
	})

	t.Run("inline preview refuses base64 over its renderer bound", func(t *testing.T) {
		a, _ := attachmentSaveApp(filepath.Join(t.TempDir(), "unused.txt"), make([]byte, maxInlineAttachmentBase64Bytes))
		if _, err := a.DownloadChatAttachment(7, "legacy.bin", ""); !errors.Is(err, errChatAttachmentPreviewTooLarge) {
			t.Fatalf("preview error = %v, want renderer bound", err)
		}
	})
}

func TestSealedChatAttachmentMaximumRoundTrip(t *testing.T) {
	plain := make([]byte, maxChatAttachmentBytes)
	for i := range plain {
		plain[i] = byte(i)
	}

	controlClient, controlServer := net.Pipe()
	wrappedControl := fixedRemoteAddrConn{Conn: controlClient, remote: fixedTransferAddr("127.0.0.1:12333")}
	cm := newConnManager(context.Background())
	cm.sink = &eventRecorder{}
	cm.mu.Lock()
	cm.conn = wrappedControl
	cm.acceptingTransfers = true
	cm.transferEpoch = 1
	cm.mu.Unlock()
	a := appWithCM(cm)
	requests := make(chan netproto.FileTransferInit, 2)
	go serveFrames(controlServer, func(frame *netproto.Frame) (netproto.MessageType, any, bool) {
		if netproto.MessageType(frame.Type) != netproto.MsgFileTransferInit {
			return 0, nil, false
		}
		var request netproto.FileTransferInit
		if err := json.Unmarshal(frame.Payload, &request); err != nil {
			return 0, nil, false
		}
		requests <- request
		return netproto.MsgFileTransferInitResponse, netproto.FileTransferInitResponse{
			Token: request.Direction, TransferID: request.Direction, Port: 12334,
		}, true
	})
	go cm.readLoop(wrappedControl)
	t.Cleanup(func() {
		cm.disconnect()
		_ = controlServer.Close()
	})

	uploadClient, uploadServer := net.Pipe()
	downloadClient, downloadServer := net.Pipe()
	dials := make(chan net.Conn, 2)
	dials <- uploadClient
	dials <- downloadClient
	originalDial := transferDial
	transferDial = func(ftEndpoint) (net.Conn, error) { return <-dials, nil }
	t.Cleanup(func() { transferDial = originalDial })

	uploaded := captureAttachmentUpload(uploadServer)
	token, err := a.UploadChatAttachment(7, "maximum.bin", base64.StdEncoding.EncodeToString(plain))
	if err != nil {
		t.Fatalf("upload maximum attachment: %v", err)
	}
	if request := <-requests; request.Direction != "upload" || request.Size != int64(maxSealedChatAttachmentBytes) {
		t.Fatalf("upload request = %#v, want sealed size %d", request, maxSealedChatAttachmentBytes)
	}
	upload := <-uploaded
	if upload.err != nil {
		t.Fatalf("capture upload: %v", upload.err)
	}
	if len(upload.data) != maxSealedChatAttachmentBytes {
		t.Fatalf("sealed size = %d, want %d", len(upload.data), maxSealedChatAttachmentBytes)
	}

	body := strings.TrimSuffix(strings.TrimPrefix(token, "[file:"), "]")
	storage, keyB64, _ := parseFileRef(body)
	if storage == "" || keyB64 == "" {
		t.Fatalf("upload token = %q", token)
	}
	chunks := transferChunks(upload.data)
	_, downloadDone := serveTransferDownload(
		t,
		downloadServer,
		chunks,
		transferDigest(chunks...),
		&netproto.Frame{Type: ftStatus, Payload: []byte(`{"ok":true}`)},
	)
	dest := filepath.Join(t.TempDir(), "maximum.bin")
	a.chatAttachmentSaveDialog = func(context.Context, wailsRuntime.SaveDialogOptions) (string, error) {
		return dest, nil
	}
	saved, err := a.SaveChatAttachment(7, storage, keyB64, "maximum.bin")
	if err != nil || saved != dest {
		t.Fatalf("save maximum attachment = %q, %v", saved, err)
	}
	if request := <-requests; request.Direction != "download" {
		t.Fatalf("download request = %#v", request)
	}
	<-downloadDone
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("saved maximum contents = %d bytes, %v", len(got), err)
	}
}

func TestChatAttachmentRejectsOversizeAndMalformedSealedData(t *testing.T) {
	t.Run("25 MiB plus one plaintext byte is rejected before upload", func(t *testing.T) {
		a := appWithCM(newConnManager(context.Background()))
		data := make([]byte, maxChatAttachmentBytes+1)
		if _, err := a.UploadChatAttachment(7, "too-large.bin", base64.StdEncoding.EncodeToString(data)); err == nil {
			t.Fatal("oversize upload unexpectedly succeeded")
		}
	})

	t.Run("extra GCM framing is rejected even below the derived wire cap", func(t *testing.T) {
		key := randKey(t)
		blob, err := sealFile([]byte("valid"), key)
		if err != nil {
			t.Fatal(err)
		}
		blob = append(blob, make([]byte, attachmentChunkLengthBytes)...)
		dest := filepath.Join(t.TempDir(), "malformed.bin")
		a, _ := attachmentSaveApp(dest, blob)
		_, err = a.SaveChatAttachment(7, "malformed.vcx", base64.StdEncoding.EncodeToString(key[:]), "malformed.bin")
		if err == nil {
			t.Fatal("malformed GCM framing unexpectedly saved")
		}
		if _, err := os.Lstat(dest); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("destination after malformed framing = %v, want absent", err)
		}
	})
}

func TestSafeAttachmentFilename(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"report.txt", "report.txt"},
		{"../report.txt", "report.txt"},
		{"C:\\Users\\me\\report.txt", "report.txt"},
		{"\x00\x1f", "attachment"},
		{"..", "attachment"},
	} {
		if got := safeAttachmentFilename(test.input); got != test.want {
			t.Fatalf("safeAttachmentFilename(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"

	"voicx/internal/netproto"
)

type transferRecordingWriter struct {
	chunks [][]byte
	err    error
	short  bool
}

func (w *transferRecordingWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	w.chunks = append(w.chunks, append([]byte(nil), p...))
	if w.short && len(p) > 0 {
		return len(p) - 1, nil
	}
	return len(p), nil
}

type transferCountingWriter struct{ n int64 }

func (w *transferCountingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

func transferDigest(chunks ...[]byte) string {
	h := sha256.New()
	for _, chunk := range chunks {
		_, _ = h.Write(chunk)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func serveTransferDownload(t *testing.T, conn net.Conn, chunks [][]byte, digest string, status *netproto.Frame) (<-chan []byte, <-chan struct{}) {
	t.Helper()
	init := make(chan []byte, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = conn.Close() }()
		frame, err := netproto.ReadFrame(conn)
		if err != nil {
			return
		}
		init <- append([]byte(nil), frame.Payload...)
		for _, chunk := range chunks {
			if err := netproto.WriteFrame(conn, &netproto.Frame{Type: ftChunk, Payload: chunk}); err != nil {
				return
			}
		}
		if digest != "" {
			payload, _ := json.Marshal(map[string]string{"sha256": digest})
			if err := netproto.WriteFrame(conn, &netproto.Frame{Type: ftDigest, Payload: payload}); err != nil {
				return
			}
		}
		if status != nil {
			_ = netproto.WriteFrame(conn, status)
		}
	}()
	return init, done
}

func TestDownloadStreamFailureMatrix(t *testing.T) {
	writerErr := errors.New("writer failed")
	for _, test := range []struct {
		name       string
		chunks     [][]byte
		digest     string
		status     *netproto.Frame
		maxBytes   int64
		writer     *transferRecordingWriter
		wantErrIs  error
		wantWrites int
	}{
		{
			name:       "overflow is rejected before the writer sees it",
			chunks:     [][]byte{[]byte("12345"), []byte("6")},
			maxBytes:   5,
			writer:     &transferRecordingWriter{},
			wantWrites: 1,
		},
		{
			name:       "bad digest",
			chunks:     [][]byte{[]byte("data")},
			digest:     transferDigest([]byte("other")),
			writer:     &transferRecordingWriter{},
			wantErrIs:  errFileDigestMismatch,
			wantWrites: 1,
		},
		{
			name:       "missing status",
			chunks:     [][]byte{[]byte("data")},
			digest:     transferDigest([]byte("data")),
			writer:     &transferRecordingWriter{},
			wantWrites: 1,
		},
		{
			name:       "wrong status frame",
			chunks:     [][]byte{[]byte("data")},
			digest:     transferDigest([]byte("data")),
			status:     &netproto.Frame{Type: ftChunk, Payload: []byte("not-status")},
			writer:     &transferRecordingWriter{},
			wantWrites: 1,
		},
		{
			name:       "failed status",
			chunks:     [][]byte{[]byte("data")},
			digest:     transferDigest([]byte("data")),
			status:     &netproto.Frame{Type: ftStatus, Payload: []byte(`{"ok":false,"error":"denied"}`)},
			writer:     &transferRecordingWriter{},
			wantWrites: 1,
		},
		{
			name:       "short writer",
			chunks:     [][]byte{[]byte("data")},
			writer:     &transferRecordingWriter{short: true},
			wantErrIs:  io.ErrShortWrite,
			wantWrites: 1,
		},
		{
			name:       "writer error",
			chunks:     [][]byte{[]byte("data")},
			writer:     &transferRecordingWriter{err: writerErr},
			wantErrIs:  writerErr,
			wantWrites: 0,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer func() { _ = client.Close() }()
			_, serverDone := serveTransferDownload(t, server, test.chunks, test.digest, test.status)
			_, err := ftDownloadTo(client, "token", "transfer", test.writer, test.maxBytes)
			<-serverDone
			if err == nil {
				t.Fatal("download unexpectedly succeeded")
			}
			if test.wantErrIs != nil && !errors.Is(err, test.wantErrIs) {
				t.Fatalf("download error = %v, want %v", err, test.wantErrIs)
			}
			if got := len(test.writer.chunks); got != test.wantWrites {
				t.Fatalf("writer calls = %d, want %d", got, test.wantWrites)
			}
		})
	}
}

func TestDownloadStreamResumesWithSeededHasherAndOffset(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	prefix, suffix := []byte("old"), []byte("new")
	init, serverDone := serveTransferDownload(
		t,
		server,
		[][]byte{suffix},
		transferDigest(prefix, suffix),
		&netproto.Frame{Type: ftStatus, Payload: []byte(`{"ok":true}`)},
	)
	h := sha256.New()
	_, _ = h.Write(prefix)
	writer := &transferRecordingWriter{}
	total, err := ftDownloadStream(client, map[string]any{
		"token": "token", "transfer_id": "transfer", "offset": int64(len(prefix)),
	}, writer, 0, int64(len(prefix)), h, nil)
	if err != nil || total != int64(len(prefix)+len(suffix)) {
		t.Fatalf("resumed download = total:%d err:%v", total, err)
	}
	var request map[string]any
	if err := json.Unmarshal(<-init, &request); err != nil {
		t.Fatalf("decode resume init: %v", err)
	}
	if got := request["offset"]; got != float64(len(prefix)) {
		t.Fatalf("resume offset = %#v, want %d", got, len(prefix))
	}
	if len(writer.chunks) != 1 || string(writer.chunks[0]) != string(suffix) {
		t.Fatalf("resume writer data = %q", writer.chunks)
	}
	<-serverDone
}

func TestDownloadStreamAcceptsExactLegacyByteLimit(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	chunk := make([]byte, netproto.MaxPayloadSize)
	for i := range chunk {
		chunk[i] = 'x'
	}
	chunks := make([][]byte, 0, 26)
	var total int64
	for total < maxLegacyTransferBytes {
		remaining := maxLegacyTransferBytes - total
		size := int64(len(chunk))
		if remaining < size {
			size = remaining
		}
		chunks = append(chunks, chunk[:int(size)])
		total += size
	}
	if total != maxLegacyTransferBytes {
		t.Fatalf("test chunks total = %d, want exact legacy limit %d", total, maxLegacyTransferBytes)
	}
	init, serverDone := serveTransferDownload(
		t,
		server,
		chunks,
		transferDigest(chunks...),
		&netproto.Frame{Type: ftStatus, Payload: []byte(`{"ok":true}`)},
	)
	writer := &transferCountingWriter{}
	got, err := ftDownloadTo(client, "token", "transfer", writer, maxLegacyTransferBytes)
	if err != nil || got != maxLegacyTransferBytes || writer.n != maxLegacyTransferBytes {
		t.Fatalf("exact-limit download = total:%d writer:%d err:%v", got, writer.n, err)
	}
	<-init
	<-serverDone
}

func TestDownloadStreamRejectsSealedWireBytesBeforeExcessWrite(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	chunk := make([]byte, netproto.MaxPayloadSize)
	chunks := make([][]byte, 0, maxSealedChatAttachmentBytes/int(netproto.MaxPayloadSize)+2)
	remaining := maxSealedChatAttachmentBytes
	for remaining > 0 {
		size := min(remaining, len(chunk))
		chunks = append(chunks, chunk[:size])
		remaining -= size
	}
	chunks = append(chunks, []byte("x"))
	_, serverDone := serveTransferDownload(t, server, chunks, "", nil)
	writer := &transferCountingWriter{}
	got, err := ftDownloadTo(client, "token", "transfer", writer, int64(maxSealedChatAttachmentBytes))
	if err == nil {
		t.Fatal("over-wire download unexpectedly succeeded")
	}
	if got != int64(maxSealedChatAttachmentBytes) || writer.n != int64(maxSealedChatAttachmentBytes) {
		t.Fatalf("over-wire download wrote total:%d writer:%d, want cap %d", got, writer.n, maxSealedChatAttachmentBytes)
	}
	<-serverDone
}

type fixedTransferAddr string

func (a fixedTransferAddr) Network() string { return "tcp" }
func (a fixedTransferAddr) String() string  { return string(a) }

type fixedRemoteAddrConn struct {
	net.Conn
	remote net.Addr
}

func (c fixedRemoteAddrConn) RemoteAddr() net.Addr { return c.remote }

func TestVerifyFileStreamsBeyondLegacyBufferLimit(t *testing.T) {
	controlClient, controlServer := net.Pipe()
	wrappedControl := fixedRemoteAddrConn{Conn: controlClient, remote: fixedTransferAddr("127.0.0.1:12333")}
	cm := newConnManager(context.Background())
	cm.sink = &eventRecorder{}
	cm.mu.Lock()
	cm.conn = wrappedControl
	cm.acceptingTransfers = true
	cm.transferEpoch = 1
	cm.mu.Unlock()
	app := appWithCM(cm)
	go serveFrames(controlServer, func(frame *netproto.Frame) (netproto.MessageType, any, bool) {
		if netproto.MessageType(frame.Type) != netproto.MsgFileTransferInit {
			return 0, nil, false
		}
		return netproto.MsgFileTransferInitResponse, netproto.FileTransferInitResponse{
			Token: "token", TransferID: "transfer", Port: 12334,
		}, true
	})
	go cm.readLoop(wrappedControl)
	t.Cleanup(func() {
		cm.disconnect()
		_ = controlServer.Close()
	})

	dataClient, dataServer := net.Pipe()
	originalDial := transferDial
	transferDial = func(ftEndpoint) (net.Conn, error) { return dataClient, nil }
	t.Cleanup(func() { transferDial = originalDial })
	chunk := make([]byte, netproto.MaxPayloadSize)
	for i := range chunk {
		chunk[i] = 'v'
	}
	count := int(maxLegacyTransferBytes/int64(len(chunk))) + 1
	chunks := make([][]byte, count)
	for i := range chunks {
		chunks[i] = chunk
	}
	_, serverDone := serveTransferDownload(
		t,
		dataServer,
		chunks,
		transferDigest(chunks...),
		&netproto.Frame{Type: ftStatus, Payload: []byte(`{"ok":true}`)},
	)
	ok, err := app.VerifyFile(1, "", "large.bin", transferDigest(chunks...))
	if err != nil || !ok {
		t.Fatalf("streaming verify = %v, %v", ok, err)
	}
	<-serverDone
}

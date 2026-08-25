package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"voicx/internal/netproto"
)

type deadlineSpyConn struct {
	net.Conn
	mu             sync.Mutex
	readDeadlines  []time.Time
	writeDeadlines []time.Time
}

func (c *deadlineSpyConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadlines = append(c.readDeadlines, t)
	c.mu.Unlock()
	return c.Conn.SetReadDeadline(t)
}

func (c *deadlineSpyConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.writeDeadlines = append(c.writeDeadlines, t)
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(t)
}

func (c *deadlineSpyConn) deadlines() ([]time.Time, []time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.readDeadlines...), append([]time.Time(nil), c.writeDeadlines...)
}

func requireActiveThenClearedDeadline(t *testing.T, deadlines []time.Time, kind string) {
	t.Helper()
	if len(deadlines) < 2 {
		t.Fatalf("%s deadlines = %v, want rolling deadline then clear", kind, deadlines)
	}
	if deadlines[0].IsZero() {
		t.Fatalf("first %s deadline was not armed", kind)
	}
	if !deadlines[len(deadlines)-1].IsZero() {
		t.Fatalf("last %s deadline = %v, want clear", kind, deadlines[len(deadlines)-1])
	}
}

func TestFileTransferStallTimesOutAndClearsDeadlines(t *testing.T) {
	originalTimeout := fileTransferIdleTimeout
	fileTransferIdleTimeout = 15 * time.Millisecond
	t.Cleanup(func() { fileTransferIdleTimeout = originalTimeout })
	client, server := net.Pipe()
	defer func() { _ = server.Close() }()
	spy := &deadlineSpyConn{Conn: client}
	serverRead := make(chan struct{})
	go func() {
		_, _ = netproto.ReadFrame(server) // init, then intentionally stall
		close(serverRead)
	}()
	_, err := ftDownloadTo(spy, "token", "id", ioDiscard{}, 0)
	<-serverRead
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("stalled transfer error = %v, want timeout", err)
	}
	reads, writes := spy.deadlines()
	requireActiveThenClearedDeadline(t, reads, "read")
	requireActiveThenClearedDeadline(t, writes, "write")
}

func TestFileTransferSlowActiveFramesRefreshIdleDeadline(t *testing.T) {
	originalTimeout := fileTransferIdleTimeout
	fileTransferIdleTimeout = 30 * time.Millisecond
	t.Cleanup(func() { fileTransferIdleTimeout = originalTimeout })
	client, server := net.Pipe()
	defer func() { _ = server.Close() }()
	spy := &deadlineSpyConn{Conn: client}
	go func() {
		_, _ = netproto.ReadFrame(server)
		for _, chunk := range [][]byte{[]byte("a"), []byte("b"), []byte("c")} {
			time.Sleep(10 * time.Millisecond)
			_ = netproto.WriteFrame(server, &netproto.Frame{Type: ftChunk, Payload: chunk})
		}
		sum := sha256.Sum256([]byte("abc"))
		_ = netproto.WriteFrame(server, &netproto.Frame{Type: ftDigest, Payload: []byte(`{"sha256":"` + hex.EncodeToString(sum[:]) + `"}`)})
		_ = netproto.WriteFrame(server, &netproto.Frame{Type: ftStatus, Payload: []byte(`{"ok":true}`)})
	}()
	var out discardBuffer
	n, err := ftDownloadTo(spy, "token", "id", &out, 0)
	if err != nil || n != 3 || out.n != 3 {
		t.Fatalf("slow active transfer = n:%d out:%d err:%v", n, out.n, err)
	}
	reads, writes := spy.deadlines()
	if len(reads) < 5 { // three chunks, digest, status
		t.Fatalf("read deadlines = %d, want each active frame to refresh", len(reads))
	}
	requireActiveThenClearedDeadline(t, reads, "read")
	requireActiveThenClearedDeadline(t, writes, "write")
}

func TestFileTransferWriteFramesArmAndClearDeadline(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = server.Close() }()
	spy := &deadlineSpyConn{Conn: client}
	go func() {
		for range 3 { // init, one chunk, digest
			_, _ = netproto.ReadFrame(server)
		}
		_ = netproto.WriteFrame(server, &netproto.Frame{Type: ftStatus, Payload: []byte(`{"ok":true}`)})
	}()
	if err := ftUploadConn(spy, "token", "id", []byte("x")); err != nil {
		t.Fatalf("upload: %v", err)
	}
	reads, writes := spy.deadlines()
	requireActiveThenClearedDeadline(t, writes, "write")
	requireActiveThenClearedDeadline(t, reads, "read")
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

type discardBuffer struct{ n int64 }

func (b *discardBuffer) Write(p []byte) (int, error) {
	b.n += int64(len(p))
	return len(p), nil
}

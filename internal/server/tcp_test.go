package server

import (
	"context"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"

	"voicx/internal/config"
	"voicx/internal/netproto"
)

// testLogger returns a discard logger suitable for tests.
func testLogger() *zap.Logger {
	logger, _ := zap.NewDevelopment()
	return logger
}

// freePort returns a string ":0"-style address that the OS will resolve to an
// ephemeral free port. We use ":0" directly because net.Listen supports it.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// TestNewTCPServer verifies that New returns a non-nil TCPServer without
// starting it.
func TestNewTCPServer(t *testing.T) {
	cfg := &config.Config{TCPAddr: ":0"}
	s := New(cfg, testLogger(), nil)
	if s == nil {
		t.Fatal("New returned nil TCPServer")
	}
}

// TestTCPServerStartShutdownPingPong exercises the full lifecycle: start the
// server, connect a TCP client, send a Ping frame, expect a Pong frame back,
// then cancel the context and assert Shutdown returns cleanly.
func TestTCPServerStartShutdownPingPong(t *testing.T) {
	addr := freePort(t)
	cfg := &config.Config{TCPAddr: addr}
	s := New(cfg, testLogger(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startErr := make(chan error, 1)
	go func() {
		startErr <- s.Start(ctx)
	}()

	// Wait until the server is accepting connections by retrying dial.
	var conn net.Conn
	var dialErr error
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr = net.Dial("tcp", addr)
		if dialErr == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if dialErr != nil {
		t.Fatalf("dial tcp server: %v", dialErr)
	}
	defer func() { _ = conn.Close() }()

	// Send a Ping frame.
	pingFrame, err := netproto.Encode(netproto.MsgPing, netproto.Ping{})
	if err != nil {
		t.Fatalf("encode ping: %v", err)
	}
	if err := netproto.WriteFrame(conn, pingFrame); err != nil {
		t.Fatalf("write ping: %v", err)
	}

	// Expect a Pong frame back.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := netproto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read pong: %v", err)
	}
	if netproto.MessageType(resp.Type) != netproto.MsgPong {
		t.Fatalf("response type = %s, want Pong", netproto.MessageType(resp.Type))
	}

	// Cancel context and assert Shutdown returns cleanly.
	cancel()
	if err := <-startErr; err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if err := s.Shutdown(); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}
}

package server

import (
	"context"
	"net"
	"testing"
	"time"

	"voicx/internal/config"
	"voicx/internal/netproto"
)

// freeUDPPort returns a UDP address string bound to an ephemeral free port
// on the loopback interface.
func freeUDPPort(t *testing.T) string {
	t.Helper()
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("freeUDPPort: %v", err)
	}
	addr := ln.LocalAddr().String()
	_ = ln.Close()
	return addr
}

// TestNewUDPServer verifies that NewUDP returns a non-nil UDPServer.
func TestNewUDPServer(t *testing.T) {
	cfg := &config.Config{UDPAddr: "127.0.0.1:0"}
	s := NewUDP(cfg, testLogger())
	if s == nil {
		t.Fatal("NewUDP returned nil UDPServer")
	}
}

// TestUDPServerStartShutdownPingPong exercises the full lifecycle: start the
// server, send a UDPMsgPing packet, expect a UDPMsgPong reply, then cancel the
// context and assert Shutdown returns cleanly. It also asserts that Stats()
// reports non-zero counters after the exchange.
func TestUDPServerStartShutdownPingPong(t *testing.T) {
	addr := freeUDPPort(t)
	cfg := &config.Config{UDPAddr: addr}
	s := NewUDP(cfg, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startErr := make(chan error, 1)
	go func() {
		startErr <- s.Start(ctx)
	}()
	select {
	case <-s.started:
		// The socket is bound; UDP dial cannot race the listener startup.
	case err := <-startErr:
		t.Fatalf("Start returned before binding: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("UDP server did not bind within 3 seconds")
	}

	// Resolve the server address and dial a UDP client.
	srvAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatalf("resolve server addr: %v", err)
	}

	conn, err := net.DialUDP("udp", nil, srvAddr)
	if err != nil {
		t.Fatalf("dial udp server: %v", err)
	}
	defer conn.Close()

	// Send a UDPMsgPing packet and expect a UDPMsgPong reply. The ping is
	// retried because UDP delivery itself is lossy even after the socket binds.
	buf := make([]byte, 64)
	deadline := time.Now().Add(5 * time.Second)
	var n int
	for {
		if _, err := conn.Write([]byte{netproto.UDPMsgPing}); err != nil {
			t.Fatalf("write ping: %v", err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		n, _, err = conn.ReadFromUDP(buf)
		if err == nil {
			break
		}
		if nerr, ok := err.(net.Error); ok && nerr.Timeout() && time.Now().Before(deadline) {
			continue
		}
		t.Fatalf("read pong: %v", err)
	}
	if n < 1 || buf[0] != netproto.UDPMsgPong {
		t.Fatalf("reply = %x, want %x", buf[:n], netproto.UDPMsgPong)
	}

	// Wait for the server to process the packet so Stats() reflects it.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if stats := s.Stats(); stats.PacketsReceived > 0 && stats.PacketsProcessed > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	stats := s.Stats()
	if stats.PacketsReceived == 0 {
		t.Errorf("Stats().PacketsReceived = 0, want > 0")
	}
	if stats.PacketsProcessed == 0 {
		t.Errorf("Stats().PacketsProcessed = 0, want > 0")
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

// tcp_tls_test.go covers the TLS layer of the control channel: handshake
// round-trip, fingerprint surfacing in the auth response, and rejection of
// plaintext clients when TLS is enabled.
package server

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"voicx/internal/config"
	"voicx/internal/netproto"
)

// startTLSTestServer starts a bare TCPServer and returns its address plus a
// stop function. tlsEnabled toggles the TLS listener (the readiness probe
// uses a matching dial).
func startTLSTestServer(t *testing.T, tlsEnabled bool) (string, *TCPServer, func()) {
	t.Helper()
	addr := freePort(t)
	srv := New(&config.Config{
		TCPAddr:    addr,
		TLSEnabled: tlsEnabled,
		TLSDir:     t.TempDir(),
	}, testLogger(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(ctx) }()

	probe := func() bool {
		if tlsEnabled {
			conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // test client
			if err == nil {
				_ = conn.Close()
				return true
			}
			return false
		}
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			_ = conn.Close()
			return true
		}
		return false
	}
	deadline := time.Now().Add(3 * time.Second)
	for !probe() {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("server did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return addr, srv, func() {
		cancel()
		<-startErr
		_ = srv.Shutdown()
	}
}

// TestTLSHandshakeAndFingerprint verifies a TLS client can complete the
// handshake and a successful auth response carries the certificate
// fingerprint.
func TestTLSHandshakeAndFingerprint(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, err := tls.Dial("tcp", env.addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // test client
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		t.Fatal("no peer certificates presented")
	}

	send(t, conn, netproto.MsgAuthenticate, netproto.Authenticate{Username: "user-uid", Password: "pw"})
	f := readOfType(t, conn, netproto.MsgAuthResponse)
	var resp netproto.AuthResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode auth response: %v", err)
	}
	if !resp.OK {
		t.Fatalf("auth failed: %s", resp.Reason)
	}
	if resp.TLSFingerprint == "" {
		t.Fatal("auth response carries no TLS fingerprint")
	}
}

// TestTLSRejectsPlaintext verifies a plaintext client cannot speak to a
// TLS-enabled listener.
func TestTLSRejectsPlaintext(t *testing.T) {
	addr, _, stop := startTLSTestServer(t, true)
	defer stop()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("tcp dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// A plaintext frame write hits the TLS record layer: the server either
	// errors the read or closes the connection; we must not get a Pong.
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	pingFrame, _ := netproto.Encode(netproto.MsgPing, netproto.Ping{})
	_ = netproto.WriteFrame(conn, pingFrame)
	if _, err := netproto.ReadFrame(conn); err == nil {
		t.Fatal("plaintext client received a protocol reply from a TLS listener")
	}
}

// TestTLSRequiresVersion13 pins the control listener's documented minimum.
func TestTLSRequiresVersion13(t *testing.T) {
	addr, _, stop := startTLSTestServer(t, true)
	defer stop()

	legacy, err := tls.Dial("tcp", addr, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // protocol-version test
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tls.VersionTLS12,
	})
	if err == nil {
		_ = legacy.Close()
		t.Fatal("TLS 1.2 handshake succeeded, want rejection")
	}

	current, err := tls.Dial("tcp", addr, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // test client
		MinVersion:         tls.VersionTLS13,
	})
	if err != nil {
		t.Fatalf("TLS 1.3 dial: %v", err)
	}
	defer func() { _ = current.Close() }()
	if version := current.ConnectionState().Version; version != tls.VersionTLS13 {
		t.Fatalf("negotiated TLS version = %#x, want TLS 1.3", version)
	}
}

// TestPlaintextStillAllowed verifies tls_enabled=false keeps the dev/e2e
// escape hatch working.
func TestPlaintextStillAllowed(t *testing.T) {
	addr, srv, stop := startTLSTestServer(t, false)
	defer stop()

	if srv.TLSFingerprint() != "" {
		t.Fatal("plaintext server reports a TLS fingerprint")
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("tcp dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	send(t, conn, netproto.MsgPing, netproto.Ping{})
	f := readFrame(t, conn)
	if netproto.MessageType(f.Type) != netproto.MsgPong {
		t.Fatalf("response = %s, want Pong", netproto.MessageType(f.Type))
	}
}

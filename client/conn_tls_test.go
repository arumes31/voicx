package main

import (
	"crypto/tls"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"voicx/internal/tlscert"
)

func TestDialTransportPreservesFingerprintMismatch(t *testing.T) {
	cert, presentedFingerprint, err := tlscert.Ensure(t.TempDir(), "", "", nil)
	if err != nil {
		t.Fatalf("create TLS certificate: %v", err)
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		if tlsConn, ok := conn.(*tls.Conn); ok {
			_ = tlsConn.Handshake()
		}
		_ = conn.Close()
	}()

	addr := listener.Addr().String()
	store := loadKnownServersAt(filepath.Join(t.TempDir(), "known_servers.json"))
	if err := store.trust(addr, tlscert.FingerprintDER([]byte("old certificate"))); err != nil {
		t.Fatalf("seed old fingerprint: %v", err)
	}
	manager := newConnManager(nil)
	manager.knownServers = store
	conn, err := manager.dialTransport(addr)
	if conn != nil {
		_ = conn.Close()
		t.Fatal("dial with changed certificate unexpectedly succeeded")
	}
	if !errors.Is(err, errFingerprintMismatch) {
		t.Fatalf("dial error = %v, want errFingerprintMismatch", err)
	}
	tlsUsed, gotFingerprint, firstSeen := manager.securitySnapshot()
	if !tlsUsed || gotFingerprint != presentedFingerprint || firstSeen {
		t.Fatalf("security snapshot = (%v, %q, %v), want (true, %q, false)",
			tlsUsed, gotFingerprint, firstSeen, presentedFingerprint)
	}

	_ = listener.Close()
	select {
	case <-serverDone:
	case <-time.After(5 * time.Second):
		t.Fatal("TLS test server did not stop")
	}
}

func TestFingerprintMismatchMessageIncludesPresentedFingerprint(t *testing.T) {
	manager := newConnManager(nil)
	want := tlscert.FingerprintDER([]byte("replacement certificate"))
	manager.mu.Lock()
	manager.tlsUsed = true
	manager.fingerprint = want
	manager.mu.Unlock()
	message := fingerprintMismatchMessage(manager)
	if !strings.Contains(message, "presented: "+want) {
		t.Fatalf("mismatch message = %q", message)
	}
}

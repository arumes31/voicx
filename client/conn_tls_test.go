package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"voicx/internal/tlscert"
)

func TestDialTransportTrustStoreFailureNeverFallsBackToPlaintext(t *testing.T) {
	for _, test := range []struct {
		name  string
		store func(t *testing.T) *knownServers
	}{
		{
			name: "corrupt store",
			store: func(t *testing.T) *knownServers {
				path := filepath.Join(t.TempDir(), "known_servers.json")
				if err := os.WriteFile(path, []byte("{bad json"), 0o600); err != nil {
					t.Fatal(err)
				}
				return loadKnownServersAt(path)
			},
		},
		{
			name: "canonical collision",
			store: func(t *testing.T) *knownServers {
				path := filepath.Join(t.TempDir(), "known_servers.json")
				if err := os.WriteFile(path, []byte(`{"servers":{"EXAMPLE.com.:123":"one","example.com:123":"two"}}`), 0o600); err != nil {
					t.Fatal(err)
				}
				return loadKnownServersAt(path)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cert, _, err := tlscert.Ensure(t.TempDir(), "", "", nil)
			if err != nil {
				t.Fatalf("create TLS certificate: %v", err)
			}
			listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			defer func() { _ = listener.Close() }()
			tcpListener := listener.(*net.TCPListener)
			tlsSeen := make(chan struct{}, 1)
			plainSeen := make(chan struct{}, 1)
			serverDone := make(chan struct{})
			go func() {
				defer close(serverDone)
				_ = tcpListener.SetDeadline(time.Now().Add(2 * time.Second))
				conn, acceptErr := listener.Accept()
				if acceptErr != nil {
					return
				}
				tlsSeen <- struct{}{}
				_ = tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}).HandshakeContext(t.Context())
				_ = conn.Close()
				_ = tcpListener.SetDeadline(time.Now().Add(100 * time.Millisecond))
				conn, acceptErr = listener.Accept()
				if acceptErr == nil {
					plainSeen <- struct{}{}
					_ = conn.Close()
				}
			}()

			manager := newConnManager(context.Background())
			manager.knownServers = test.store(t)
			manager.allowPlaintext = true
			conn, err := manager.dialTransport(listener.Addr().String())
			if conn != nil {
				_ = conn.Close()
				t.Fatal("trust-store failure unexpectedly connected")
			}
			if !errors.Is(err, errTrustStoreUnavailable) {
				t.Fatalf("dial error = %v, want trust-store unavailable", err)
			}
			select {
			case <-tlsSeen:
			case <-time.After(time.Second):
				t.Fatal("trust-store failure did not make its TLS attempt")
			}
			select {
			case <-serverDone:
			case <-time.After(time.Second):
				t.Fatal("dual-mode listener did not finish")
			}
			select {
			case <-plainSeen:
				t.Fatal("trust-store failure retried over plaintext")
			default:
			}
		})
	}
}

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
			_ = tlsConn.HandshakeContext(t.Context())
		}
		_ = conn.Close()
	}()

	addr := listener.Addr().String()
	store := loadKnownServersAt(filepath.Join(t.TempDir(), "known_servers.json"))
	if err := store.trust(addr, tlscert.FingerprintDER([]byte("old certificate"))); err != nil {
		t.Fatalf("seed old fingerprint: %v", err)
	}
	manager := newConnManager(context.Background())
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
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse TLS certificate: %v", err)
	}
	notBefore, notAfter, trusted := manager.certificateValiditySnapshot()
	if !notBefore.Equal(leaf.NotBefore) || !notAfter.Equal(leaf.NotAfter) {
		t.Fatalf(
			"certificate validity snapshot = (%v, %v), want (%v, %v)",
			notBefore,
			notAfter,
			leaf.NotBefore,
			leaf.NotAfter,
		)
	}
	if trusted {
		t.Fatal("fingerprint-mismatched certificate validity was marked trusted")
	}

	_ = listener.Close()
	select {
	case <-serverDone:
	case <-time.After(5 * time.Second):
		t.Fatal("TLS test server did not stop")
	}
}

func TestDialTransportRetainsAcceptedCertificateValidity(t *testing.T) {
	cert, presentedFingerprint, err := tlscert.Ensure(t.TempDir(), "", "", nil)
	if err != nil {
		t.Fatalf("create TLS certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse TLS certificate: %v", err)
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
			_ = tlsConn.HandshakeContext(t.Context())
		}
		_ = conn.Close()
	}()

	addr := listener.Addr().String()
	store := loadKnownServersAt(filepath.Join(t.TempDir(), "known_servers.json"))
	manager := newConnManager(context.Background())
	manager.knownServers = store
	conn, err := manager.dialTransport(addr)
	if err != nil {
		t.Fatalf("dial first-seen TLS server: %v", err)
	}
	manager.mu.Lock()
	manager.conn = conn
	manager.mu.Unlock()

	notBefore, notAfter, trusted := manager.certificateValiditySnapshot()
	if !trusted || !notBefore.Equal(leaf.NotBefore) || !notAfter.Equal(leaf.NotAfter) {
		t.Fatalf(
			"accepted certificate validity = (%v, %v, %v), want (%v, %v, true)",
			notBefore,
			notAfter,
			trusted,
			leaf.NotBefore,
			leaf.NotAfter,
		)
	}
	if got, err := store.verify(addr, presentedFingerprint); err != nil || got != trustOK {
		t.Fatalf("first-seen fingerprint status = %v, want trustOK", got)
	}
	app := appWithCM(manager)
	if warning := app.CertificateClockWarning(); warning != "" {
		t.Fatalf("currently valid accepted certificate warning = %q", warning)
	}

	manager.disconnect()
	notBefore, notAfter, trusted = manager.certificateValiditySnapshot()
	if !notBefore.IsZero() || !notAfter.IsZero() || trusted {
		t.Fatalf("disconnect retained certificate validity = (%v, %v, %v)", notBefore, notAfter, trusted)
	}
	if warning := app.CertificateClockWarning(); warning != "" {
		t.Fatalf("disconnected certificate warning = %q", warning)
	}

	_ = listener.Close()
	select {
	case <-serverDone:
	case <-time.After(5 * time.Second):
		t.Fatal("TLS test server did not stop")
	}
}

func TestFingerprintMismatchMessageIncludesPresentedFingerprint(t *testing.T) {
	manager := newConnManager(context.Background())
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

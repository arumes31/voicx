package main

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"strings"
	"testing"

	"voicx/internal/tlscert"
)

func TestPinFingerprint(t *testing.T) {
	t.Parallel()

	leaf := []byte("test certificate DER")
	want := tlscert.FingerprintDER(leaf)
	verify := pinFingerprint(strings.ToUpper(want))
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Raw: leaf}}}
	if err := verify(state); err != nil {
		t.Fatalf("matching certificate rejected: %v", err)
	}
	if err := verify(tls.ConnectionState{}); err == nil {
		t.Fatal("missing certificate accepted")
	}
	different := tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Raw: []byte("different certificate")}}}
	if err := verify(different); err == nil {
		t.Fatal("mismatched certificate accepted")
	}
}

func TestPinFingerprintRequiresPin(t *testing.T) {
	t.Parallel()

	verify := pinFingerprint("")
	if verify == nil {
		t.Fatal("empty fingerprint disabled the custom verifier")
	}
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Raw: []byte("certificate")}}}
	if err := verify(state); err == nil {
		t.Fatal("empty fingerprint accepted a certificate")
	}
}

func TestFTDialRequiresPin(t *testing.T) {
	t.Parallel()

	if _, err := ftDial(ftEndpoint{addr: "127.0.0.1:1", tls: true}); err == nil ||
		!strings.Contains(err.Error(), "fingerprint is missing") {
		t.Fatalf("ftDial without pin error = %v", err)
	}
}

func TestPinnedTLSConfigUsesStandardVerification(t *testing.T) {
	t.Parallel()

	cert, fingerprint, err := tlscert.Ensure(t.TempDir(), "", "", nil)
	if err != nil {
		t.Fatalf("create TLS certificate: %v", err)
	}
	config, err := pinnedTLSConfig(cert.Certificate[0], fingerprint)
	if err != nil {
		t.Fatalf("build pinned TLS config: %v", err)
	}
	if config.InsecureSkipVerify {
		t.Fatal("pinned TLS config disabled standard certificate verification")
	}

	serverConn, clientConn := net.Pipe()
	serverTLS := tls.Server(serverConn, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	})
	clientTLS := tls.Client(clientConn, config)
	t.Cleanup(func() {
		_ = clientTLS.Close()
		_ = serverTLS.Close()
	})
	serverResult := make(chan error, 1)
	go func() { serverResult <- serverTLS.HandshakeContext(t.Context()) }()
	if err := clientTLS.HandshakeContext(t.Context()); err != nil {
		t.Fatalf("client handshake with pinned certificate: %v", err)
	}
	if err := <-serverResult; err != nil {
		t.Fatalf("server handshake with pinned certificate: %v", err)
	}
}

func TestPinnedTLSConfigRejectsMismatchedCertificateMaterial(t *testing.T) {
	t.Parallel()

	cert, _, err := tlscert.Ensure(t.TempDir(), "", "", nil)
	if err != nil {
		t.Fatalf("create TLS certificate: %v", err)
	}
	want := tlscert.FingerprintDER([]byte("different certificate"))
	if _, err := pinnedTLSConfig(cert.Certificate[0], want); err == nil ||
		!strings.Contains(err.Error(), "certificate mismatch") {
		t.Fatalf("mismatched certificate material error = %v", err)
	}
}

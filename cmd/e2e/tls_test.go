package main

import (
	"crypto/tls"
	"crypto/x509"
	"strings"
	"testing"

	"voicx/internal/netproto"
	"voicx/internal/tlscert"
)

func TestControlTLSConfigRestrictsInsecureMode(t *testing.T) {
	if _, _, err := controlTLSConfig("192.0.2.1:12333", controlTLSMode{insecure: true}); err == nil {
		t.Fatal("remote -tls-insecure address accepted")
	}

	cfg, enabled, err := controlTLSConfig("127.0.0.1:12333", controlTLSMode{insecure: true})
	if err != nil {
		t.Fatalf("loopback -tls-insecure: %v", err)
	}
	if !enabled || !cfg.InsecureSkipVerify || cfg.MinVersion != tls.VersionTLS13 {
		t.Fatalf("unexpected loopback TLS config: %+v", cfg)
	}
}

func TestPinnedTLSConfigVerifiesExactCertificate(t *testing.T) {
	cert, fingerprint, err := tlscert.Ensure(t.TempDir(), "", "", []string{"localhost"})
	if err != nil {
		t.Fatalf("generate certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}

	cfg, err := pinnedTLSConfig(fingerprint)
	if err != nil {
		t.Fatalf("pinnedTLSConfig: %v", err)
	}
	if err := cfg.VerifyConnection(state); err != nil {
		t.Fatalf("matching certificate rejected: %v", err)
	}

	wrong := strings.Repeat("00:", 31) + "00"
	cfg, err = pinnedTLSConfig(wrong)
	if err != nil {
		t.Fatalf("wrong pin syntax: %v", err)
	}
	if err := cfg.VerifyConnection(state); err == nil {
		t.Fatal("mismatched certificate accepted")
	}
}

func TestDialFileTransferRequiresConsistentTLSMetadata(t *testing.T) {
	if _, err := dialFileTransfer("127.0.0.1:1", netproto.FileTransferInitResponse{TLS: true}); err == nil {
		t.Fatal("TLS file-transfer response without fingerprint accepted")
	}
	if _, err := dialFileTransfer("127.0.0.1:1", netproto.FileTransferInitResponse{
		TLSFingerprint: strings.Repeat("00:", 31) + "00",
	}); err == nil {
		t.Fatal("plaintext file-transfer response with fingerprint accepted")
	}
}

package main

import (
	"crypto/tls"
	"crypto/x509"
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

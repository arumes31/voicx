package main

import (
	"strings"
	"testing"

	"voicx/internal/tlscert"
)

func TestPinFingerprint(t *testing.T) {
	t.Parallel()

	leaf := []byte("test certificate DER")
	want := tlscert.FingerprintDER(leaf)
	verify := pinFingerprint(strings.ToUpper(want))
	if err := verify([][]byte{leaf}, nil); err != nil {
		t.Fatalf("matching certificate rejected: %v", err)
	}
	if err := verify(nil, nil); err == nil {
		t.Fatal("missing certificate accepted")
	}
	if err := verify([][]byte{[]byte("different certificate")}, nil); err == nil {
		t.Fatal("mismatched certificate accepted")
	}
}

func TestPinFingerprintRequiresPin(t *testing.T) {
	t.Parallel()

	verify := pinFingerprint("")
	if verify == nil {
		t.Fatal("empty fingerprint disabled the custom verifier")
	}
	if err := verify([][]byte{[]byte("certificate")}, nil); err == nil {
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

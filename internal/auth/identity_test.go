package auth

import (
	"bytes"
	"testing"
)

// TestGenerateIdentityKeyPairNonEmpty verifies that GenerateIdentityKeyPair
// returns non-empty PEM strings.
func TestGenerateIdentityKeyPairNonEmpty(t *testing.T) {
	pub, priv, err := GenerateIdentityKeyPair()
	if err != nil {
		t.Fatalf("GenerateIdentityKeyPair: %v", err)
	}
	if pub == "" {
		t.Fatal("expected non-empty public key PEM")
	}
	if priv == "" {
		t.Fatal("expected non-empty private key PEM")
	}
	if !bytes.Contains([]byte(pub), []byte("BEGIN PUBLIC KEY")) {
		t.Fatalf("public key PEM missing header: %q", pub)
	}
	if !bytes.Contains([]byte(priv), []byte("BEGIN PRIVATE KEY")) {
		t.Fatalf("private key PEM missing header: %q", priv)
	}
}

// TestUniqueIDDeterministic verifies that UniqueIDFromPublicKey returns a
// non-empty deterministic string for the same key.
func TestUniqueIDDeterministic(t *testing.T) {
	pub, _, err := GenerateIdentityKeyPair()
	if err != nil {
		t.Fatalf("GenerateIdentityKeyPair: %v", err)
	}
	id1, err := UniqueIDFromPublicKey(pub)
	if err != nil {
		t.Fatalf("UniqueIDFromPublicKey 1: %v", err)
	}
	if id1 == "" {
		t.Fatal("expected non-empty unique id")
	}
	id2, err := UniqueIDFromPublicKey(pub)
	if err != nil {
		t.Fatalf("UniqueIDFromPublicKey 2: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("expected deterministic unique id, got %q and %q", id1, id2)
	}

	// Different keys should produce different IDs.
	pub2, _, err := GenerateIdentityKeyPair()
	if err != nil {
		t.Fatalf("GenerateIdentityKeyPair 2: %v", err)
	}
	id3, err := UniqueIDFromPublicKey(pub2)
	if err != nil {
		t.Fatalf("UniqueIDFromPublicKey 3: %v", err)
	}
	if id1 == id3 {
		t.Fatal("expected different unique ids for different keys")
	}
}

// TestSignVerifyChallengeRoundTrip verifies that SignChallenge + VerifyChallenge
// round-trip succeeds for a valid signature and fails for a tampered one.
func TestSignVerifyChallengeRoundTrip(t *testing.T) {
	pub, priv, err := GenerateIdentityKeyPair()
	if err != nil {
		t.Fatalf("GenerateIdentityKeyPair: %v", err)
	}
	challenge := []byte("voicx-challenge-12345")

	sig, err := SignChallenge(priv, challenge)
	if err != nil {
		t.Fatalf("SignChallenge: %v", err)
	}
	if len(sig) == 0 {
		t.Fatal("expected non-empty signature")
	}

	if err := VerifyChallenge(pub, challenge, sig); err != nil {
		t.Fatalf("VerifyChallenge(valid): %v", err)
	}

	// Tamper with the signature.
	tampered := make([]byte, len(sig))
	copy(tampered, sig)
	tampered[0] ^= 0xff
	if err := VerifyChallenge(pub, challenge, tampered); err == nil {
		t.Fatal("VerifyChallenge(tampered sig) unexpectedly succeeded")
	}

	// Tamper with the challenge.
	badChallenge := append([]byte(nil), challenge...)
	badChallenge[0] ^= 0xff
	if err := VerifyChallenge(pub, badChallenge, sig); err == nil {
		t.Fatal("VerifyChallenge(tampered challenge) unexpectedly succeeded")
	}
}

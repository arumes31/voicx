package main

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/nacl/box"
)

func genPair(t *testing.T) (pub, priv [32]byte) {
	t.Helper()
	p, s, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("box.GenerateKey: %v", err)
	}
	return *p, *s
}

// TestDMRoundTrip verifies box seal/open for direct messages, including
// wrong-key rejection.
func TestDMRoundTrip(t *testing.T) {
	alicePub, alicePriv := genPair(t)
	bobPub, bobPriv := genPair(t)

	blob, err := sealDM("hello bob", bobPub, alicePriv)
	if err != nil {
		t.Fatalf("sealDM: %v", err)
	}
	if blob == "hello bob" {
		t.Fatal("ciphertext equals plaintext")
	}
	plain, err := openDM(blob, alicePub, bobPriv)
	if err != nil {
		t.Fatalf("openDM: %v", err)
	}
	if plain != "hello bob" {
		t.Fatalf("plaintext = %q, want %q", plain, "hello bob")
	}

	// Wrong recipient key must fail.
	evePub, _ := genPair(t)
	if _, err := openDM(blob, evePub, bobPriv); err == nil {
		t.Fatal("openDM with wrong sender key succeeded")
	}
	// Tampered ciphertext must fail.
	raw := []byte(blob)
	raw[len(raw)-5] ^= 0xFF
	if _, err := openDM(string(raw), alicePub, bobPriv); err == nil {
		t.Fatal("openDM on tampered blob succeeded")
	}
}

// TestScopeRoundTrip verifies secretbox seal/open for channel/global
// messages, including wrong-key rejection.
func TestScopeRoundTrip(t *testing.T) {
	var key, other [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(other[:]); err != nil {
		t.Fatal(err)
	}

	blob, err := sealScope("channel secret", key)
	if err != nil {
		t.Fatalf("sealScope: %v", err)
	}
	plain, err := openScope(blob, key)
	if err != nil {
		t.Fatalf("openScope: %v", err)
	}
	if plain != "channel secret" {
		t.Fatalf("plaintext = %q", plain)
	}
	if _, err := openScope(blob, other); err == nil {
		t.Fatal("openScope with wrong key succeeded")
	}
}

// TestScopeKeyStore verifies latest-generation tracking and per-generation
// lookup (key rotation: old generations stay readable).
func TestScopeKeyStore(t *testing.T) {
	s := newScopeKeyStore()
	var k1, k2 [32]byte
	k1[0], k2[0] = 1, 2
	s.put(7, 1, k1)
	s.put(7, 2, k2)

	id, cur, ok := s.current(7)
	if !ok || id != 2 || cur != k2 {
		t.Fatalf("current = %d/%v, want 2/k2", id, ok)
	}
	if got, ok := s.get(7, 1); !ok || got != k1 {
		t.Fatal("generation 1 lost after rotation")
	}
	if _, _, ok := s.current(99); ok {
		t.Fatal("unknown scope has a key")
	}
}

// TestIdentityX25519Upgrade verifies an old identity.json (no X25519 fields)
// is upgraded in place and the pair decodes.
func TestIdentityX25519Upgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	// Old-format identity: Ed25519 fields only.
	old := `{"public_key":"-----BEGIN PUBLIC KEY-----\nx\n-----END PUBLIC KEY-----","private_key":"-----BEGIN PRIVATE KEY-----\ny\n-----END PRIVATE KEY-----"}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	id, err := loadOrCreateIdentityAt(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if id.X25519Public == "" || id.X25519Private == "" {
		t.Fatal("old identity not upgraded with X25519 pair")
	}
	if _, _, err := id.x25519(); err != nil {
		t.Fatalf("x25519 decode: %v", err)
	}
	// Reload: the upgrade persisted.
	id2, err := loadOrCreateIdentityAt(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if id2.X25519Public != id.X25519Public {
		t.Fatal("X25519 key changed across reload (upgrade not persisted)")
	}
}

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIdentityRoundTrip verifies first-run generation, persistence, stable
// reload, and a stable derived unique ID.
func TestIdentityRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voicx", "identity.json")

	id1, err := loadOrCreateIdentityAt(path)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if id1.PublicKey == "" || id1.PrivateKey == "" {
		t.Fatal("generated identity has empty keys")
	}

	// File exists with restrictive content.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("identity file not written: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("identity file is empty")
	}

	// Second load returns the SAME key pair.
	id2, err := loadOrCreateIdentityAt(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if id1.PublicKey != id2.PublicKey || id1.PrivateKey != id2.PrivateKey {
		t.Fatal("reloaded identity differs from generated one")
	}

	// Unique ID derives and is stable.
	uid1, err := id1.uniqueID()
	if err != nil {
		t.Fatalf("uniqueID: %v", err)
	}
	uid2, err := id2.uniqueID()
	if err != nil {
		t.Fatalf("uniqueID (reloaded): %v", err)
	}
	if uid1 == "" || uid1 != uid2 {
		t.Fatalf("unique IDs differ: %q vs %q", uid1, uid2)
	}

	// A different path gets a DIFFERENT identity.
	id3, err := loadOrCreateIdentityAt(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatalf("second path generate: %v", err)
	}
	if id3.PublicKey == id1.PublicKey {
		t.Fatal("different paths produced the same identity")
	}
}

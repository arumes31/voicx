package main

import (
	"path/filepath"
	"testing"
)

// TestTOFUFirstSeenThenMatch verifies the trust-on-first-use lifecycle:
// unknown → trust → later verifies OK.
func TestTOFUFirstSeenThenMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_servers.json")
	ks := loadKnownServersAt(path)

	addr := "example.com:12333"
	fp := "aa:bb:cc"
	if got := ks.verify(addr, fp); got != trustUnknown {
		t.Fatalf("verify (first seen) = %v, want trustUnknown", got)
	}
	if err := ks.trust(addr, fp); err != nil {
		t.Fatalf("trust: %v", err)
	}
	if got := ks.verify(addr, fp); got != trustOK {
		t.Fatalf("verify (after trust) = %v, want trustOK", got)
	}
}

// TestTOFUPersistence verifies the store survives a reload.
func TestTOFUPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_servers.json")
	ks := loadKnownServersAt(path)
	if err := ks.trust("a:1", "fp-a"); err != nil {
		t.Fatalf("trust: %v", err)
	}
	if err := ks.trust("b:2", "fp-b"); err != nil {
		t.Fatalf("trust: %v", err)
	}

	reloaded := loadKnownServersAt(path)
	if got := reloaded.verify("a:1", "fp-a"); got != trustOK {
		t.Fatalf("verify a = %v, want trustOK", got)
	}
	if got := reloaded.verify("b:2", "fp-b"); got != trustOK {
		t.Fatalf("verify b = %v, want trustOK", got)
	}
	if got := reloaded.verify("c:3", "fp-c"); got != trustUnknown {
		t.Fatalf("verify c = %v, want trustUnknown", got)
	}
}

// TestTOFUMismatch verifies a changed fingerprint is rejected, and that
// explicitly trusting the new fingerprint resolves the mismatch.
func TestTOFUMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_servers.json")
	ks := loadKnownServersAt(path)
	addr := "example.com:12333"
	if err := ks.trust(addr, "old-fp"); err != nil {
		t.Fatalf("trust: %v", err)
	}

	if got := ks.verify(addr, "new-fp"); got != trustMismatch {
		t.Fatalf("verify (changed) = %v, want trustMismatch", got)
	}
	// Explicit user action: trust the new fingerprint.
	if err := ks.trust(addr, "new-fp"); err != nil {
		t.Fatalf("trust new: %v", err)
	}
	if got := ks.verify(addr, "new-fp"); got != trustOK {
		t.Fatalf("verify (after re-trust) = %v, want trustOK", got)
	}
}

// TestTOFUMissingFile verifies a missing store behaves as empty.
func TestTOFUMissingFile(t *testing.T) {
	ks := loadKnownServersAt(filepath.Join(t.TempDir(), "nope.json"))
	if got := ks.verify("x:1", "fp"); got != trustUnknown {
		t.Fatalf("verify = %v, want trustUnknown", got)
	}
}

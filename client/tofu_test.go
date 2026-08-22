package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"voicx/internal/tlscert"
)

func mustVerifyTOFU(t *testing.T, ks *knownServers, addr, fp string) trustStatus {
	t.Helper()
	got, err := ks.verify(addr, fp)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestTOFUFirstSeenThenMatch verifies the trust-on-first-use lifecycle:
// unknown → trust → later verifies OK.
func TestTOFUFirstSeenThenMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_servers.json")
	ks := loadKnownServersAt(path)

	addr := "example.com:12333"
	fp := "aa:bb:cc"
	if got := mustVerifyTOFU(t, ks, addr, fp); got != trustUnknown {
		t.Fatalf("verify (first seen) = %v, want trustUnknown", got)
	}
	if err := ks.trust(addr, fp); err != nil {
		t.Fatalf("trust: %v", err)
	}
	if got := mustVerifyTOFU(t, ks, addr, fp); got != trustOK {
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
	if got := mustVerifyTOFU(t, reloaded, "a:1", "fp-a"); got != trustOK {
		t.Fatalf("verify a = %v, want trustOK", got)
	}
	if got := mustVerifyTOFU(t, reloaded, "b:2", "fp-b"); got != trustOK {
		t.Fatalf("verify b = %v, want trustOK", got)
	}
	if got := mustVerifyTOFU(t, reloaded, "c:3", "fp-c"); got != trustUnknown {
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

	if got := mustVerifyTOFU(t, ks, addr, "new-fp"); got != trustMismatch {
		t.Fatalf("verify (changed) = %v, want trustMismatch", got)
	}
	// Explicit user action: trust the new fingerprint.
	if err := ks.trust(addr, "new-fp"); err != nil {
		t.Fatalf("trust new: %v", err)
	}
	if got := mustVerifyTOFU(t, ks, addr, "new-fp"); got != trustOK {
		t.Fatalf("verify (after re-trust) = %v, want trustOK", got)
	}
}

// TestTOFUMissingFile verifies a missing store behaves as empty.
func TestTOFUMissingFile(t *testing.T) {
	ks := loadKnownServersAt(filepath.Join(t.TempDir(), "nope.json"))
	if got := mustVerifyTOFU(t, ks, "x:1", "fp"); got != trustUnknown {
		t.Fatalf("verify = %v, want trustUnknown", got)
	}
}

func TestTOFUCorruptStoreFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_servers.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("seed corrupt store: %v", err)
	}
	ks := loadKnownServersAt(path)
	if _, err := ks.verify("server.test:12333", "fingerprint"); err == nil ||
		!errors.Is(err, errTrustStoreUnavailable) {
		t.Fatalf("verify with corrupt store error = %v", err)
	}
	if err := ks.trust("server.test:12333", "fingerprint"); err == nil ||
		!errors.Is(err, errTrustStoreUnavailable) {
		t.Fatalf("trust with corrupt store error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corrupt store: %v", err)
	}
	if string(got) != "{not-json" {
		t.Fatalf("corrupt trust store was overwritten: %q", got)
	}
}

func TestNormalizeServerAddrAliases(t *testing.T) {
	for _, test := range []struct {
		in, want string
	}{
		{" Example.COM.:12333 ", "example.com:12333"},
		{"127.0.0.1:12", "127.0.0.1:12"},
		{"[2001:0db8::1]:443", "[2001:db8::1]:443"},
	} {
		got, err := normalizeServerAddr(test.in)
		if err != nil || got != test.want {
			t.Fatalf("normalizeServerAddr(%q) = %q, %v; want %q", test.in, got, err, test.want)
		}
	}
	for _, invalid := range []string{"host", "host:0", "host:65536", "[::1]", ":123"} {
		if _, err := normalizeServerAddr(invalid); err == nil {
			t.Fatalf("normalizeServerAddr(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestTOFULegacyAliasesCollapseAndConflictsFailClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_servers.json")
	if err := os.WriteFile(path, []byte(`{"servers":{"EXAMPLE.com.:123":"same","example.com:123":"same"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ks := loadKnownServersAt(path)
	if got := mustVerifyTOFU(t, ks, "example.com:123", "same"); got != trustOK {
		t.Fatalf("canonical legacy pin = %v", got)
	}
	if len(ks.Servers) != 1 {
		t.Fatalf("canonical pins = %#v", ks.Servers)
	}
	if err := os.WriteFile(path, []byte(`{"servers":{"EXAMPLE.com.:123":"one","example.com:123":"two"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ks = loadKnownServersAt(path)
	if _, err := ks.verify("example.com:123", "one"); err == nil || !errors.Is(err, errTrustStoreUnavailable) {
		t.Fatalf("conflicting aliases verify = %v", err)
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	fingerprint := tlscert.FingerprintDER([]byte("certificate"))
	got, err := normalizeFingerprint(strings.ToUpper(fingerprint))
	if err != nil || got != fingerprint {
		t.Fatalf("normalizeFingerprint() = (%q, %v), want (%q, nil)", got, err, fingerprint)
	}
	for _, invalid := range []string{"", "aa:bb", strings.Repeat("gg:", 31) + "gg"} {
		if _, err := normalizeFingerprint(invalid); err == nil {
			t.Fatalf("normalizeFingerprint(%q) succeeded", invalid)
		}
	}
}

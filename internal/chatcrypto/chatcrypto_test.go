// chatcrypto_test.go covers the KEK ring (91-135). Pure Go, no database: the
// ring is the single point where losing or mis-parsing key material silently
// orphans every stored ciphertext, so every failure mode here must be loud.
package chatcrypto

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// b64Key encodes n distinct raw bytes as one KEK value.
func b64Key(fill byte) string {
	var k [32]byte
	for i := range k {
		k[i] = fill + byte(i)
	}
	return base64.StdEncoding.EncodeToString(k[:])
}

// writeRing writes a ring file and returns its path.
func writeRing(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chat_master.key")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("writing ring: %v", err)
	}
	return path
}

// TestKEKWrapUnwrapRoundTrip is the base guarantee: a scope key survives the
// wrap/unwrap cycle byte for byte, and the wrapped form is exactly the layout
// the database column and the store's length check assume.
func TestKEKWrapUnwrapRoundTrip(t *testing.T) {
	ring, err := LoadKEKRing(writeRing(t, "1:"+b64Key(7)), "", false)
	if err != nil {
		t.Fatalf("LoadKEKRing: %v", err)
	}
	var scope [32]byte
	for i := range scope {
		scope[i] = byte(200 - i)
	}
	kekID, wrapped, err := ring.Wrap(scope)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if kekID != 1 || kekID != ring.NewestID() {
		t.Fatalf("Wrap kek id = %d, newest = %d, want 1", kekID, ring.NewestID())
	}
	if len(wrapped) != WrappedLen {
		t.Fatalf("wrapped length = %d, want %d", len(wrapped), WrappedLen)
	}
	got, err := ring.Unwrap(kekID, wrapped)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if got != scope {
		t.Fatal("unwrapped scope key differs from the original")
	}

	// A second wrap of the same key must not repeat the nonce.
	_, wrapped2, err := ring.Wrap(scope)
	if err != nil {
		t.Fatalf("second Wrap: %v", err)
	}
	if string(wrapped2[:24]) == string(wrapped[:24]) {
		t.Fatal("wrap nonce repeated")
	}

	// Tamper and truncation are both refused rather than returning garbage.
	bad := append([]byte(nil), wrapped...)
	bad[len(bad)-1] ^= 0xff
	if _, err := ring.Unwrap(kekID, bad); err == nil {
		t.Fatal("Unwrap accepted tampered ciphertext")
	}
	if _, err := ring.Unwrap(kekID, wrapped[:WrappedLen-1]); err == nil {
		t.Fatal("Unwrap accepted a short wrapped key")
	}
	if _, err := ring.Unwrap(2, wrapped); err == nil {
		t.Fatal("Unwrap accepted an id that is not in the ring")
	}
}

// TestKEKRotationUnwrapsOlderGenerations pins the append-only rotation rule:
// adding a KEK must never make already-stored generations unreadable.
func TestKEKRotationUnwrapsOlderGenerations(t *testing.T) {
	old, err := LoadKEKRing(writeRing(t, "1:"+b64Key(1)), "", false)
	if err != nil {
		t.Fatalf("LoadKEKRing(1): %v", err)
	}
	var scope [32]byte
	scope[0] = 0xab
	kekID, wrapped, err := old.Wrap(scope)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}

	rotated, err := LoadKEKRing(writeRing(t, "# rotated", "", "1:"+b64Key(1), "2:"+b64Key(9)), "", false)
	if err != nil {
		t.Fatalf("LoadKEKRing(1,2): %v", err)
	}
	if rotated.NewestID() != 2 {
		t.Fatalf("NewestID = %d, want 2", rotated.NewestID())
	}
	if ids := rotated.IDs(); len(ids) != 2 {
		t.Fatalf("IDs = %v, want two entries", ids)
	}
	got, err := rotated.Unwrap(kekID, wrapped)
	if err != nil || got != scope {
		t.Fatalf("old generation unreadable after rotation: %v", err)
	}
	newID, _, err := rotated.Wrap(scope)
	if err != nil || newID != 2 {
		t.Fatalf("new wrap used kek %d (err %v), want 2", newID, err)
	}
	// The retired ring must NOT be able to open the new generation's KEK id.
	if _, err := old.Unwrap(2, wrapped); err == nil {
		t.Fatal("stale ring claimed to hold kek 2")
	}
	// A duplicate id is corruption, not a merge.
	if _, err := LoadKEKRing(writeRing(t, "1:"+b64Key(1), "1:"+b64Key(2)), "", false); err == nil {
		t.Fatal("duplicate key id accepted")
	}
}

// TestMissingKEKFailsClosed covers the startup decision: with generations in
// the database and no ring, booting would orphan them, so it must fail; with
// an empty database it may mint one, private to the owner.
func TestMissingKEKFailsClosed(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "keys", "chat_master.key")
	_, err := LoadKEKRing(missing, "", false)
	if err == nil {
		t.Fatal("LoadKEKRing(allowCreate=false) accepted a missing ring")
	}
	if !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "--reset-chat-keys") {
		t.Fatalf("fail-closed error is not actionable: %v", err)
	}

	ring, err := LoadKEKRing(missing, "", true)
	if err != nil {
		t.Fatalf("LoadKEKRing(allowCreate=true): %v", err)
	}
	if ring.NewestID() != 1 {
		t.Fatalf("minted NewestID = %d, want 1", ring.NewestID())
	}
	info, err := os.Stat(missing)
	if err != nil {
		t.Fatalf("minted ring not written: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("minted ring mode = %v, want 0600", info.Mode().Perm())
	}

	// Reloading returns the same material, and creation never clobbers.
	again, err := LoadKEKRing(missing, "", true)
	if err != nil {
		t.Fatalf("reloading minted ring: %v", err)
	}
	if again.Fingerprint() != ring.Fingerprint() {
		t.Fatal("reload produced different key material")
	}

	// The env var wins over the file and stays copy-pasteable without an id.
	env, err := LoadKEKRing(missing, b64Key(3), false)
	if err != nil {
		t.Fatalf("LoadKEKRing(env): %v", err)
	}
	if env.NewestID() != 1 || env.Fingerprint() == ring.Fingerprint() {
		t.Fatal("env ring did not override the file")
	}
	if _, err := LoadKEKRing("", "", true); err == nil {
		t.Fatal("empty path with no env accepted")
	}
}

// TestKEKRejectsNon32ByteMaterial guards the no-passphrase rule: an unsalted
// operator-chosen string is dictionary-recoverable from exactly the dump this
// package defends against, so it is refused with the generation command.
func TestKEKRejectsNon32ByteMaterial(t *testing.T) {
	short := base64.StdEncoding.EncodeToString(make([]byte, 16))
	long := base64.StdEncoding.EncodeToString(make([]byte, 64))
	for _, bad := range []string{"hunter2", "correct horse battery staple", short, long, ""} {
		_, err := LoadKEKRing(writeRing(t, "1:"+bad), "", false)
		if err == nil {
			t.Fatalf("accepted non-32-byte material %q", bad)
		}
		if bad != "" && !strings.Contains(err.Error(), "openssl rand -base64 32") {
			t.Fatalf("error for %q lacks the generation hint: %v", bad, err)
		}
	}
	// A file with only comments holds no keys and must not load as empty.
	if _, err := LoadKEKRing(writeRing(t, "# nothing here"), "", false); err == nil {
		t.Fatal("accepted a ring with no keys")
	}
	// A non-numeric or zero id is refused too: id 0 would collide with the
	// "unset" kek_id of a row that was never wrapped.
	for _, line := range []string{"x:" + b64Key(1), "0:" + b64Key(1)} {
		if _, err := LoadKEKRing(writeRing(t, line), "", false); err == nil {
			t.Fatalf("accepted bad key id line %q", line)
		}
	}
}

// TestFingerprintIsDomainSeparated keeps the startup log line from doubling as
// a hash an attacker can compare against any other digest of the same bytes.
func TestFingerprintIsDomainSeparated(t *testing.T) {
	raw := b64Key(5)
	ring, err := LoadKEKRing(writeRing(t, "1:"+raw), "", false)
	if err != nil {
		t.Fatalf("LoadKEKRing: %v", err)
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("decoding fixture: %v", err)
	}
	bare := sha256.Sum256(key)
	fp := ring.Fingerprint()
	if len(fp) != 16 {
		t.Fatalf("fingerprint length = %d, want 16", len(fp))
	}
	if fp == hex.EncodeToString(bare[:])[:16] {
		t.Fatal("fingerprint is a bare sha256 of the key")
	}
	want := sha256.Sum256(append([]byte("voicx-kek-fp-v1"), key...))
	if fp != hex.EncodeToString(want[:])[:16] {
		t.Fatalf("fingerprint = %s, want the voicx-kek-fp-v1 prefix", fp)
	}
	if same, _ := LoadKEKRing(writeRing(t, "1:"+raw), "", false); same.Fingerprint() != fp {
		t.Fatal("fingerprint is not stable for identical material")
	}
	// It tracks the NEWEST key, so a rotation is visible in the boot log.
	rotated, err := LoadKEKRing(writeRing(t, "1:"+raw, "2:"+b64Key(11)), "", false)
	if err != nil {
		t.Fatalf("LoadKEKRing(rotated): %v", err)
	}
	if rotated.Fingerprint() == fp {
		t.Fatal("fingerprint did not follow the newest kek")
	}
}

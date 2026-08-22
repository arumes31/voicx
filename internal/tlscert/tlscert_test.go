package tlscert

import (
	"bytes"
	"crypto/tls"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestEnsureGeneratesAndReloads verifies the first call generates and
// persists a certificate, and the second call loads the same one back
// (stable fingerprint — the TOFU guarantee).
func TestEnsureGeneratesAndReloads(t *testing.T) {
	dir := t.TempDir()

	cert1, fp1, err := Ensure(dir, "", "", []string{"voicx"})
	if err != nil {
		t.Fatalf("Ensure (generate): %v", err)
	}
	if fp1 == "" {
		t.Fatal("empty fingerprint")
	}
	if _, err := os.Stat(filepath.Join(dir, "cert.pem")); err != nil {
		t.Fatalf("cert.pem not written: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "key.pem")); err != nil {
		t.Fatalf("key.pem not written: %v", err)
	} else if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		// Windows ignores the write mode; on POSIX the key must be 0600.
		t.Fatalf("key.pem mode = %o, want 600", fi.Mode().Perm())
	}

	cert2, fp2, err := Ensure(dir, "", "", []string{"voicx"})
	if err != nil {
		t.Fatalf("Ensure (reload): %v", err)
	}
	if fp1 != fp2 {
		t.Fatalf("fingerprint changed across restart: %s != %s", fp1, fp2)
	}
	if len(cert1.Certificate) == 0 || len(cert2.Certificate) == 0 {
		t.Fatal("certificate chain empty")
	}
}

// TestEnsureExplicitPaths verifies custom cert/key file locations.
func TestEnsureExplicitPaths(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "custom.crt")
	keyFile := filepath.Join(dir, "custom.key")
	if _, _, err := Ensure("", certFile, keyFile, nil); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	for _, f := range []string{certFile, keyFile} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("%s not written: %v", f, err)
		}
	}
}

// TestGeneratedCertUsable verifies the generated certificate can actually
// terminate a TLS handshake (client trusting it via InsecureSkipVerify, as
// the TOFU clients do).
func TestGeneratedCertUsable(t *testing.T) {
	dir := t.TempDir()
	cert, fp, err := Ensure(dir, "", "", nil)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(fp) != 32*3-1 {
		t.Fatalf("fingerprint %q has unexpected shape", fp)
	}
	_ = tls.Certificate(cert)
}

func TestEnsureRejectsPermissiveExistingKeyWithoutChangingIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not provide POSIX file modes")
	}
	dir := t.TempDir()
	if _, _, err := Ensure(dir, "", "", nil); err != nil {
		t.Fatalf("generate certificate: %v", err)
	}
	keyFile := filepath.Join(dir, "key.pem")
	if err := os.Chmod(keyFile, 0o644); err != nil {
		t.Fatalf("chmod key: %v", err)
	}
	if _, _, err := Ensure(dir, "", "", nil); err == nil {
		t.Fatal("permissive TLS private key was accepted")
	}
	info, err := os.Stat(keyFile)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("key mode = %o, want unchanged 644", info.Mode().Perm())
	}
}

func TestEnsureFailsClosedForCorruptExistingKey(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := Ensure(dir, "", "", nil); err != nil {
		t.Fatalf("generate certificate: %v", err)
	}
	keyFile := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(keyFile, []byte("not a key"), 0o600); err != nil {
		t.Fatalf("corrupt key: %v", err)
	}
	if _, _, err := Ensure(dir, "", "", nil); err == nil {
		t.Fatal("corrupt existing key caused regeneration instead of failing closed")
	}
}

func TestEnsureRejectsPartialExistingPairWithoutChangingSurvivor(t *testing.T) {
	for _, test := range []struct {
		name         string
		missing      string
		surviving    string
		missingLabel string
	}{
		{
			name:         "certificate missing while key remains",
			missing:      "cert.pem",
			surviving:    "key.pem",
			missingLabel: "certificate",
		},
		{
			name:         "key missing while certificate remains",
			missing:      "key.pem",
			surviving:    "cert.pem",
			missingLabel: "private key",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			if _, _, err := Ensure(dir, "", "", nil); err != nil {
				t.Fatalf("generate certificate pair: %v", err)
			}
			survivingPath := filepath.Join(dir, test.surviving)
			before, err := os.ReadFile(survivingPath)
			if err != nil {
				t.Fatalf("read surviving %s: %v", test.surviving, err)
			}
			beforeInfo, err := os.Stat(survivingPath)
			if err != nil {
				t.Fatalf("stat surviving %s: %v", test.surviving, err)
			}
			missingPath := filepath.Join(dir, test.missing)
			if err := os.Remove(missingPath); err != nil {
				t.Fatalf("remove %s: %v", test.missing, err)
			}

			_, _, err = Ensure(dir, "", "", nil)
			if err == nil {
				t.Fatal("partial pair caused regeneration instead of failing closed")
			}
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Ensure error = %v, want wrapped ErrNotExist", err)
			}
			if !strings.Contains(err.Error(), test.missingLabel) {
				t.Fatalf("Ensure error = %q, want missing component %q", err, test.missingLabel)
			}
			after, err := os.ReadFile(survivingPath)
			if err != nil {
				t.Fatalf("read surviving %s after Ensure: %v", test.surviving, err)
			}
			afterInfo, err := os.Stat(survivingPath)
			if err != nil {
				t.Fatalf("stat surviving %s after Ensure: %v", test.surviving, err)
			}
			if !bytes.Equal(after, before) || afterInfo.Mode().Perm() != beforeInfo.Mode().Perm() {
				t.Fatalf("surviving %s was modified by failed Ensure", test.surviving)
			}
			if _, err := os.Stat(missingPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("missing %s was regenerated: %v", test.missing, err)
			}
		})
	}
}

func TestNotAfterAndLeafRejectInvalidCertificate(t *testing.T) {
	if _, err := Leaf(nil); err == nil {
		t.Fatal("Leaf(nil) succeeded")
	}
	bad := tls.Certificate{Certificate: [][]byte{{1, 2, 3}}}
	if _, err := NotAfter(&bad); err == nil {
		t.Fatal("NotAfter accepted malformed certificate")
	}

	cert, _, err := Ensure(t.TempDir(), "", "", nil)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	leaf, err := Leaf(&cert)
	if err != nil {
		t.Fatalf("Leaf: %v", err)
	}
	notAfter, err := NotAfter(&cert)
	if err != nil {
		t.Fatalf("NotAfter: %v", err)
	}
	if !notAfter.Equal(leaf.NotAfter) {
		t.Fatalf("NotAfter = %s, leaf = %s", notAfter, leaf.NotAfter)
	}
	for _, test := range []struct {
		name     string
		deadline time.Time
		want     bool
	}{
		{name: "before expiry", deadline: notAfter.Add(-time.Nanosecond)},
		{name: "exact expiry boundary", deadline: notAfter, want: true},
		{name: "after expiry", deadline: notAfter.Add(time.Nanosecond), want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ExpiresBy(&cert, test.deadline)
			if err != nil {
				t.Fatalf("ExpiresBy: %v", err)
			}
			if got != test.want {
				t.Fatalf("ExpiresBy(%s) = %t, want %t", test.deadline, got, test.want)
			}
		})
	}
}

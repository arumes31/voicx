package tlscert

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"runtime"
	"testing"
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

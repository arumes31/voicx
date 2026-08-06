package store

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewPIICipherRejectsWrongKeyLength(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		size int
	}{
		{name: "empty", size: 0},
		{name: "one byte", size: 1},
		{name: "one byte short", size: 31},
		{name: "one byte long", size: 33},
		{name: "double length", size: 64},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewPIICipher(make([]byte, test.size)); err == nil {
				t.Fatalf("NewPIICipher(%d-byte key) unexpectedly succeeded", test.size)
			}
		})
	}
}

func TestPIICipherRejectsTruncatedAndTamperedCiphertext(t *testing.T) {
	t.Parallel()

	cipher, err := NewPIICipher(make([]byte, 32))
	if err != nil {
		t.Fatalf("NewPIICipher: %v", err)
	}
	aad := piiAAD(42, "email")
	blob, err := cipher.seal("person@example.test", aad)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	shortLength := cipher.aead.NonceSize() + cipher.aead.Overhead() - 1
	tamperedNonce := bytes.Clone(blob)
	tamperedNonce[0] ^= 0xff
	tamperedCiphertext := bytes.Clone(blob)
	tamperedCiphertext[len(tamperedCiphertext)-1] ^= 0xff
	tests := []struct {
		name string
		blob []byte
	}{
		{name: "empty"},
		{name: "below nonce and tag minimum", blob: bytes.Clone(blob[:shortLength])},
		{name: "tampered nonce", blob: tamperedNonce},
		{name: "tampered ciphertext", blob: tamperedCiphertext},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := cipher.open(test.blob, aad); err == nil {
				t.Fatal("open unexpectedly accepted invalid ciphertext")
			}
		})
	}
}

func TestPIICipherUsesFreshNonce(t *testing.T) {
	t.Parallel()

	cipher, err := NewPIICipher(make([]byte, 32))
	if err != nil {
		t.Fatalf("NewPIICipher: %v", err)
	}
	aad := piiAAD(42, "last_ip")
	first, err := cipher.seal("192.0.2.10", aad)
	if err != nil {
		t.Fatalf("first seal: %v", err)
	}
	second, err := cipher.seal("192.0.2.10", aad)
	if err != nil {
		t.Fatalf("second seal: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("identical plaintext and AAD produced identical AES-GCM ciphertext")
	}
}

func TestLoadOrCreatePIICipherRejectsMalformedExistingKey(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "pii.key")
	if err := os.WriteFile(path, make([]byte, 31), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := LoadOrCreatePIICipher(path); err == nil || !strings.Contains(err.Error(), "exactly 32 bytes") {
		t.Fatalf("LoadOrCreatePIICipher() error = %v, want malformed-key error", err)
	}
}

func TestLoadOrCreatePIICipherReloadsSameKey(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "keys", "pii.key")
	first, err := LoadOrCreatePIICipher(path)
	if err != nil {
		t.Fatalf("first LoadOrCreatePIICipher: %v", err)
	}
	second, err := LoadOrCreatePIICipher(path)
	if err != nil {
		t.Fatalf("second LoadOrCreatePIICipher: %v", err)
	}

	aad := piiAAD(9, "email")
	blob, err := first.seal("person@example.test", aad)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	plain, err := second.open(blob, aad)
	if err != nil {
		t.Fatalf("open with reloaded key: %v", err)
	}
	if plain != "person@example.test" {
		t.Fatalf("open with reloaded key = %q, want original plaintext", plain)
	}
}

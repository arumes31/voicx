package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSignFile(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "checksums.txt")
	signaturePath := filepath.Join(dir, "checksums.txt.sig")
	manifest := []byte("# voicx-version: v1.2.3\nabc  client.exe\n")
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	encodedKey := base64.StdEncoding.EncodeToString(privateKey)
	if err := signFile(manifestPath, signaturePath, encodedKey); err != nil {
		t.Fatalf("sign file: %v", err)
	}
	encodedSignature, err := os.ReadFile(signaturePath)
	if err != nil {
		t.Fatalf("read signature: %v", err)
	}
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encodedSignature)))
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if !ed25519.Verify(publicKey, manifest, signature) {
		t.Fatal("signature did not verify")
	}
}

func TestDecodePrivateKeyRejectsMissingAndMalformedKeys(t *testing.T) {
	for _, encoded := range []string{"", "not-base64", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := decodePrivateKey(encoded); err == nil {
			t.Fatalf("decodePrivateKey(%q) succeeded", encoded)
		}
	}
}

func TestGenerateKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-signing-key.txt")
	encodedPublicKey, err := generateKeyFile(path)
	if err != nil {
		t.Fatalf("generate key file: %v", err)
	}
	encodedPrivateKey, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read private key: %v", err)
	}
	privateKey, err := decodePrivateKey(string(encodedPrivateKey))
	if err != nil {
		t.Fatalf("decode generated private key: %v", err)
	}
	publicKey, err := base64.StdEncoding.DecodeString(encodedPublicKey)
	if err != nil {
		t.Fatalf("decode generated public key: %v", err)
	}
	if string(privateKey.Public().(ed25519.PublicKey)) != string(publicKey) {
		t.Fatal("generated public and private keys do not match")
	}
	if _, err := generateKeyFile(path); err == nil {
		t.Fatal("overwrote an existing private key file")
	}
}

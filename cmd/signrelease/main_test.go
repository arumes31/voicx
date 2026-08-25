package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type failingSignatureFile struct {
	*os.File
	writeErr error
	syncErr  error
	closeErr error
}

func (f *failingSignatureFile) Write(data []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.File.Write(data)
}

func (f *failingSignatureFile) Sync() error {
	if f.syncErr != nil {
		return f.syncErr
	}
	return f.File.Sync()
}

func (f *failingSignatureFile) Close() error {
	err := f.File.Close()
	if f.closeErr != nil {
		return f.closeErr
	}
	return err
}

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
	if err := signFile(manifestPath, signaturePath, encodedKey, "v1.2.3"); err != nil {
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
	if runtime.GOOS != "windows" {
		info, err := os.Stat(signaturePath)
		if err != nil {
			t.Fatalf("stat signature: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("signature permissions = %#o, want 0600", got)
		}
	}
}

func TestSignFileRejectsInvalidVersionHeaderWithoutCreatingOutput(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	encodedKey := base64.StdEncoding.EncodeToString(privateKey)
	validManifest := []byte("# voicx-version: v1.2.3\nabc  client.exe\n")

	for _, test := range []struct {
		name      string
		manifest  []byte
		version   string
		wantError string
	}{
		{name: "missing header", manifest: []byte("abc  client.exe\n"), version: "v1.2.3", wantError: "no release version"},
		{name: "header is not first", manifest: []byte("abc  client.exe\n# voicx-version: v1.2.3\n"), version: "v1.2.3", wantError: "must be the first line"},
		{name: "empty header", manifest: []byte("# voicx-version: \nabc  client.exe\n"), version: "v1.2.3", wantError: "release version is empty"},
		{name: "duplicate header", manifest: []byte("# voicx-version: v1.2.3\n# voicx-version: v1.2.3\n"), version: "v1.2.3", wantError: "multiple release versions"},
		{name: "mismatched header", manifest: []byte("# voicx-version: v1.2.4\nabc  client.exe\n"), version: "v1.2.3", wantError: "does not match release"},
		{name: "empty expected version", manifest: validManifest, wantError: "expected release version"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			manifestPath := filepath.Join(dir, "checksums.txt")
			signaturePath := filepath.Join(dir, "checksums.txt.sig")
			if err := os.WriteFile(manifestPath, test.manifest, 0o600); err != nil {
				t.Fatalf("write manifest: %v", err)
			}
			err := signFile(manifestPath, signaturePath, encodedKey, test.version)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("signFile error = %v, want %q", err, test.wantError)
			}
			if _, err := os.Stat(signaturePath); !os.IsNotExist(err) {
				t.Fatalf("invalid manifest created signature file: stat error = %v", err)
			}
		})
	}
}

func TestSignFileDoesNotOverwriteExistingSignature(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "checksums.txt")
	signaturePath := filepath.Join(dir, "checksums.txt.sig")
	if err := os.WriteFile(manifestPath, []byte("# voicx-version: v1.2.3\nabc  client.exe\n"), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	sentinel := []byte("do-not-overwrite")
	if err := os.WriteFile(signaturePath, sentinel, 0o600); err != nil {
		t.Fatalf("write sentinel signature: %v", err)
	}
	err = signFile(manifestPath, signaturePath, base64.StdEncoding.EncodeToString(privateKey), "v1.2.3")
	if err == nil || !strings.Contains(err.Error(), "create signature file") {
		t.Fatalf("signFile error = %v, want exclusive creation error", err)
	}
	got, err := os.ReadFile(signaturePath)
	if err != nil {
		t.Fatalf("read sentinel signature: %v", err)
	}
	if string(got) != string(sentinel) {
		t.Fatalf("signature bytes = %q, want preserved sentinel %q", got, sentinel)
	}
}

func TestSignFileDoesNotFollowExistingSignatureSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows symlink creation requires extra privileges")
	}
	_, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "checksums.txt")
	signaturePath := filepath.Join(dir, "checksums.txt.sig")
	targetPath := filepath.Join(dir, "signature-target.txt")
	if err := os.WriteFile(manifestPath, []byte("# voicx-version: v1.2.3\nabc  client.exe\n"), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	sentinel := []byte("do-not-overwrite-symlink-target")
	if err := os.WriteFile(targetPath, sentinel, 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	if err := os.Symlink(targetPath, signaturePath); err != nil {
		t.Fatalf("create signature symlink: %v", err)
	}
	err = signFile(manifestPath, signaturePath, base64.StdEncoding.EncodeToString(privateKey), "v1.2.3")
	if err == nil || !strings.Contains(err.Error(), "create signature file") {
		t.Fatalf("signFile error = %v, want exclusive creation error", err)
	}
	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read symlink target: %v", err)
	}
	if string(got) != string(sentinel) {
		t.Fatalf("symlink target bytes = %q, want preserved sentinel %q", got, sentinel)
	}
}

func TestWriteSignatureFileRemovesPartialOutputOnFailure(t *testing.T) {
	writeFailure := errors.New("write failed")
	syncFailure := errors.New("sync failed")
	closeFailure := errors.New("close failed")
	for _, test := range []struct {
		name     string
		writeErr error
		syncErr  error
		closeErr error
		want     []error
	}{
		{name: "write", writeErr: writeFailure, want: []error{writeFailure}},
		{name: "sync", syncErr: syncFailure, want: []error{syncFailure}},
		{name: "close", closeErr: closeFailure, want: []error{closeFailure}},
		{name: "sync and close", syncErr: syncFailure, closeErr: closeFailure, want: []error{syncFailure, closeFailure}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "checksums.txt.sig")
			err := writeSignatureFileWithOpener(path, []byte("signature"), func(path string) (signatureFile, error) {
				file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
				if err != nil {
					return nil, err
				}
				return &failingSignatureFile{File: file, writeErr: test.writeErr, syncErr: test.syncErr, closeErr: test.closeErr}, nil
			})
			for _, want := range test.want {
				if !errors.Is(err, want) {
					t.Fatalf("writeSignatureFileWithOpener error = %v, want %v", err, want)
				}
			}
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Fatalf("partial signature remained: stat error = %v", statErr)
			}
		})
	}
}

func TestDecodePrivateKeyRejectsMissingAndMalformedKeys(t *testing.T) {
	for _, encoded := range []string{"", "not-base64", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := decodePrivateKey(encoded); err == nil {
			t.Fatalf("decodePrivateKey(%q) succeeded", encoded)
		}
	}
}

func TestVerifyFile(t *testing.T) {
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
	if err := os.WriteFile(signaturePath, []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, manifest))), 0o600); err != nil {
		t.Fatalf("write signature: %v", err)
	}
	encodedPublicKey := base64.StdEncoding.EncodeToString(publicKey)
	if err := verifyFile(manifestPath, signaturePath, encodedPublicKey, "v1.2.3"); err != nil {
		t.Fatalf("verify file: %v", err)
	}
	if err := verifyFile(manifestPath, signaturePath, encodedPublicKey, "v1.2.4"); err == nil {
		t.Fatal("verify file accepted an incorrect release version")
	}
}

func TestRunVerifyRequiresVersion(t *testing.T) {
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
	if err := os.WriteFile(signaturePath, []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, manifest))), 0o600); err != nil {
		t.Fatalf("write signature: %v", err)
	}
	err = run([]string{
		"-verify",
		"-in", manifestPath,
		"-out", signaturePath,
		"-public-keys", base64.StdEncoding.EncodeToString(publicKey),
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "expected release version") {
		t.Fatalf("run without -version error = %v, want expected version rejection", err)
	}
}

func TestRunSignRequiresVersion(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "checksums.txt")
	signaturePath := filepath.Join(dir, "checksums.txt.sig")
	if err := os.WriteFile(manifestPath, []byte("# voicx-version: v1.2.3\nabc  client.exe\n"), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	t.Setenv(signingKeyEnv, base64.StdEncoding.EncodeToString(privateKey))
	err = run([]string{"-in", manifestPath, "-out", signaturePath}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "expected release version") {
		t.Fatalf("run without -version error = %v, want expected version rejection", err)
	}
	if _, err := os.Stat(signaturePath); !os.IsNotExist(err) {
		t.Fatalf("missing -version created signature file: stat error = %v", err)
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

package updatemanifest

import (
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
)

func TestVerify(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	manifest := []byte(VersionPrefix + "v1.2.3\nabc  client.exe\n")
	keys := base64.StdEncoding.EncodeToString(publicKey)
	otherPublicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate other key: %v", err)
	}
	otherKeys := base64.StdEncoding.EncodeToString(otherPublicKey)
	sign := func(manifest []byte) []byte {
		return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, manifest)))
	}

	tests := []struct {
		name      string
		manifest  []byte
		signature []byte
		keys      string
		version   string
		wantError string
	}{
		{
			name:      "valid manifest",
			manifest:  manifest,
			signature: sign(manifest),
			keys:      keys,
			version:   "v1.2.3",
		},
		{
			name:      "wrong version",
			manifest:  manifest,
			signature: sign(manifest),
			keys:      keys,
			version:   "v1.2.4",
			wantError: "does not match release",
		},
		{
			name:      "tampered manifest",
			manifest:  append([]byte(VersionPrefix+"v1.2.3\nabc  client.ex"), 'f', '\n'),
			signature: sign(manifest),
			keys:      keys,
			version:   "v1.2.3",
			wantError: "not trusted",
		},
		{
			name:      "missing trusted key",
			manifest:  manifest,
			signature: sign(manifest),
			version:   "v1.2.3",
			wantError: "no trusted update signing key",
		},
		{
			name:      "signature from a different valid trusted key is rejected",
			manifest:  manifest,
			signature: sign(manifest),
			keys:      otherKeys,
			version:   "v1.2.3",
			wantError: "not trusted",
		},
		{
			name:      "key rotation accepts any configured trusted key",
			manifest:  manifest,
			signature: sign(manifest),
			keys:      otherKeys + "," + keys,
			version:   "v1.2.3",
		},
		{
			name:      "multiple version lines",
			manifest:  append(append([]byte(nil), manifest...), []byte(VersionPrefix+"v1.2.3\n")...),
			keys:      keys,
			version:   "v1.2.3",
			wantError: "multiple release versions",
		},
		{
			name:      "missing version line",
			manifest:  []byte("abc  client.exe\n"),
			keys:      keys,
			version:   "v1.2.3",
			wantError: "no release version",
		},
		{
			name:      "version line must be first",
			manifest:  []byte("abc  client.exe\n" + VersionPrefix + "v1.2.3\n"),
			keys:      keys,
			version:   "v1.2.3",
			wantError: "must be the first line",
		},
		{
			name:      "empty version line",
			manifest:  []byte(VersionPrefix + "\nabc  client.exe\n"),
			keys:      keys,
			version:   "v1.2.3",
			wantError: "release version is empty",
		},
		{
			name:      "empty expected version",
			manifest:  manifest,
			signature: sign(manifest),
			keys:      keys,
			wantError: "expected release version",
		},
		{
			name:      "malformed trusted key",
			manifest:  manifest,
			signature: sign(manifest),
			keys:      "not-base64",
			version:   "v1.2.3",
			wantError: "decode trusted update signing key",
		},
		{
			name:      "malformed signature",
			manifest:  manifest,
			signature: []byte("not-base64"),
			keys:      keys,
			version:   "v1.2.3",
			wantError: "decode update signature",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			signature := tt.signature
			if signature == nil {
				signature = sign(tt.manifest)
			}
			err := Verify(tt.manifest, signature, tt.keys, tt.version)
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("Verify: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("Verify error = %v, want %q", err, tt.wantError)
			}
		})
	}
}

func TestVerifyRejectsEmptyAndOversizeManifest(t *testing.T) {
	for _, manifest := range [][]byte{
		nil,
		make([]byte, MaxSize+1),
	} {
		if err := Verify(manifest, []byte("signature"), "key", "v1.2.3"); err == nil || !strings.Contains(err.Error(), "manifest size") {
			t.Fatalf("Verify(%d-byte manifest) error = %v, want size rejection", len(manifest), err)
		}
	}
}

package auth

import (
	"crypto/ed25519"
	"encoding/pem"
	"strings"
	"testing"
)

func TestLoadPublicKeyRejectsMalformedPEM(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		encoded string
		wantErr string
	}{
		{name: "empty", wantErr: "decode public key PEM"},
		{name: "not PEM", encoded: "not a pem block", wantErr: "decode public key PEM"},
		{
			name:    "wrong block type",
			encoded: encodeTestPEM("PRIVATE KEY", make([]byte, ed25519.PublicKeySize)),
			wantErr: "unexpected PEM block type",
		},
		{
			name:    "short key",
			encoded: encodeTestPEM("PUBLIC KEY", make([]byte, ed25519.PublicKeySize-1)),
			wantErr: "invalid ed25519 public key length",
		},
		{
			name:    "long key",
			encoded: encodeTestPEM("PUBLIC KEY", make([]byte, ed25519.PublicKeySize+1)),
			wantErr: "invalid ed25519 public key length",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if _, err := LoadPublicKey(test.encoded); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("LoadPublicKey() error = %v, want text %q", err, test.wantErr)
			}
		})
	}
}

func TestLoadPrivateKeyRejectsMalformedPEM(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		encoded string
		wantErr string
	}{
		{name: "empty", wantErr: "decode private key PEM"},
		{name: "not PEM", encoded: "not a pem block", wantErr: "decode private key PEM"},
		{
			name:    "wrong block type",
			encoded: encodeTestPEM("PUBLIC KEY", make([]byte, ed25519.PrivateKeySize)),
			wantErr: "unexpected PEM block type",
		},
		{
			name:    "short key",
			encoded: encodeTestPEM("PRIVATE KEY", make([]byte, ed25519.PrivateKeySize-1)),
			wantErr: "invalid ed25519 private key length",
		},
		{
			name:    "long key",
			encoded: encodeTestPEM("PRIVATE KEY", make([]byte, ed25519.PrivateKeySize+1)),
			wantErr: "invalid ed25519 private key length",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if _, err := LoadPrivateKey(test.encoded); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("LoadPrivateKey() error = %v, want text %q", err, test.wantErr)
			}
		})
	}
}

func TestChallengeOperationsRejectMissingInputs(t *testing.T) {
	t.Parallel()

	publicKey, privateKey, err := GenerateIdentityKeyPair()
	if err != nil {
		t.Fatalf("GenerateIdentityKeyPair: %v", err)
	}

	if _, err := SignChallenge(privateKey, nil); err == nil || !strings.Contains(err.Error(), "challenge must not be empty") {
		t.Fatalf("SignChallenge(nil) error = %v, want empty-challenge error", err)
	}
	if err := VerifyChallenge(publicKey, nil, make([]byte, ed25519.SignatureSize)); err == nil || !strings.Contains(err.Error(), "challenge must not be empty") {
		t.Fatalf("VerifyChallenge(empty challenge) error = %v, want empty-challenge error", err)
	}
	if err := VerifyChallenge(publicKey, []byte("challenge"), nil); err == nil || !strings.Contains(err.Error(), "signature must not be empty") {
		t.Fatalf("VerifyChallenge(empty signature) error = %v, want empty-signature error", err)
	}
}

func encodeTestPEM(blockType string, data []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: data}))
}

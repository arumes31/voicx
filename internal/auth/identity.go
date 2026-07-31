package auth

import (
	"crypto"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
)

// GenerateIdentityKeyPair generates a new Ed25519 key pair and returns the
// public and private keys PEM-encoded as strings suitable for storage in the
// users table.
func GenerateIdentityKeyPair() (publicKeyPEM string, privateKeyPEM string, err error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return "", "", fmt.Errorf("generating ed25519 key pair: %w", err)
	}

	pubBlock := &pem.Block{Type: "PUBLIC KEY", Bytes: pub}
	privBlock := &pem.Block{Type: "PRIVATE KEY", Bytes: priv}

	return string(pem.EncodeToMemory(pubBlock)), string(pem.EncodeToMemory(privBlock)), nil
}

// LoadPublicKey parses a PEM-encoded Ed25519 public key and returns the raw
// ed25519.PublicKey.
func LoadPublicKey(publicKeyPEM string) (ed25519.PublicKey, error) {
	block, _ := pem.Decode([]byte(publicKeyPEM))
	if block == nil {
		return nil, errors.New("failed to decode public key PEM block")
	}
	if block.Type != "PUBLIC KEY" {
		return nil, fmt.Errorf("unexpected PEM block type %q, want %q", block.Type, "PUBLIC KEY")
	}
	if len(block.Bytes) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid ed25519 public key length: got %d, want %d", len(block.Bytes), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(block.Bytes), nil
}

// LoadPrivateKey parses a PEM-encoded Ed25519 private key and returns the raw
// ed25519.PrivateKey.
func LoadPrivateKey(privateKeyPEM string) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return nil, errors.New("failed to decode private key PEM block")
	}
	if block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("unexpected PEM block type %q, want %q", block.Type, "PRIVATE KEY")
	}
	if len(block.Bytes) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid ed25519 private key length: got %d, want %d", len(block.Bytes), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(block.Bytes), nil
}

// UniqueIDFromPublicKey computes a TeamSpeak-style unique ID from a
// PEM-encoded Ed25519 public key.
//
// Algorithm (mirrors TeamSpeak 3's unique identifier derivation):
//  1. Parse the PEM block and extract the raw public key bytes (the DER-encoded
//     SubjectPublicKeyInfo is not used here; instead the raw Ed25519 public key
//     bytes are hashed, which is sufficient and deterministic for our purposes).
//  2. Compute SHA-256 of the raw public key bytes.
//  3. Base64-encode the SHA-256 digest (standard encoding).
//  4. Base64-encode the resulting string again (double base64, as TS3 does a
//     double base64 of the public key's hash).
//
// The final string is suitable for the users.unique_id column.
func UniqueIDFromPublicKey(publicKeyPEM string) (string, error) {
	pub, err := LoadPublicKey(publicKeyPEM)
	if err != nil {
		return "", fmt.Errorf("loading public key: %w", err)
	}

	h := sha256.Sum256(pub)
	first := base64.StdEncoding.EncodeToString(h[:])
	second := base64.StdEncoding.EncodeToString([]byte(first))
	return second, nil
}

// SignChallenge signs the given challenge bytes with the PEM-encoded Ed25519
// private key and returns the signature.
func SignChallenge(privateKeyPEM string, challenge []byte) ([]byte, error) {
	priv, err := LoadPrivateKey(privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("loading private key: %w", err)
	}
	if len(challenge) == 0 {
		return nil, errors.New("challenge must not be empty")
	}
	return ed25519.Sign(priv, challenge), nil
}

// VerifyChallenge verifies an Ed25519 signature over the given challenge using
// the PEM-encoded public key. It returns nil if the signature is valid and an
// error otherwise. Verification is performed by crypto/ed25519 which already
// uses constant-time comparison internally.
func VerifyChallenge(publicKeyPEM string, challenge, signature []byte) error {
	pub, err := LoadPublicKey(publicKeyPEM)
	if err != nil {
		return fmt.Errorf("loading public key: %w", err)
	}
	if len(challenge) == 0 {
		return errors.New("challenge must not be empty")
	}
	if len(signature) == 0 {
		return errors.New("signature must not be empty")
	}
	if !ed25519.Verify(pub, challenge, signature) {
		return errors.New("signature verification failed")
	}
	return nil
}

// Ensure the crypto package is referenced for documentation purposes; the
// ed25519 scheme implements crypto.Signer.
var _ crypto.Signer = (ed25519.PrivateKey)(nil)

// subtle is referenced to document that constant-time comparison is used for
// signature verification via ed25519.Verify.
var _ = subtle.ConstantTimeCompare

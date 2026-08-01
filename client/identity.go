// identity.go implements the client's Ed25519 identity (TS3 model: the
// identity IS the key pair). It is generated on first run and persisted to
// <UserConfigDir>/voicx/identity.json with 0600 permissions.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/nacl/box"

	"voicx/internal/auth"
)

// identity is the client's key material, PEM/base64-encoded: the Ed25519
// signing pair (the TS3-style identity) plus an X25519 pair for E2EE chat
// (wave 4b). The X25519 fields generate automatically on first load of an
// older identity file.
type identity struct {
	PublicKey  string `json:"public_key"`
	PrivateKey string `json:"private_key"`
	// X25519 encryption pair (base64, 32 bytes each) for E2EE direct
	// messages and unsealing channel chat keys.
	X25519Public  string `json:"x25519_public,omitempty"`
	X25519Private string `json:"x25519_private,omitempty"`
}

// x25519 returns the identity's X25519 key pair, decoding the base64 fields.
func (id *identity) x25519() (pub, priv [32]byte, err error) {
	pubRaw, err := base64.StdEncoding.DecodeString(id.X25519Public)
	if err != nil || len(pubRaw) != 32 {
		return pub, priv, fmt.Errorf("invalid x25519 public key in identity")
	}
	privRaw, err := base64.StdEncoding.DecodeString(id.X25519Private)
	if err != nil || len(privRaw) != 32 {
		return pub, priv, fmt.Errorf("invalid x25519 private key in identity")
	}
	copy(pub[:], pubRaw)
	copy(priv[:], privRaw)
	return pub, priv, nil
}

// uniqueID derives the client's unique ID from its public key (stable across
// runs, TS3-style).
func (id *identity) uniqueID() (string, error) {
	return auth.UniqueIDFromPublicKey(id.PublicKey)
}

// identityPath returns the default identity file location.
func identityPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "voicx", "identity.json"), nil
}

// loadOrCreateIdentity loads the identity from the default path, generating
// and persisting a new key pair on first run.
func loadOrCreateIdentity() (*identity, error) {
	path, err := identityPath()
	if err != nil {
		return nil, err
	}
	return loadOrCreateIdentityAt(path)
}

// loadOrCreateIdentityAt is loadOrCreateIdentity with an explicit path
// (tests). Older identity files without X25519 fields are upgraded in place.
func loadOrCreateIdentityAt(path string) (*identity, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		var id identity
		if jerr := json.Unmarshal(data, &id); jerr == nil && id.PublicKey != "" && id.PrivateKey != "" {
			if id.X25519Public == "" || id.X25519Private == "" {
				if err := id.generateX25519(); err != nil {
					return nil, err
				}
				if err := saveIdentityAt(path, &id); err != nil {
					return nil, err
				}
			}
			return &id, nil
		}
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	pubPEM, privPEM, err := auth.GenerateIdentityKeyPair()
	if err != nil {
		return nil, err
	}
	id := &identity{PublicKey: pubPEM, PrivateKey: privPEM}
	if err := id.generateX25519(); err != nil {
		return nil, err
	}
	if err := saveIdentityAt(path, id); err != nil {
		return nil, err
	}
	return id, nil
}

// generateX25519 creates a fresh X25519 pair for E2EE chat.
func (id *identity) generateX25519() error {
	pub, priv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	id.X25519Public = base64.StdEncoding.EncodeToString(pub[:])
	id.X25519Private = base64.StdEncoding.EncodeToString(priv[:])
	return nil
}

// saveIdentityAt persists the identity with owner-only permissions.
func saveIdentityAt(path string, id *identity) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

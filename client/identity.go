// identity.go implements the client's Ed25519 identity (TS3 model: the
// identity IS the key pair). It is generated on first run and persisted to
// <UserConfigDir>/voicx/identity.json with 0600 permissions.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"voicx/internal/auth"
)

// identity is the client's Ed25519 key pair, PEM-encoded.
type identity struct {
	PublicKey  string `json:"public_key"`
	PrivateKey string `json:"private_key"`
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
// (tests).
func loadOrCreateIdentityAt(path string) (*identity, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		var id identity
		if jerr := json.Unmarshal(data, &id); jerr == nil && id.PublicKey != "" && id.PrivateKey != "" {
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

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return nil, err
	}
	return id, nil
}

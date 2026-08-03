// tofu.go implements the client's trust-on-first-use store for control
// channel TLS fingerprints: <UserConfigDir>/voicx/known_servers.json maps
// server address -> SHA-256 certificate fingerprint. The first connection to
// a server is accepted and pinned; a later fingerprint mismatch is a hard
// error (possible MITM) until the user explicitly trusts the new one.
package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// trustStatus is the result of verifying a presented fingerprint against the
// TOFU store.
type trustStatus int

const (
	// trustUnknown means the server address has no pinned fingerprint yet.
	trustUnknown trustStatus = iota
	// trustOK means the presented fingerprint matches the pinned one.
	trustOK
	// trustMismatch means the presented fingerprint differs from the pinned
	// one — possible MITM; connecting must fail.
	trustMismatch
)

// knownServers is a JSON-backed addr -> fingerprint store.
type knownServers struct {
	path    string
	mu      sync.Mutex
	Servers map[string]string `json:"servers"`
}

// knownServersPath returns the default TOFU store location.
func knownServersPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "voicx", "known_servers.json"), nil
}

// loadKnownServersAt loads the store from path; a missing file yields an
// empty store (first run).
func loadKnownServersAt(path string) *knownServers {
	ks := &knownServers{path: path, Servers: map[string]string{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return ks
	}
	_ = json.Unmarshal(data, ks)
	if ks.Servers == nil {
		ks.Servers = map[string]string{}
	}
	return ks
}

// verify compares fp against the pinned fingerprint for addr.
func (k *knownServers) verify(addr, fp string) trustStatus {
	k.mu.Lock()
	defer k.mu.Unlock()
	known, ok := k.Servers[addr]
	if !ok {
		return trustUnknown
	}
	if !secureEqualFold(known, fp) {
		return trustMismatch
	}
	return trustOK
}

// secureEqualFold compares normalized fingerprints without a data-dependent
// early exit. Fingerprints, transfer tokens, and authentication material must
// not use ordinary string equality in verification paths.
func secureEqualFold(a, b string) bool {
	a = strings.ToLower(a)
	b = strings.ToLower(b)
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// trust pins fp for addr and persists the store.
func (k *knownServers) trust(addr, fp string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.Servers[addr] = fp
	if err := os.MkdirAll(filepath.Dir(k.path), 0o750); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(k.path, raw, 0o600)
}

// errFingerprintMismatch is returned when a server's presented certificate
// fingerprint differs from the pinned one (possible MITM).
var errFingerprintMismatch = errors.New("tls fingerprint mismatch")

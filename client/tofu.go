// tofu.go implements the client's trust-on-first-use store for control
// channel TLS fingerprints: <UserConfigDir>/voicx/known_servers.json maps
// server address -> SHA-256 certificate fingerprint. The first connection to
// a server is accepted and pinned; a later fingerprint mismatch is a hard
// error (possible MITM) until the user explicitly trusts the new one.
package main

import (
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	loadErr error
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
	// #nosec G304 -- path is the application-owned config path or an explicit test path.
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ks
	}
	if err != nil {
		ks.loadErr = fmt.Errorf("reading TLS trust store: %w", err)
		return ks
	}
	var persisted struct {
		Servers map[string]string `json:"servers"`
	}
	if err := json.Unmarshal(data, &persisted); err != nil {
		ks.loadErr = fmt.Errorf("parsing TLS trust store: %w", err)
		return ks
	}
	if persisted.Servers != nil {
		ks.Servers = persisted.Servers
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
	if k.loadErr != nil {
		return k.loadErr
	}
	next := make(map[string]string, len(k.Servers)+1)
	for knownAddr, knownFingerprint := range k.Servers {
		next[knownAddr] = knownFingerprint
	}
	next[addr] = fp
	raw, err := json.MarshalIndent(struct {
		Servers map[string]string `json:"servers"`
	}{Servers: next}, "", "  ")
	if err != nil {
		return err
	}
	if err := writeKnownServers(k.path, raw); err != nil {
		return err
	}
	k.Servers = next
	return nil
}

func writeKnownServers(path string, raw []byte) (retErr error) {
	dir := filepath.Dir(path)
	// #nosec G703 -- dir is derived from the application-owned trust-store path.
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create TLS trust-store directory: %w", err)
	}
	// #nosec G304 -- dir is derived from the application-owned trust-store path.
	tmp, err := os.CreateTemp(dir, ".known_servers-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary TLS trust store: %w", err)
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("secure temporary TLS trust store: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		return fmt.Errorf("write temporary TLS trust store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary TLS trust store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary TLS trust store: %w", err)
	}
	// #nosec G703 -- both paths are within the application-owned trust-store directory.
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace TLS trust store: %w", err)
	}
	keep = true
	return nil
}

func normalizeFingerprint(fingerprint string) (string, error) {
	parts := strings.Split(strings.TrimSpace(fingerprint), ":")
	if len(parts) != 32 {
		return "", errors.New("fingerprint must contain 32 colon-separated bytes")
	}
	for _, part := range parts {
		if len(part) != 2 {
			return "", errors.New("fingerprint must contain two hex digits per byte")
		}
		if _, err := hex.DecodeString(part); err != nil {
			return "", errors.New("fingerprint contains non-hexadecimal data")
		}
	}
	return strings.ToLower(strings.Join(parts, ":")), nil
}

// errFingerprintMismatch is returned when a server's presented certificate
// fingerprint differs from the pinned one (possible MITM).
var errFingerprintMismatch = errors.New("tls fingerprint mismatch")

// Package chatcrypto holds the key-encryption-key (KEK) ring that protects
// the per-scope chat keys at rest (91-135, "all msg must be encrypted").
//
// Scope keys are persisted wrapped, and the KEK never lives in the database:
// the threat this buys is precisely DB READ access — dumps, backups, WAL,
// replicas, DBA/hosting access, SQL injection, log leakage. A dump alone is
// therefore inert. It is NOT a defence against a compromised live process.
//
// It is a separate package so the rewrap tooling and the tests can use it
// without importing internal/server.
package chatcrypto

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/crypto/nacl/secretbox"
)

// WrappedLen is the exact size of a wrapped scope key: nonce[24] followed by
// secretbox(key32, KEK).
const WrappedLen = 24 + secretbox.Overhead + 32

// badMaterial is the only message an operator sees when a KEK is not 32 raw
// bytes. There is deliberately no passphrase mode: an unsalted, unstretched
// hash of operator-chosen text is dictionary-recoverable from exactly the
// database dump this package exists to defend against.
const badMaterial = "chat master key must be 32 raw bytes, base64-encoded (generate with: openssl rand -base64 32)"

// KEKRing is the set of key-encryption keys, newest last. Wraps always use
// the newest; unwraps dispatch on the id stored beside each wrapped key, so
// rotation is append-only and never rewrites history.
type KEKRing struct {
	keys   map[uint16][32]byte
	newest uint16
}

// LoadKEKRing loads the ring from envB64 when it is non-empty, otherwise from
// path. allowCreate must be true ONLY when the database holds zero scope key
// rows: silently minting a fresh KEK against existing rows would orphan every
// stored message, so a missing ring is otherwise a fatal startup error.
func LoadKEKRing(path, envB64 string, allowCreate bool) (*KEKRing, error) {
	if strings.TrimSpace(envB64) != "" {
		return parseRing(strings.NewReader(envB64), "VOICX_CHAT_MASTER_KEY")
	}
	if path == "" {
		return nil, errors.New("chat master key: chat_master_key_file is empty and VOICX_CHAT_MASTER_KEY is unset")
	}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		return parseRing(bytes.NewReader(raw), path)
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("reading chat master key %s: %w", path, err)
	case !allowCreate:
		return nil, fmt.Errorf("chat master key missing: stored scope key generations cannot be unwrapped; "+
			"restore %s (or set VOICX_CHAT_MASTER_KEY) — it is backed up with the database; "+
			"to abandon all chat history instead, run with --reset-chat-keys", path)
	}
	return createRing(path)
}

// parseRing reads "<decimal id>:<base64 32 raw bytes>" lines. A bare base64
// value with no "id:" prefix is accepted as id 1 so a single env var stays
// copy-pasteable.
func parseRing(r io.Reader, origin string) (*KEKRing, error) {
	ring := &KEKRing{keys: make(map[uint16][32]byte)}
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		id := uint16(1)
		body := text
		if i := strings.Index(text, ":"); i >= 0 {
			n, err := strconv.ParseUint(strings.TrimSpace(text[:i]), 10, 16)
			if err != nil || n == 0 {
				return nil, fmt.Errorf("chat master key %s line %d: key id must be a positive 16-bit integer", origin, line)
			}
			id = uint16(n)
			body = strings.TrimSpace(text[i+1:])
		}
		key, err := decodeKey(body)
		if err != nil {
			return nil, fmt.Errorf("chat master key %s line %d: %w", origin, line, err)
		}
		if _, dup := ring.keys[id]; dup {
			return nil, fmt.Errorf("chat master key %s line %d: key id %d appears twice", origin, line, id)
		}
		ring.keys[id] = key
		if id > ring.newest {
			ring.newest = id
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading chat master key %s: %w", origin, err)
	}
	if len(ring.keys) == 0 {
		return nil, fmt.Errorf("chat master key %s holds no keys", origin)
	}
	return ring, nil
}

func decodeKey(b64 string) ([32]byte, error) {
	var key [32]byte
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) != 32 {
		return key, errors.New(badMaterial)
	}
	copy(key[:], raw)
	return key, nil
}

// createRing writes a fresh single-key ring at mode 0600.
func createRing(path string) (*KEKRing, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("creating chat master key directory %s: %w", dir, err)
		}
	}
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, fmt.Errorf("generating chat master key: %w", err)
	}
	line := "1:" + base64.StdEncoding.EncodeToString(key[:]) + "\n"
	// O_EXCL so a concurrent starter cannot lose its key to ours.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("creating chat master key %s: %w", path, err)
	}
	if _, err := f.WriteString(line); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("writing chat master key %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("syncing chat master key %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("writing chat master key %s: %w", path, err)
	}
	dirPath := filepath.Dir(path)
	if dirPath == "" {
		dirPath = "."
	}
	if d, err := os.Open(dirPath); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return &KEKRing{keys: map[uint16][32]byte{1: key}, newest: 1}, nil
}

// Wrap seals a scope key under the newest KEK and returns that KEK's id.
func (r *KEKRing) Wrap(key [32]byte) (uint16, []byte, error) {
	kek, ok := r.keys[r.newest]
	if !ok {
		return 0, nil, errors.New("chat master key ring is empty")
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return 0, nil, fmt.Errorf("generating wrap nonce: %w", err)
	}
	return r.newest, secretbox.Seal(nonce[:24:24], key[:], &nonce, &kek), nil
}

// Unwrap recovers a scope key wrapped under the KEK named by kekID.
func (r *KEKRing) Unwrap(kekID uint16, wrapped []byte) ([32]byte, error) {
	var out [32]byte
	kek, ok := r.keys[kekID]
	if !ok {
		return out, fmt.Errorf("chat master key %d is not in the ring", kekID)
	}
	if len(wrapped) != WrappedLen {
		return out, fmt.Errorf("wrapped scope key is %d bytes, want %d", len(wrapped), WrappedLen)
	}
	var nonce [24]byte
	copy(nonce[:], wrapped[:24])
	plain, ok := secretbox.Open(nil, wrapped[24:], &nonce, &kek)
	if !ok {
		return out, errors.New("unwrapping scope key failed (wrong chat master key?)")
	}
	copy(out[:], plain)
	return out, nil
}

// NewestID is the KEK id new wraps use.
func (r *KEKRing) NewestID() uint16 { return r.newest }

// IDs returns every KEK id in the ring, unsorted (rewrap tooling).
func (r *KEKRing) IDs() []uint16 {
	out := make([]uint16, 0, len(r.keys))
	for id := range r.keys {
		out = append(out, id)
	}
	return out
}

// Fingerprint identifies the newest KEK for startup logs. It is domain-
// separated so it can never collide with any other hash of the same bytes,
// and it is NOT exposed on /health or any unauthenticated surface — it is an
// oracle over the master key, however weak.
func (r *KEKRing) Fingerprint() string {
	kek := r.keys[r.newest]
	sum := sha256.Sum256(append([]byte("voicx-kek-fp-v1"), kek[:]...))
	return hex.EncodeToString(sum[:])[:16]
}

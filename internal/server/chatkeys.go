// chatkeys.go implements the server-side chat key manager: per-scope
// symmetric chat keys (channel IDs, 0 = global), their persistence as wrapped
// generations, sealed delivery to members, and rotation on member leave.
//
// Trust model (documented in README): the server HOLDS the channel/global
// keys because moderation (114-119) runs on the decrypted body at send time.
// Keys are distributed sealed with nacl/box (SealAnonymous) to each member's
// X25519 public key. Direct messages never touch this manager: they are true
// E2EE (sender seals for the recipient's public key; the server relays
// ciphertext it cannot read).
//
// What ciphertext at rest buys (91) is DB READ access — dumps, backups, WAL,
// replicas, DBA/hosting access, SQL injection, log leakage — and explicitly
// NOT live process compromise, which still sees every channel message while
// it is moderated.
//
// Entitlement: a current member of a scope may fetch EVERY generation of that
// scope. That is not a widening — the server hands the same member the full
// scrollback anyway — but it means rotation on leave is NOT forward secrecy
// over history: it stops an ex-member reading FUTURE messages and nothing
// more. The shield tooltip says so out loud.
//
// Key material is never stored bare: each generation is wrapped with the KEK
// ring (internal/chatcrypto), whose file lives outside the database. Losing
// it is unrecoverable by design, so the server refuses to start rather than
// mint a fresh ring over existing generations.
package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"

	"go.uber.org/zap"
	"golang.org/x/crypto/nacl/box"
	"golang.org/x/crypto/nacl/secretbox"

	"voicx/internal/chatcrypto"
	"voicx/internal/netproto"
)

// globalChatScope is the scope ID of the server-wide (global chat) key.
const globalChatScope int64 = 0

// maxArchivalGenerations caps the per-scope in-memory cache of retired
// generations. Beyond it the oldest cached generation is dropped and re-read
// from the store on demand; nothing is ever deleted from the database.
const maxArchivalGenerations = 64

// ErrNoScopeKey reports that a scope has no key generation yet. It is
// deliberately distinct from a lookup failure: callers on the send path turn
// it into "rejoin the channel" rather than minting, because minting from a
// lookup would let an attacker-supplied channel id fill the disk (91).
var ErrNoScopeKey = errors.New("no chat key generation for this scope")

// errChatKeysUnconfigured reports that persistence is not wired. Chat is then
// unavailable rather than silently RAM-only: with ciphertext at rest a key
// that does not survive a restart is unrecoverable corruption.
var errChatKeysUnconfigured = errors.New("chat key persistence is not configured")

// errInvalidPublicKey is returned when a published key is not 32-byte base64.
var errInvalidPublicKey = &chatKeyError{"invalid X25519 public key (want 32-byte base64)"}

type chatKeyError struct{ msg string }

func (e *chatKeyError) Error() string { return e.msg }

// scopeEntry caches one scope's current generation plus recently touched
// archival generations. Its mutex guards this scope's DB I/O, so two scopes
// never contend and one slow scope cannot stall the rest.
type scopeEntry struct {
	mu     sync.Mutex
	loaded bool
	curID  uint32
	curKey [32]byte
	old    map[uint32][32]byte
	oldLRU []uint32
}

// chatKeyManager generates, persists and rotates per-scope chat keys.
type chatKeyManager struct {
	// mu guards the scopes map lookup ONLY. It is never held across DB I/O.
	mu     sync.Mutex
	scopes map[int64]*scopeEntry

	store  ScopeKeyStore
	kek    *chatcrypto.KEKRing
	logger *zap.Logger
}

func newChatKeyManager(st ScopeKeyStore, kek *chatcrypto.KEKRing, logger *zap.Logger) *chatKeyManager {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &chatKeyManager{scopes: make(map[int64]*scopeEntry), store: st, kek: kek, logger: logger}
}

// configured reports whether key persistence is wired.
func (m *chatKeyManager) configured() bool {
	return m != nil && m.store != nil && m.kek != nil
}

// entry finds or creates a scope's cache entry. Only the map lookup is done
// under the manager lock.
func (m *chatKeyManager) entry(scope int64) *scopeEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.scopes[scope]
	if !ok {
		e = &scopeEntry{old: map[uint32][32]byte{}}
		m.scopes[scope] = e
	}
	return e
}

// loadLocked populates the entry's current generation from the store. It must
// be called with e.mu held. A scope with no generation loads with curID 0.
func (m *chatKeyManager) loadLocked(ctx context.Context, e *scopeEntry, scope int64) error {
	if e.loaded {
		return nil
	}
	if !m.configured() {
		return errChatKeysUnconfigured
	}
	row, err := m.store.CurrentScopeKey(ctx, scope)
	if err != nil {
		return fmt.Errorf("loading scope key: %w", err)
	}
	if row == nil {
		e.loaded = true
		return nil
	}
	key, err := m.kek.Unwrap(row.KEKID, row.Wrapped)
	if err != nil {
		return fmt.Errorf("unwrapping scope %d generation %d: %w", scope, row.KeyID, err)
	}
	e.curID, e.curKey, e.loaded = row.KeyID, key, true
	return nil
}

// mintLocked allocates a fresh generation id from the database and inserts a
// new random key under it. It must be called with e.mu held.
func (m *chatKeyManager) mintLocked(ctx context.Context, e *scopeEntry, scope int64) (uint32, [32]byte, error) {
	var zero [32]byte
	if !m.configured() {
		return 0, zero, errChatKeysUnconfigured
	}
	id, err := m.store.AllocScopeKeyID(ctx, scope)
	if err != nil {
		return 0, zero, fmt.Errorf("allocating scope key id: %w", err)
	}
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		// With persistence an all-zero key is unrecoverable corruption, so a
		// crypto/rand failure is surfaced instead of swallowed.
		return 0, zero, fmt.Errorf("generating scope key: %w", err)
	}
	kekID, wrapped, err := m.kek.Wrap(key)
	if err != nil {
		return 0, zero, fmt.Errorf("wrapping scope key: %w", err)
	}
	if err := m.store.InsertScopeKey(ctx, scope, id, wrapped, kekID); err != nil {
		return 0, zero, fmt.Errorf("persisting scope key: %w", err)
	}
	e.curID, e.curKey, e.loaded = id, key, true
	return id, key, nil
}

// cacheOldLocked records a retired generation in the archival cache. It must
// be called with e.mu held.
func (m *chatKeyManager) cacheOldLocked(e *scopeEntry, id uint32, key [32]byte) {
	if id == 0 {
		return
	}
	if _, ok := e.old[id]; !ok {
		e.oldLRU = append(e.oldLRU, id)
	}
	e.old[id] = key
	for len(e.oldLRU) > maxArchivalGenerations {
		delete(e.old, e.oldLRU[0])
		e.oldLRU = e.oldLRU[1:]
	}
}

// EnsureScope returns the scope's current generation, MINTING generation 1
// when the scope has none. It is the ONLY minting entry point, and callers
// must have established that the client belongs to the scope (or that the
// scope is the fixed global one) before reaching it.
func (m *chatKeyManager) EnsureScope(ctx context.Context, scope int64) (uint32, [32]byte, error) {
	var zero [32]byte
	if m == nil {
		return 0, zero, errChatKeysUnconfigured
	}
	e := m.entry(scope)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := m.loadLocked(ctx, e, scope); err != nil {
		return 0, zero, err
	}
	if e.curID != 0 {
		return e.curID, e.curKey, nil
	}
	return m.mintLocked(ctx, e, scope)
}

// current returns the scope's current generation and NEVER mints. It returns
// ErrNoScopeKey when the scope has no generation.
func (m *chatKeyManager) current(ctx context.Context, scope int64) (uint32, [32]byte, error) {
	var zero [32]byte
	if m == nil {
		return 0, zero, errChatKeysUnconfigured
	}
	e := m.entry(scope)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := m.loadLocked(ctx, e, scope); err != nil {
		return 0, zero, err
	}
	if e.curID == 0 {
		return 0, zero, ErrNoScopeKey
	}
	return e.curID, e.curKey, nil
}

// at returns one specific generation of a scope, reading and unwrapping it
// from the store when it is not cached. It never mints.
func (m *chatKeyManager) at(ctx context.Context, scope int64, gen uint32) ([32]byte, error) {
	var zero [32]byte
	if m == nil {
		return zero, errChatKeysUnconfigured
	}
	if gen == 0 {
		return zero, ErrNoScopeKey
	}
	e := m.entry(scope)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := m.loadLocked(ctx, e, scope); err != nil {
		return zero, err
	}
	if e.curID == gen {
		return e.curKey, nil
	}
	if k, ok := e.old[gen]; ok {
		return k, nil
	}
	if !m.configured() {
		return zero, errChatKeysUnconfigured
	}
	row, err := m.store.GetScopeKey(ctx, scope, gen)
	if err != nil {
		return zero, fmt.Errorf("loading scope key generation: %w", err)
	}
	if row == nil {
		return zero, ErrNoScopeKey
	}
	key, err := m.kek.Unwrap(row.KEKID, row.Wrapped)
	if err != nil {
		return zero, fmt.Errorf("unwrapping scope %d generation %d: %w", scope, gen, err)
	}
	m.cacheOldLocked(e, gen, key)
	return key, nil
}

// rotate replaces the scope's key with a fresh generation and returns it. The
// generation id comes from the database sequence, never from memory: deriving
// it from an unloaded in-RAM map would re-mint a colliding id with different
// key material and orphan every message under the real one.
//
// Retired generations are NEVER deleted — a message referencing a deleted
// generation would be silently and permanently unreadable.
//
// The caller MUST NOT hold any manager lock while redistributing the result:
// deliverScopeKey -> sealFor -> at re-enters this scope's mutex.
func (m *chatKeyManager) rotate(ctx context.Context, scope int64) (uint32, [32]byte, error) {
	var zero [32]byte
	if m == nil {
		return 0, zero, errChatKeysUnconfigured
	}
	e := m.entry(scope)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := m.loadLocked(ctx, e, scope); err != nil {
		return 0, zero, err
	}
	if e.curID == 0 {
		// Nothing to retire yet: the first generation is a plain mint.
		return m.mintLocked(ctx, e, scope)
	}
	id, err := m.store.AllocScopeKeyID(ctx, scope)
	if err != nil {
		return 0, zero, fmt.Errorf("allocating scope key id: %w", err)
	}
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return 0, zero, fmt.Errorf("generating scope key: %w", err)
	}
	kekID, wrapped, err := m.kek.Wrap(key)
	if err != nil {
		return 0, zero, fmt.Errorf("wrapping scope key: %w", err)
	}
	if err := m.store.RotateScopeKey(ctx, scope, id, wrapped, kekID); err != nil {
		return 0, zero, fmt.Errorf("rotating scope key: %w", err)
	}
	m.cacheOldLocked(e, e.curID, e.curKey)
	e.curID, e.curKey = id, key
	return id, key, nil
}

// sealFor seals one generation of a scope for a member's X25519 public key
// (base64) using box.SealAnonymous, producing a ChannelKey message.
func (m *chatKeyManager) sealFor(ctx context.Context, scope int64, gen uint32, memberPubB64 string) (*netproto.ChannelKey, error) {
	pubRaw, err := base64.StdEncoding.DecodeString(memberPubB64)
	if err != nil || len(pubRaw) != 32 {
		return nil, errInvalidPublicKey
	}
	var pub [32]byte
	copy(pub[:], pubRaw)

	key, err := m.at(ctx, scope, gen)
	if err != nil {
		return nil, err
	}
	// SealAnonymous generates an ephemeral sender keypair internally and
	// prepends its public key, so the client needs no server key to open.
	sealed, err := box.SealAnonymous(nil, key[:], &pub, rand.Reader)
	if err != nil {
		return nil, err
	}
	return &netproto.ChannelKey{
		ChannelID: scope,
		KeyID:     gen,
		SealedKey: base64.StdEncoding.EncodeToString(sealed),
	}, nil
}

// open decrypts a scope-encrypted body (base64 nonce‖secretbox) with an
// explicit generation. Taking the generation rather than assuming "current"
// is what lets an edit or a history entry under a rotated key still open.
func (m *chatKeyManager) open(ctx context.Context, scope int64, gen uint32, blobB64 string) (string, error) {
	key, err := m.at(ctx, scope, gen)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(blobB64)
	if err != nil || len(raw) < 24 {
		return "", errors.New("invalid scope ciphertext")
	}
	var nonce [24]byte
	copy(nonce[:], raw[:24])
	plain, ok := secretbox.Open(nil, raw[24:], &nonce, &key)
	if !ok {
		return "", errors.New("scope decryption failed")
	}
	return string(plain), nil
}

// seal encrypts plaintext with the scope's current generation. It never
// mints; callers that may face a fresh scope call EnsureScope first.
func (m *chatKeyManager) seal(ctx context.Context, scope int64, plain string) (uint32, string, error) {
	id, key, err := m.current(ctx, scope)
	if err != nil {
		return 0, "", err
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		// A fixed zero nonce reused across a whole scope breaks secretbox
		// outright, so this failure is surfaced rather than ignored.
		return 0, "", fmt.Errorf("generating nonce: %w", err)
	}
	sealed := secretbox.Seal(nil, []byte(plain), &nonce, &key)
	return id, base64.StdEncoding.EncodeToString(append(nonce[:], sealed...)), nil
}

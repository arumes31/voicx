// chatkeys.go implements the server-side chat key manager (wave 4b): per-
// scope symmetric chat keys (channel IDs, 0 = global), their sealed delivery
// to members, and rotation on member leave.
//
// Trust model (documented in README): the server HOLDS the channel/global
// keys — wave 5 needs server-side history, search, and moderation. Keys are
// distributed sealed with nacl/box (SealAnonymous) to each member's X25519
// public key, so the distribution channel is encrypted even though the
// server is a key holder. Direct messages never touch this manager: they are
// true E2EE (sender seals for the recipient's public key; the server relays
// ciphertext it cannot read).
package server

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"

	"golang.org/x/crypto/nacl/box"
	"golang.org/x/crypto/nacl/secretbox"

	"voicx/internal/netproto"
)

// globalChatScope is the scope ID of the server-wide (global chat) key.
const globalChatScope int64 = 0

// scopeChatKey is one generation of a scope's symmetric chat key.
type scopeChatKey struct {
	id  uint32
	key [32]byte
}

// chatKeyManager generates and rotates per-scope chat keys. Keys live in
// memory only: a server restart regenerates them (clients re-key on
// reconnect), which also bounds history readability to the server's uptime.
type chatKeyManager struct {
	mu   sync.Mutex
	keys map[int64]*scopeChatKey
}

func newChatKeyManager() *chatKeyManager {
	return &chatKeyManager{keys: make(map[int64]*scopeChatKey)}
}

// current returns the scope's current key, generating the first generation
// lazily.
func (m *chatKeyManager) current(scope int64) (uint32, [32]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[scope]
	if !ok {
		k = &scopeChatKey{id: 1}
		if _, err := rand.Read(k.key[:]); err != nil {
			// crypto/rand failure is fatal for key generation; return a
			// zero key only in the impossible case and let callers log.
			return 0, k.key
		}
		m.keys[scope] = k
	}
	return k.id, k.key
}

// rotate replaces the scope's key with a fresh generation and returns it.
// The old key id is invalidated; ex-members holding it cannot read new
// messages (they keep access to history — documented limitation).
func (m *chatKeyManager) rotate(scope int64) (uint32, [32]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.keys[scope]
	k := &scopeChatKey{id: 1}
	if prev != nil {
		k.id = prev.id + 1
	}
	_, _ = rand.Read(k.key[:])
	m.keys[scope] = k
	return k.id, k.key
}

// sealFor seals the scope's current key for a member's X25519 public key
// (base64) using box.SealAnonymous, producing a ChannelKey message.
func (m *chatKeyManager) sealFor(scope int64, memberPubB64 string) (*netproto.ChannelKey, error) {
	pubRaw, err := base64.StdEncoding.DecodeString(memberPubB64)
	if err != nil || len(pubRaw) != 32 {
		return nil, errInvalidPublicKey
	}
	var pub [32]byte
	copy(pub[:], pubRaw)

	id, key := m.current(scope)
	// SealAnonymous generates an ephemeral sender keypair internally and
	// prepends its public key, so the client needs no server key to open.
	sealed, err := box.SealAnonymous(nil, key[:], &pub, rand.Reader)
	if err != nil {
		return nil, err
	}
	return &netproto.ChannelKey{
		ChannelID: scope,
		KeyID:     id,
		SealedKey: base64.StdEncoding.EncodeToString(sealed),
	}, nil
}

// errInvalidPublicKey is returned when a published key is not 32-byte base64.
var errInvalidPublicKey = &chatKeyError{"invalid X25519 public key (want 32-byte base64)"}

type chatKeyError struct{ msg string }

func (e *chatKeyError) Error() string { return e.msg }

// open decrypts a scope-encrypted message body (base64 nonce‖secretbox)
// with the scope's CURRENT key. The caller validates the key id first, so
// current is always the right generation for moderation.
func (m *chatKeyManager) open(scope int64, blobB64 string) (string, error) {
	_, key := m.current(scope)
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

// seal encrypts plaintext with the scope's current key (used for chat_edited
// broadcasts so the wire format stays uniform). It returns the current key
// id and the base64 ciphertext.
func (m *chatKeyManager) seal(scope int64, plain string) (uint32, string) {
	id, key := m.current(scope)
	var nonce [24]byte
	_, _ = rand.Read(nonce[:])
	sealed := secretbox.Seal(nil, []byte(plain), &nonce, &key)
	return id, base64.StdEncoding.EncodeToString(append(nonce[:], sealed...))
}

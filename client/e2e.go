// e2e.go implements the client half of chat encryption (wave 4b): the X25519
// key directory cache, the per-scope channel key store, and payload
// seal/open. The frontend stays plaintext internally — all crypto happens
// here, in the Go backend.
//
// Wire format (base64 string in ChatSend/ChatBroadcast.text):
//   - direct messages: base64(nonce[24] || box.Seal(text, recipientPub, senderPriv))
//     — true E2EE; key_id = 0.
//   - channel/global:  base64(nonce[24] || secretbox.Seal(text, scopeKey))
//     — server-held keys, distributed sealed via MsgChannelKey; key_id = generation.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/crypto/nacl/box"
	"golang.org/x/crypto/nacl/secretbox"

	"voicx/internal/netproto"
)

// missingKeyText replaces ciphertext when the required key is unknown
// (e.g. history from before a key rotation, or an old client).
const missingKeyText = "[encrypted message — missing key]"

// newClientMsgID returns a client-generated message reference for DM
// receipts (124): 8 random bytes, hex-encoded.
func newClientMsgID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x", b[:])
}

// --- key material ------------------------------------------------------------

// pubKeyCache caches X25519 public keys by unique ID.
type pubKeyCache struct {
	mu   sync.Mutex
	keys map[string][32]byte
}

func newPubKeyCache() *pubKeyCache { return &pubKeyCache{keys: map[string][32]byte{}} }

func (c *pubKeyCache) get(uid string) ([32]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k, ok := c.keys[uid]
	return k, ok
}

func (c *pubKeyCache) put(uid string, key [32]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keys[uid] = key
}

// scopeKeyStore keeps the chat keys per scope (channelID, 0 = global) and
// generation.
type scopeKeyStore struct {
	mu     sync.Mutex
	keys   map[int64]map[uint32][32]byte
	latest map[int64]uint32
}

func newScopeKeyStore() *scopeKeyStore {
	return &scopeKeyStore{keys: map[int64]map[uint32][32]byte{}, latest: map[int64]uint32{}}
}

func (s *scopeKeyStore) put(scope int64, id uint32, key [32]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys[scope] == nil {
		s.keys[scope] = map[uint32][32]byte{}
	}
	s.keys[scope][id] = key
	s.latest[scope] = id
}

func (s *scopeKeyStore) get(scope int64, id uint32) ([32]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[scope][id]
	return k, ok
}

func (s *scopeKeyStore) current(scope int64) (uint32, [32]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.latest[scope]
	if !ok {
		return 0, [32]byte{}, false
	}
	k, ok := s.keys[scope][id]
	return id, k, ok
}

// --- seal / open -------------------------------------------------------------

// sealDM encrypts a direct message for recipientPub (true E2EE).
func sealDM(text string, recipientPub, senderPriv [32]byte) (string, error) {
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	sealed := box.Seal(nil, []byte(text), &nonce, &recipientPub, &senderPriv)
	blob := append(nonce[:], sealed...)
	return base64.StdEncoding.EncodeToString(blob), nil
}

// openDM decrypts a direct message with the recipient's private key and the
// sender's public key.
func openDM(blobB64 string, senderPub, recipientPriv [32]byte) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(blobB64)
	if err != nil || len(raw) < 24 {
		return "", errors.New("invalid DM ciphertext")
	}
	var nonce [24]byte
	copy(nonce[:], raw[:24])
	plain, ok := box.Open(nil, raw[24:], &nonce, &senderPub, &recipientPriv)
	if !ok {
		return "", errors.New("DM decryption failed (wrong key or tampered)")
	}
	return string(plain), nil
}

// sealScope encrypts a channel/global message with the scope key.
func sealScope(text string, key [32]byte) (string, error) {
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	sealed := secretbox.Seal(nil, []byte(text), &nonce, &key)
	blob := append(nonce[:], sealed...)
	return base64.StdEncoding.EncodeToString(blob), nil
}

// openScope decrypts a channel/global message with the scope key.
func openScope(blobB64 string, key [32]byte) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(blobB64)
	if err != nil || len(raw) < 24 {
		return "", errors.New("invalid scope ciphertext")
	}
	var nonce [24]byte
	copy(nonce[:], raw[:24])
	plain, ok := secretbox.Open(nil, raw[24:], &nonce, &key)
	if !ok {
		return "", errors.New("scope decryption failed (wrong key or tampered)")
	}
	return string(plain), nil
}

// --- connManager integration -------------------------------------------------

// publishE2EKey publishes the client's X25519 public key after auth. The
// server answers with the sealed global key (and the current channel's key
// when in one), delivered asynchronously as MsgChannelKey frames.
func (m *connManager) publishE2EKey() error {
	id, err := m.identity()
	if err != nil {
		return err
	}
	pub, _, err := id.x25519()
	if err != nil {
		return err
	}
	return m.write(netproto.MsgKeyPublish, netproto.KeyPublish{
		PublicKey: base64.StdEncoding.EncodeToString(pub[:]),
	})
}

// handleChannelKey unseals a scope key delivered by the server.
func (m *connManager) handleChannelKey(f *netproto.Frame) {
	var ck netproto.ChannelKey
	if err := netproto.Decode(f, &ck); err != nil {
		return
	}
	id, err := m.identity()
	if err != nil {
		return
	}
	pub, priv, err := id.x25519()
	if err != nil {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(ck.SealedKey)
	if err != nil {
		return
	}
	key, ok := box.OpenAnonymous(nil, raw, &pub, &priv)
	if !ok || len(key) != 32 {
		m.emit("servererror", "failed to unseal chat key (channel "+fmt.Sprint(ck.ChannelID)+")")
		return
	}
	var k [32]byte
	copy(k[:], key)
	m.scopeKeys.put(ck.ChannelID, ck.KeyID, k)
}

// peerPubKey resolves a user's X25519 public key: cache first, then the
// server directory. Returns ok=false when the user never published one.
func (m *connManager) peerPubKey(uniqueID string) ([32]byte, bool) {
	if k, ok := m.pubKeys.get(uniqueID); ok {
		return k, true
	}
	f, err := m.request(netproto.MsgKeyRequest, netproto.MsgKeyResponse,
		netproto.KeyRequest{UniqueID: uniqueID}, 5*time.Second)
	if err != nil {
		return [32]byte{}, false
	}
	var resp netproto.KeyResponse
	if err := netproto.Decode(f, &resp); err != nil || resp.PublicKey == "" {
		return [32]byte{}, false
	}
	raw, err := base64.StdEncoding.DecodeString(resp.PublicKey)
	if err != nil || len(raw) != 32 {
		return [32]byte{}, false
	}
	var k [32]byte
	copy(k[:], raw)
	m.pubKeys.put(uniqueID, k)
	return k, true
}

// encryptChat seals an outgoing chat message for the scope. It returns the
// ChatSend to transmit, or an error the UI can display (missing keys).
func (m *connManager) encryptChat(scope, target, text string) (netproto.ChatSend, error) {
	id, err := m.identity()
	if err != nil {
		return netproto.ChatSend{}, err
	}
	_, priv, err := id.x25519()
	if err != nil {
		return netproto.ChatSend{}, err
	}

	switch scope {
	case "direct":
		uniqueID := target
		peer, ok := m.peerPubKey(uniqueID)
		if !ok {
			return netproto.ChatSend{}, fmt.Errorf("no encryption key for %s (old client?)", uniqueID)
		}
		blob, err := sealDM(text, peer, priv)
		if err != nil {
			return netproto.ChatSend{}, err
		}
		// (124) client-generated ref for delivery/read receipts.
		return netproto.ChatSend{ToUniqueID: uniqueID, Text: blob, Enc: true, ClientMsgID: newClientMsgID()}, nil

	case "channel":
		var channelID int64
		if _, err := fmt.Sscan(target, &channelID); err != nil {
			return netproto.ChatSend{}, fmt.Errorf("invalid channel id %q", target)
		}
		keyID, key, ok := m.scopeKeys.current(channelID)
		if !ok {
			return netproto.ChatSend{}, fmt.Errorf("no chat key for this channel yet (rejoin or wait for re-key)")
		}
		blob, err := sealScope(text, key)
		if err != nil {
			return netproto.ChatSend{}, err
		}
		return netproto.ChatSend{ChannelID: target, Text: blob, Enc: true, KeyID: keyID}, nil

	default: // global
		keyID, key, ok := m.scopeKeys.current(0)
		if !ok {
			return netproto.ChatSend{}, fmt.Errorf("no global chat key yet (wait for key delivery)")
		}
		blob, err := sealScope(text, key)
		if err != nil {
			return netproto.ChatSend{}, err
		}
		return netproto.ChatSend{Text: blob, Enc: true, KeyID: keyID}, nil
	}
}

// maybeDecryptChat inspects a broadcast event envelope; chat events with
// enc=true are decrypted in place (or replaced with missingKeyText).
// Everything else passes through unchanged.
func (m *connManager) maybeDecryptChat(payload string) string {
	var env struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(payload), &env); err != nil || env.Type != "chat" {
		return payload
	}
	var chat netproto.ChatBroadcast
	if err := json.Unmarshal(env.Data, &chat); err != nil || !chat.Enc {
		return payload
	}

	if chat.E2E || chat.KeyID == 0 {
		// True E2EE DM: needs the sender's public key — resolved
		// asynchronously so the read loop never blocks on a directory
		// request (the response arrives on this same loop).
		go m.decryptDMAsync(chat, payload)
		return ""
	}

	key, ok := m.scopeKeys.get(scopeOf(chat), chat.KeyID)
	if !ok {
		chat.Text = missingKeyText
	} else if plain, err := openScope(chat.Text, key); err != nil {
		chat.Text = missingKeyText
	} else {
		chat.Text = plain
	}
	return rewrapChat(env.Type, chat, payload)
}

// scopeOf maps a chat broadcast to its key scope (channel id, 0 = global).
func scopeOf(chat netproto.ChatBroadcast) int64 {
	if chat.ChannelID == "" {
		return 0
	}
	var id int64
	_, _ = fmt.Sscan(chat.ChannelID, &id)
	return id
}

// decryptDMAsync resolves the sender's public key, decrypts the DM, and
// emits the chat event (or the missing-key placeholder).
func (m *connManager) decryptDMAsync(chat netproto.ChatBroadcast, payload string) {
	id, err := m.identity()
	if err != nil {
		return
	}
	_, priv, err := id.x25519()
	if err != nil {
		return
	}
	peer, ok := m.peerPubKey(chat.FromUniqueID)
	if !ok {
		chat.Text = missingKeyText
	} else if plain, err := openDM(chat.Text, peer, priv); err != nil {
		chat.Text = missingKeyText
	} else {
		chat.Text = plain
	}
	m.emit("event", rewrapChat("chat", chat, payload))
}

// rewrapChat re-encodes a (decrypted) chat broadcast into its event
// envelope, falling back to the original payload on error.
func rewrapChat(envType string, chat netproto.ChatBroadcast, fallback string) string {
	data, err := json.Marshal(chat)
	if err != nil {
		return fallback
	}
	env, err := json.Marshal(map[string]any{"type": envType, "data": json.RawMessage(data)})
	if err != nil {
		return fallback
	}
	return string(env)
}

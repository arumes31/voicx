// e2e.go implements the client half of chat encryption: the X25519 key
// directory cache, the per-scope chat key store, payload seal/open and the
// attachment file seal. The frontend stays plaintext internally — all crypto
// happens here, in the Go backend.
//
// Wire format (base64 string in ChatSend/ChatBroadcast.text and in
// ChatHistoryEntry.body_enc):
//   - direct messages: base64(nonce[24] || box.Seal(text, recipientPub, senderPriv))
//     — true E2EE; key_id = 0.
//   - channel/global:  base64(nonce[24] || secretbox.Seal(text, scopeKey))
//     — server-held key generations, distributed sealed via MsgChannelKey, the
//     AuthResponse, a history/pins page or MsgChatKeyBundle; key_id = generation.
//
// Attachments carry their own random key inside the (encrypted) message body,
// so the file bytes get exactly the protection the message text gets.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/nacl/box"
	"golang.org/x/crypto/nacl/secretbox"

	"voicx/internal/netproto"
)

// Placeholders for text the client cannot show. The three states are
// deliberately distinct: conflating a transient truncation with a permanent
// refusal turns a retryable miss into a permanent "[missing key]" on screen.
const (
	// missingKeyText is TRANSIENT: the generation is unknown and a pull is
	// pending or timed out.
	missingKeyText = "[encrypted message — key unavailable]"
	// refusedKeyText is PERMANENT: the server deliberately withheld the
	// generation (not a member, or an unknown generation).
	refusedKeyText = "[encrypted message — you do not have access]"
	// refusedPlainText marks a history body the server handed over in the
	// clear. No server config may downgrade this client (91-135).
	refusedPlainText = "[refused: server sent plaintext history]"
	// decryptFailedText is PERMANENT: key is available but payload decryption failed.
	decryptFailedText = "[encrypted message — decryption failed]"
)

// maxKeyRequest caps one MsgChatKeyRequest; the server caps the answer at the
// same number and reports Truncated when it could not serve them all.
const maxKeyRequest = 64

// scopeKeyWait/scopeKeyRetry bound the pull-and-retry for a generation that
// arrived on a live broadcast before its key did.
const (
	scopeKeyWait  = 10 * time.Second
	scopeKeyRetry = 2 * time.Second
)

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

type scopeKeyPullKey struct {
	scope int64
	keyID uint32
}

// scopeKeyStore keeps the chat key generations per scope (channelID, 0 =
// global) plus the generations the server permanently refused.
type scopeKeyStore struct {
	mu       sync.Mutex
	keys     map[int64]map[uint32][32]byte
	latest   map[int64]uint32
	refused  map[int64]map[uint32]bool
	inFlight map[scopeKeyPullKey]bool
	// updated is closed and replaced whenever a generation lands, so a waiter
	// blocked on a missing generation wakes without polling.
	updated chan struct{}
}

func newScopeKeyStore() *scopeKeyStore {
	return &scopeKeyStore{
		keys:     map[int64]map[uint32][32]byte{},
		latest:   map[int64]uint32{},
		refused:  map[int64]map[uint32]bool{},
		inFlight: map[scopeKeyPullKey]bool{},
		updated:  make(chan struct{}),
	}
}

// put installs the scope's CURRENT generation (a key push, the auth
// response). latest only ever advances: an archival generation arriving out
// of order must never push the send key backwards, or every following send is
// rejected as stale.
func (s *scopeKeyStore) put(scope int64, id uint32, key [32]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store(scope, id, key)
	if cur, ok := s.latest[scope]; !ok || id > cur {
		s.latest[scope] = id
	}
	s.wake()
}

// putGen installs an ARCHIVAL generation (history, pins, search, key
// bundles). It never touches the send generation.
func (s *scopeKeyStore) putGen(scope int64, id uint32, key [32]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store(scope, id, key)
	s.wake()
}

// store records key material and clears any stale refusal; callers hold s.mu.
func (s *scopeKeyStore) store(scope int64, id uint32, key [32]byte) {
	if s.keys[scope] == nil {
		s.keys[scope] = map[uint32][32]byte{}
	}
	s.keys[scope][id] = key
	delete(s.refused[scope], id)
}

// wake releases everyone waiting on a generation; callers hold s.mu.
func (s *scopeKeyStore) wake() {
	close(s.updated)
	s.updated = make(chan struct{})
}

func (s *scopeKeyStore) get(scope int64, id uint32) ([32]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[scope][id]
	return k, ok
}

// has reports whether the scope's generation is already installed.
func (s *scopeKeyStore) has(scope int64, id uint32) bool {
	_, ok := s.get(scope, id)
	return ok
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

// markRefused records generations the server permanently withheld so the UI
// can say "no access" instead of "retry pending".
func (s *scopeKeyStore) markRefused(scope int64, ids []uint32) {
	if len(ids) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refused[scope] == nil {
		s.refused[scope] = map[uint32]bool{}
	}
	for _, id := range ids {
		if _, ok := s.keys[scope][id]; ok {
			continue // already usable; a refusal cannot revoke it
		}
		s.refused[scope][id] = true
	}
	s.wake()
}

func (s *scopeKeyStore) isRefused(scope int64, id uint32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused[scope][id]
}

// waitCh returns a channel closed on the next install or refusal. Take it
// BEFORE sending the request, or the answer can land in the gap.
func (s *scopeKeyStore) waitCh() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updated
}

// claimPull attempts to claim in-flight pull ownership for (scope, keyID).
// It returns true if claimed, or false if a pull is already in flight.
func (s *scopeKeyStore) claimPull(scope int64, keyID uint32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	pk := scopeKeyPullKey{scope: scope, keyID: keyID}
	if s.inFlight[pk] {
		return false
	}
	s.inFlight[pk] = true
	return true
}

// releasePull releases in-flight pull ownership for (scope, keyID) and wakes waiters.
func (s *scopeKeyStore) releasePull(scope int64, keyID uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pk := scopeKeyPullKey{scope: scope, keyID: keyID}
	delete(s.inFlight, pk)
	s.wake()
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

// sealFile encrypts attachment bytes with a per-file key as
// nonce[24] || secretbox — raw bytes, because this blob is what crosses the
// file-transfer port and lands on the server's disk (12).
func sealFile(data []byte, key [32]byte) ([]byte, error) {
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	return secretbox.Seal(nonce[:], data, &nonce, &key), nil
}

// openFile decrypts an attachment blob produced by sealFile.
func openFile(blob []byte, key [32]byte) ([]byte, error) {
	if len(blob) < 24 {
		return nil, errors.New("invalid attachment ciphertext")
	}
	var nonce [24]byte
	copy(nonce[:], blob[:24])
	plain, ok := secretbox.Open(nil, blob[24:], &nonce, &key)
	if !ok {
		return nil, errors.New("attachment decryption failed (wrong key or tampered)")
	}
	return plain, nil
}

// --- connManager integration -------------------------------------------------

// publishE2EKey publishes the client's X25519 public key after auth. The
// server answers with the sealed channel key when the client is in a channel;
// the global generation already rode along in the AuthResponse.
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

// x25519PublicB64 returns the client's X25519 public key for the auth
// handshake, so the server can seal the global generation and the MOTD into
// the AuthResponse itself (133).
func (m *connManager) x25519PublicB64() string {
	id, err := m.identity()
	if err != nil {
		return ""
	}
	pub, _, err := id.x25519()
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(pub[:])
}

// handleChannelKey unseals a scope key delivered by the server. A pushed key
// is always the scope's current generation.
func (m *connManager) handleChannelKey(f *netproto.Frame) {
	var ck netproto.ChannelKey
	if err := netproto.Decode(f, &ck); err != nil {
		return
	}
	key, ok := m.unsealChannelKey(ck)
	if !ok {
		m.emit("servererror", "failed to unseal chat key (channel "+fmt.Sprint(ck.ChannelID)+")")
		return
	}
	m.scopeKeys.put(ck.ChannelID, ck.KeyID, key)
}

// handleChatKeyBundle installs the generations the server answered a pull
// with (99/100). Truncated is deliberately NOT treated as a refusal: the
// surplus stays unknown and is re-requested by the next miss.
func (m *connManager) handleChatKeyBundle(f *netproto.Frame) {
	var b netproto.ChatKeyBundle
	if err := netproto.Decode(f, &b); err != nil {
		return
	}
	m.installKeys(b.ChannelID, b.Keys)
	m.scopeKeys.markRefused(b.ChannelID, b.Refused)
}

// unsealChannelKey opens one sealed generation with the client's X25519
// private key (box.SealAnonymous envelope).
func (m *connManager) unsealChannelKey(ck netproto.ChannelKey) ([32]byte, bool) {
	var out [32]byte
	id, err := m.identity()
	if err != nil {
		return out, false
	}
	pub, priv, err := id.x25519()
	if err != nil {
		return out, false
	}
	raw, err := base64.StdEncoding.DecodeString(ck.SealedKey)
	if err != nil {
		return out, false
	}
	key, ok := box.OpenAnonymous(nil, raw, &pub, &priv)
	if !ok || len(key) != 32 {
		return out, false
	}
	copy(out[:], key)
	return out, true
}

// installKeys unseals ARCHIVAL generations bundled with a history or pins
// page, or answered to a pull. It never advances the send generation.
func (m *connManager) installKeys(scope int64, keys []netproto.ChannelKey) {
	for _, ck := range keys {
		if key, ok := m.unsealChannelKey(ck); ok {
			m.scopeKeys.putGen(scope, ck.KeyID, key)
		}
	}
}

// installCurrentKeys unseals generations that ARE the scope's current one
// (the AuthResponse's global key), advancing the send generation.
func (m *connManager) installCurrentKeys(scope int64, keys []netproto.ChannelKey) {
	for _, ck := range keys {
		if key, ok := m.unsealChannelKey(ck); ok {
			m.scopeKeys.put(scope, ck.KeyID, key)
		}
	}
}

// requestKeys pulls specific generations the client missed (99/100). It is
// fire-and-forget: the bundle lands on the read loop.
func (m *connManager) requestKeys(scope int64, ids []uint32) error {
	// Generations that landed while this pull was being assembled are dropped,
	// so a retry never spends the 64-id budget on keys already held.
	want := make([]uint32, 0, len(ids))
	for _, id := range ids {
		if !m.scopeKeys.has(scope, id) {
			want = append(want, id)
		}
	}
	if len(want) == 0 {
		return nil
	}
	if len(want) > maxKeyRequest {
		want = want[:maxKeyRequest]
	}
	return m.write(netproto.MsgChatKeyRequest, netproto.ChatKeyRequest{ChannelID: scope, KeyIDs: want})
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

// maybeDecryptEvent inspects a broadcast event envelope and decrypts every
// sealed payload in it: chat bodies, edited bodies (so the UI no longer has
// to re-fetch a history page to read an edit) and announcements. Everything
// else passes through unchanged. An empty return means the event will be
// emitted later by an async decrypt goroutine.
func (m *connManager) maybeDecryptEvent(payload string) string {
	var env struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		return payload
	}
	switch env.Type {
	case "chat":
		return m.decryptChatEvent(env.Data, payload)
	case "chat_edited":
		return m.decryptEventField(env.Type, "body", env.Data, payload)
	case "announcement":
		return m.decryptEventField(env.Type, "text", env.Data, payload)
	}
	return payload
}

// decryptChatEvent unseals a live chat broadcast.
func (m *connManager) decryptChatEvent(data json.RawMessage, payload string) string {
	var chat netproto.ChatBroadcast
	if err := json.Unmarshal(data, &chat); err != nil || !chat.Enc {
		return payload
	}

	if chat.E2E || chat.KeyID == 0 {
		// True E2EE DM: needs the sender's public key — resolved
		// asynchronously so the read loop never blocks on a directory
		// request (the response arrives on this same loop).
		// recover is per-goroutine and this parses attacker-controlled
		// ciphertext, so it needs its own crash guard (331).
		go guardCrash("decryptDM", func() { m.decryptDMAsync(chat, payload) })
		return ""
	}

	scope := scopeOf(chat)
	blob := chat.Text
	text, ok := m.resolveScopeText(scope, chat.KeyID, blob)
	if !ok {
		// The generation is unknown: pull it (99/100) and re-emit. This MUST
		// be a goroutine — the bundle arrives on this read loop — and needs
		// its own crash guard for the same reason as the DM branch (331).
		go guardCrash("chatKeyFetch", func() {
			chat.Text = m.awaitScopeText(scope, chat.KeyID, blob)
			m.emit("event", rewrapChat("chat", chat, payload))
		})
		return ""
	}
	chat.Text = text
	return rewrapChat("chat", chat, payload)
}

// decryptEventField unseals one sealed field of a scope event (the edited
// body, the announcement text) and strips enc/key_id, so the frontend never
// sees crypto state.
func (m *connManager) decryptEventField(envType, field string, data json.RawMessage, payload string) string {
	obj, ok := decodeEventObject(data)
	if !ok {
		return payload
	}
	if enc, _ := obj["enc"].(bool); !enc {
		return payload // old server, or an unsealed event
	}
	blob, _ := obj[field].(string)
	scope := jsonInt64(obj["channel_id"]) // absent on announcements = global
	keyID := uint32(jsonInt64(obj["key_id"]))

	text, ok := m.resolveScopeText(scope, keyID, blob)
	if !ok {
		go guardCrash("chatKeyFetch", func() {
			m.emit("event", finishEventField(envType, field, obj,
				m.awaitScopeText(scope, keyID, blob), payload))
		})
		return ""
	}
	return finishEventField(envType, field, obj, text, payload)
}

// resolveScopeText unseals blob with the scope generation. ok=false means the
// generation is simply unknown and worth pulling; a refusal or a failed open
// resolves immediately to a placeholder.
func (m *connManager) resolveScopeText(scope int64, keyID uint32, blob string) (string, bool) {
	if m.scopeKeys.isRefused(scope, keyID) {
		return refusedKeyText, true
	}
	key, ok := m.scopeKeys.get(scope, keyID)
	if !ok {
		return "", false
	}
	plain, err := openScope(blob, key)
	if err != nil {
		return decryptFailedText, true
	}
	return plain, true
}

// awaitScopeText pulls a generation the client missed and unseals blob once it
// lands. It MUST run off the read loop: the bundle arrives on that loop, and
// connManager.request holds a process-global mutex while waiting on it.
func (m *connManager) awaitScopeText(scope int64, keyID uint32, blob string) string {
	if !m.scopeKeys.claimPull(scope, keyID) {
		deadline := time.Now().Add(scopeKeyWait)
		for {
			ch := m.scopeKeys.waitCh()
			if text, ok := m.resolveScopeText(scope, keyID, blob); ok {
				return text
			}
			if time.Now().After(deadline) {
				return missingKeyText
			}
			select {
			case <-ch:
			case <-time.After(scopeKeyRetry):
			}
		}
	}
	defer m.scopeKeys.releasePull(scope, keyID)

	deadline := time.Now().Add(scopeKeyWait)
	for {
		if text, ok := m.resolveScopeText(scope, keyID, blob); ok {
			return text
		}
		if time.Now().After(deadline) {
			return missingKeyText
		}
		// Take the wake channel BEFORE asking, or the answer can land in the
		// gap and this waits out the full retry window for nothing.
		ch := m.scopeKeys.waitCh()
		if err := m.requestKeys(scope, []uint32{keyID}); err != nil {
			return missingKeyText
		}
		select {
		case <-ch:
		case <-time.After(scopeKeyRetry):
		}
	}
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

// finishEventField writes the decrypted text back into a raw event object,
// drops the crypto state and re-wraps the envelope.
func finishEventField(envType, field string, obj map[string]any, text, fallback string) string {
	obj[field] = text
	delete(obj, "enc")
	delete(obj, "key_id")
	data, err := json.Marshal(obj)
	if err != nil {
		return fallback
	}
	env, err := json.Marshal(map[string]any{"type": envType, "data": json.RawMessage(data)})
	if err != nil {
		return fallback
	}
	return string(env)
}

// decodeEventObject decodes a raw event payload keeping numbers exact, so a
// re-marshalled message id cannot drift through float64.
func decodeEventObject(data json.RawMessage) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, false
	}
	return obj, true
}

// jsonInt64 reads an integer out of a json.Number/float64/string field.
func jsonInt64(v any) int64 {
	switch n := v.(type) {
	case json.Number:
		i, _ := n.Int64()
		return i
	case float64:
		return int64(n)
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	}
	return 0
}

// e2e_test.go covers the wave-4b E2EE plumbing: key publish/request, chat
// key distribution and rotation, plaintext rejection, encrypted channel
// round-trip, and ciphertext spooling.
package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
	"golang.org/x/crypto/nacl/secretbox"

	"voicx/internal/chatcrypto"
	"voicx/internal/config"
	"voicx/internal/netproto"
	"voicx/internal/state"
)

// testX25519 generates a throwaway X25519 keypair for tests.
func testX25519(t *testing.T) (pub, priv [32]byte) {
	t.Helper()
	p, s, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("box.GenerateKey: %v", err)
	}
	return *p, *s
}

// x25519Pub derives the public key for a private key (X25519 with the
// curve base point).
func x25519Pub(priv [32]byte) [32]byte {
	out, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return [32]byte{}
	}
	var pub [32]byte
	copy(pub[:], out)
	return pub
}

func b64e(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// publishKey sends MsgKeyPublish and reads the global-scope ChannelKey that
// follows.
func publishKey(t *testing.T, conn net.Conn, pub [32]byte) netproto.ChannelKey {
	t.Helper()
	send(t, conn, netproto.MsgKeyPublish, netproto.KeyPublish{PublicKey: b64e(pub[:])})
	f := readOfType(t, conn, netproto.MsgChannelKey)
	var ck netproto.ChannelKey
	if err := netproto.Decode(f, &ck); err != nil {
		t.Fatalf("decode channel key: %v", err)
	}
	return ck
}

// unseal opens a sealed scope key with the recipient's keypair (kept for
// future client-side assertions; sealed delivery is verified end-to-end by
// the client TOFU/e2e tests).
func unseal(t *testing.T, ck netproto.ChannelKey, pub, priv [32]byte) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(ck.SealedKey)
	if err != nil {
		t.Fatalf("sealed key not base64: %v", err)
	}
	key, ok := box.OpenAnonymous(nil, raw, &pub, &priv)
	if !ok {
		t.Fatal("box.OpenAnonymous failed on sealed channel key")
	}
	return key
}

// TestKeyPublishAndRequest verifies the directory: a registered user's
// published key is persisted and resolvable; guests resolve from state.
func TestKeyPublishAndRequest(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = conn.Close() }()
	pub, _ := testX25519(t)
	send(t, conn, netproto.MsgKeyPublish, netproto.KeyPublish{PublicKey: b64e(pub[:])})
	// The publish triggers delivery of the global key; drain it.
	readOfType(t, conn, netproto.MsgChannelKey)

	// Persisted for the registered user.
	if got := env.auth.e2eByUID["user-uid"]; got != b64e(pub[:]) {
		t.Fatalf("persisted key = %q, want %q", got, b64e(pub[:]))
	}

	// Resolvable via KeyRequest from another client.
	conn2, _ := dialAuthed(t, env.addr, "admin-uid")
	defer func() { _ = conn2.Close() }()
	send(t, conn2, netproto.MsgKeyRequest, netproto.KeyRequest{UniqueID: "user-uid"})
	f := readOfType(t, conn2, netproto.MsgKeyResponse)
	var resp netproto.KeyResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode key response: %v", err)
	}
	if resp.PublicKey != b64e(pub[:]) {
		t.Fatalf("directory key = %q, want %q", resp.PublicKey, b64e(pub[:]))
	}

	// Unknown user → empty key (omitempty drops the field, so use a fresh
	// struct to avoid stale values from the first decode).
	send(t, conn2, netproto.MsgKeyRequest, netproto.KeyRequest{UniqueID: "nobody"})
	f = readOfType(t, conn2, netproto.MsgKeyResponse)
	var resp2 netproto.KeyResponse
	if err := netproto.Decode(f, &resp2); err != nil {
		t.Fatalf("decode key response: %v", err)
	}
	if resp2.PublicKey != "" {
		t.Fatalf("unknown user key = %q, want empty", resp2.PublicKey)
	}
}

// TestPlaintextChatRejected verifies plaintext chat is rejected when
// chat_allow_plaintext is false (the default), with a clear error.
func TestPlaintextChatRejected(t *testing.T) {
	env := startTestEnvFull(t, nil, func(c *config.Config) { c.ChatAllowPlaintext = false })
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = conn.Close() }()

	send(t, conn, netproto.MsgChatSend, netproto.ChatSend{Text: "hello"})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("error code = %d, want %d", e.Code, errCodePermissionDenied)
	}
	if got := e.Message; got == "" || got[:10] != "plaintext " {
		t.Fatalf("unexpected error message: %q", got)
	}
}

// TestChannelKeyRotationOnLeave verifies members get a sealed key on join,
// and a leave rotates the key: remaining members receive a bumped key id and
// sends with the stale key are rejected.
func TestChannelKeyRotationOnLeave(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	alice, _ := dialAuthed(t, env.addr, "admin-uid")
	defer func() { _ = alice.Close() }()
	bob, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = bob.Close() }()

	apub, _ := testX25519(t)
	bpub, _ := testX25519(t)
	publishKey(t, alice, apub)
	publishKey(t, bob, bpub)

	env.state.AddChannel(testChannel(1))
	send(t, alice, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 1})
	ckAlice1 := readChannelKeyFor(t, alice, 1)
	send(t, bob, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 1})
	ckBob1 := readChannelKeyFor(t, bob, 1)

	if ckAlice1.KeyID != ckBob1.KeyID {
		t.Fatalf("same generation expected: alice %d, bob %d", ckAlice1.KeyID, ckBob1.KeyID)
	}
	if ckAlice1.SealedKey == ckBob1.SealedKey {
		t.Fatal("sealed keys must differ per member (sealed to different pubkeys)")
	}

	// Bob leaves; alice must receive a bumped generation.
	_ = bob.Close()
	ckAlice2 := readChannelKeyFor(t, alice, 1)
	if ckAlice2.KeyID != ckAlice1.KeyID+1 {
		t.Fatalf("rotated key id = %d, want %d", ckAlice2.KeyID, ckAlice1.KeyID+1)
	}

	// A send with the stale key id is rejected.
	send(t, alice, netproto.MsgChatSend, netproto.ChatSend{
		ChannelID: "1", Text: b64e([]byte("ciphertext")), Enc: true, KeyID: ckAlice1.KeyID,
	})
	f := readOfType(t, alice, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Message == "" || e.Message[:5] != "stale" {
		t.Fatalf("error = %q, want stale-key rejection", e.Message)
	}
}

// sealScopeTest seals plaintext the same way a client would (nonce‖secretbox).
func sealScopeTest(t *testing.T, key [32]byte, plain string) string {
	t.Helper()
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	return base64.StdEncoding.EncodeToString(append(nonce[:], secretbox.Seal(nil, []byte(plain), &nonce, &key)...))
}

// openScopeTest opens a scope-sealed body the way a client would.
func openScopeTest(t *testing.T, key [32]byte, blobB64 string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(blobB64)
	if err != nil || len(raw) < 24 {
		t.Fatalf("scope ciphertext is not base64 nonce||box: %q", blobB64)
	}
	var nonce [24]byte
	copy(nonce[:], raw[:24])
	plain, ok := secretbox.Open(nil, raw[24:], &nonce, &key)
	if !ok {
		t.Fatal("scope decryption failed")
	}
	return string(plain)
}

// unsealScopeKey opens one sealed generation from a piggybacked key bundle.
func unsealScopeKey(t *testing.T, ck netproto.ChannelKey, pub, priv [32]byte) [32]byte {
	t.Helper()
	var key [32]byte
	copy(key[:], unseal(t, ck, pub, priv))
	return key
}

// TestEncryptedChannelChatRoundTrip verifies an encrypted channel message is
// fanned out as ciphertext: the server decrypts it for moderation/storage
// but relays the ORIGINAL ciphertext to members.
func TestEncryptedChannelChatRoundTrip(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	alice, _ := dialAuthed(t, env.addr, "admin-uid")
	defer func() { _ = alice.Close() }()
	bob, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = bob.Close() }()

	apub, _ := testX25519(t)
	bpub, bpriv := testX25519(t)
	publishKey(t, alice, apub)
	publishKey(t, bob, bpub)

	env.state.AddChannel(testChannel(1))
	send(t, alice, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 1})
	readChannelKeyFor(t, alice, 1)
	send(t, bob, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 1})
	ckBob := readChannelKeyFor(t, bob, 1)

	// Bob unseals his channel key and sends a properly sealed message.
	var scopeKey [32]byte
	copy(scopeKey[:], unseal(t, ckBob, bpub, bpriv))
	plaintext := "meet at eight"
	ciphertext := sealScopeTest(t, scopeKey, plaintext)
	send(t, bob, netproto.MsgChatSend, netproto.ChatSend{
		ChannelID: "1", Text: ciphertext, Enc: true, KeyID: ckBob.KeyID,
	})

	data := readEventOfType(t, alice, eventChat)
	var chat netproto.ChatBroadcast
	if err := json.Unmarshal(data, &chat); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}
	if !chat.Enc || chat.KeyID != ckBob.KeyID {
		t.Fatalf("chat enc/key = %v/%d, want true/%d", chat.Enc, chat.KeyID, ckBob.KeyID)
	}
	if chat.Text != ciphertext {
		t.Fatalf("server altered the relayed ciphertext: %q != %q", chat.Text, ciphertext)
	}
	if chat.ID == 0 {
		t.Fatal("chat message was not stored (missing id)")
	}
	if chat.E2E {
		t.Fatal("channel chat must not be marked E2E (server-held keys)")
	}
}

// TestE2EDMSpoolCiphertext verifies an encrypted DM to an offline user is
// spooled as ciphertext with the sender's unique ID, and delivered marked
// E2E on the recipient's next login.
func TestE2EDMSpoolCiphertext(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	alice, _ := dialAuthed(t, env.addr, "admin-uid")
	apub, _ := testX25519(t)
	publishKey(t, alice, apub)

	ciphertext := b64e([]byte("sealed-for-bob"))
	send(t, alice, netproto.MsgChatSend, netproto.ChatSend{
		ToUniqueID: "user-uid", Text: ciphertext, Enc: true,
	})

	// Spooled as ciphertext with the sender's unique ID (the server processes
	// the frame asynchronously — poll for it).
	var entry spooledEntry
	deadline := time.Now().Add(3 * time.Second)
	for {
		env.spool.mu.Lock()
		if len(env.spool.pending) == 1 {
			entry = env.spool.pending[0]
			env.spool.mu.Unlock()
			break
		}
		env.spool.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("spool size = %d, want 1", len(env.spool.pending))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if entry.Message != ciphertext || entry.FromUniqueID != "admin-uid" {
		t.Fatalf("spooled = %+v, want ciphertext + admin-uid", entry)
	}
	_ = alice.Close()

	// Bob logs in: the offline delivery is marked Enc + E2E with the sender's
	// unique ID so the client can fetch the public key.
	bob, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = bob.Close() }()
	data := readEventOfType(t, bob, eventChat)
	var chat netproto.ChatBroadcast
	if err := json.Unmarshal(data, &chat); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}
	if !chat.Offline || !chat.Enc || !chat.E2E || chat.FromUniqueID != "admin-uid" || chat.Text != ciphertext {
		t.Fatalf("delivered spooled chat = %+v", chat)
	}
}

// testKeyManager builds a standalone key manager over a fresh fake store and
// KEK ring, for the tests that need to reason about generations directly.
func testKeyManager(t *testing.T, st ScopeKeyStore) (*chatKeyManager, ScopeKeyStore, *chatcrypto.KEKRing) {
	t.Helper()
	if st == nil {
		st = newFakeScopeKeys()
	}
	kek, err := chatcrypto.LoadKEKRing(filepath.Join(t.TempDir(), "kek.ring"), "", true)
	if err != nil {
		t.Fatalf("load kek ring: %v", err)
	}
	return newChatKeyManager(st, kek, testLogger()), st, kek
}

// TestScopeKeyNotMintedByChatSend is the disk-exhaustion guard: a send naming
// a channel the client is not in must be refused BEFORE anything can mint,
// because minting from an attacker-supplied scope id fills the disk (91).
func TestScopeKeyNotMintedByChatSend(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = conn.Close() }()
	pub, _ := testX25519(t)
	publishKey(t, conn, pub)

	fsk, ok := env.deps.ScopeKeys.(*fakeScopeKeys)
	if !ok {
		t.Fatalf("scope key store is %T, want the fake", env.deps.ScopeKeys)
	}
	before := fsk.insertCount()

	send(t, conn, netproto.MsgChatSend, netproto.ChatSend{
		ChannelID: "999999", Text: b64e([]byte("ciphertext")), Enc: true, KeyID: 1,
	})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("error = %d %q, want permission denied", e.Code, e.Message)
	}
	if got := fsk.insertCount(); got != before {
		t.Fatalf("a rejected send minted %d generations", got-before)
	}

	// The same holds through the plaintext escape hatch, which skips every
	// key check and would otherwise reach EnsureScope.
	send(t, conn, netproto.MsgChatSend, netproto.ChatSend{ChannelID: "999998", Text: "plain"})
	f = readOfType(t, conn, netproto.MsgError)
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("plaintext hatch: error = %d %q, want permission denied", e.Code, e.Message)
	}
	if got := fsk.insertCount(); got != before {
		t.Fatalf("the plaintext hatch minted %d generations", got-before)
	}
}

// TestRotateOnUnloadedScopeDoesNotCollide verifies generation ids come from
// the database sequence, never from RAM: a manager that never loaded a scope
// must not re-mint a colliding id with different key material.
func TestRotateOnUnloadedScopeDoesNotCollide(t *testing.T) {
	ctx := t.Context()
	first, st, kek := testKeyManager(t, nil)

	gen1, key1, err := first.EnsureScope(ctx, 7)
	if err != nil {
		t.Fatalf("EnsureScope: %v", err)
	}
	if gen1 != 1 {
		t.Fatalf("first generation = %d, want 1", gen1)
	}

	// A fresh manager over the same store has no memory of scope 7 at all.
	cold := newChatKeyManager(st, kek, testLogger())
	gen2, key2, err := cold.rotate(ctx, 7)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if gen2 != 2 {
		t.Fatalf("rotated generation = %d, want 2 from the sequence", gen2)
	}
	if key1 == key2 {
		t.Fatal("rotation reused the previous key material")
	}
	recovered, err := cold.at(ctx, 7, gen1)
	if err != nil {
		t.Fatalf("at(gen 1): %v", err)
	}
	if recovered != key1 {
		t.Fatal("rotation overwrote generation 1's key material")
	}
}

// TestHistorySurvivesRestart verifies a restart recovers identical key bytes,
// so ciphertext written before it still opens.
func TestHistorySurvivesRestart(t *testing.T) {
	ctx := t.Context()
	before, st, kek := testKeyManager(t, nil)

	gen, key, err := before.EnsureScope(ctx, 3)
	if err != nil {
		t.Fatalf("EnsureScope: %v", err)
	}
	sealedGen, blob, err := before.seal(ctx, 3, "survives the restart")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if sealedGen != gen {
		t.Fatalf("sealed under %d, want %d", sealedGen, gen)
	}

	after := newChatKeyManager(st, kek, testLogger())
	recovered, err := after.at(ctx, 3, gen)
	if err != nil {
		t.Fatalf("at after restart: %v", err)
	}
	if recovered != key {
		t.Fatal("the restarted manager recovered different key material")
	}
	plain, err := after.open(ctx, 3, gen, blob)
	if err != nil {
		t.Fatalf("open after restart: %v", err)
	}
	if plain != "survives the restart" {
		t.Fatalf("opened %q", plain)
	}
}

// TestRotationCoalescing verifies leaves inside chat_key_rotate_min_seconds
// produce exactly ONE new generation: a client flapping on a bad link would
// otherwise mint one persisted generation per reconnect.
func TestRotationCoalescing(t *testing.T) {
	env := startTestEnvFull(t, nil, func(c *config.Config) { c.ChatKeyRotateMinSecs = 1 })
	defer env.stop()
	ctx := t.Context()

	if _, _, err := env.srv.chatKeys.EnsureScope(ctx, 4); err != nil {
		t.Fatalf("EnsureScope: %v", err)
	}
	fsk := env.deps.ScopeKeys.(*fakeScopeKeys)
	before := fsk.countFor(4)

	for i := 0; i < 10; i++ {
		env.srv.rotateScopeKey(ctx, 4)
	}
	waitFor(t, "the coalesced rotation to fire", func() bool { return fsk.countFor(4) > before })
	time.Sleep(300 * time.Millisecond)
	if got := fsk.countFor(4) - before; got != 1 {
		t.Fatalf("ten leaves in one window produced %d generations, want 1", got)
	}
}

// TestChatKeyRequestRequiresMembership verifies a non-member's request is
// answered with Refused rather than sealed keys.
func TestChatKeyRequestRequiresMembership(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	alice, bob, _, keyID, _ := chatPair(t, env)
	defer func() { _ = alice.Close() }()
	defer func() { _ = bob.Close() }()

	env.state.AddChannel(testChannel(2))
	outsider, _ := dialAuthed(t, env.addr, "admin-uid")
	defer func() { _ = outsider.Close() }()
	opub, _ := testX25519(t)
	publishKey(t, outsider, opub)
	send(t, outsider, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 2})
	readChannelKeyFor(t, outsider, 2)

	send(t, outsider, netproto.MsgChatKeyRequest, netproto.ChatKeyRequest{ChannelID: 1, KeyIDs: []uint32{keyID}})
	f := readOfType(t, outsider, netproto.MsgChatKeyBundle)
	var bundle netproto.ChatKeyBundle
	if err := netproto.Decode(f, &bundle); err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	if len(bundle.Keys) != 0 || len(bundle.Refused) != 1 || bundle.Refused[0] != keyID {
		t.Fatalf("bundle = %+v, want generation %d refused", bundle, keyID)
	}
}

// TestChatKeyRequestRejectsEmptyIDs verifies the cheap-to-send request is
// shape-checked before it costs a seal.
func TestChatKeyRequestRejectsEmptyIDs(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = conn.Close() }()
	pub, _ := testX25519(t)
	publishKey(t, conn, pub)

	send(t, conn, netproto.MsgChatKeyRequest, netproto.ChatKeyRequest{ChannelID: 0})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodeMalformed {
		t.Fatalf("empty key_ids: error = %d, want malformed", e.Code)
	}

	over := make([]uint32, maxKeysPerResponse+1)
	send(t, conn, netproto.MsgChatKeyRequest, netproto.ChatKeyRequest{ChannelID: 0, KeyIDs: over})
	f = readOfType(t, conn, netproto.MsgError)
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodeMalformed {
		t.Fatalf("oversized key_ids: error = %d, want malformed", e.Code)
	}
}

// TestChatKeyRequestRateLimited verifies the handler runs through the same
// per-user bucket as a send: it is cheap to call and expensive to serve.
func TestChatKeyRequestRateLimited(t *testing.T) {
	env := startTestEnvFull(t, nil, func(c *config.Config) {
		c.ChatRateMsgs = 2
		c.ChatRateWindowSeconds = 30
	})
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = conn.Close() }()
	pub, _ := testX25519(t)
	publishKey(t, conn, pub)

	for i := 0; i < 2; i++ {
		send(t, conn, netproto.MsgChatKeyRequest, netproto.ChatKeyRequest{ChannelID: 0, KeyIDs: []uint32{1}})
		readOfType(t, conn, netproto.MsgChatKeyBundle)
	}
	send(t, conn, netproto.MsgChatKeyRequest, netproto.ChatKeyRequest{ChannelID: 0, KeyIDs: []uint32{1}})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if !strings.Contains(e.Message, "rate limit") {
		t.Fatalf("error = %q, want a rate-limit rejection", e.Message)
	}
}

// readChannelKeyFor reads ChannelKey frames until one for the given scope
// arrives (the global key may arrive first).
func readChannelKeyFor(t *testing.T, conn net.Conn, scope int64) netproto.ChannelKey {
	t.Helper()
	for i := 0; i < 5; i++ {
		f := readOfType(t, conn, netproto.MsgChannelKey)
		var ck netproto.ChannelKey
		if err := netproto.Decode(f, &ck); err != nil {
			t.Fatalf("decode channel key: %v", err)
		}
		if ck.ChannelID == scope {
			return ck
		}
	}
	t.Fatalf("no ChannelKey for scope %d", scope)
	return netproto.ChannelKey{}
}

// testChannel returns a minimal permanent channel for state registration.
func testChannel(id int64) *state.Channel {
	return &state.Channel{ChannelID: id, Name: "test", ChannelType: 2}
}

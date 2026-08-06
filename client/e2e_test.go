package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/nacl/box"
	"golang.org/x/crypto/nacl/secretbox"

	"voicx/internal/netproto"
)

func genPair(t *testing.T) (pub, priv [32]byte) {
	t.Helper()
	p, s, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("box.GenerateKey: %v", err)
	}
	return *p, *s
}

func TestNewClientMsgID(t *testing.T) {
	t.Parallel()

	seen := make(map[string]struct{}, 128)
	for range 128 {
		id := newClientMsgID()
		if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
			t.Fatalf("newClientMsgID() = %q, want canonical UUID", id)
		}
		raw, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
		if err != nil {
			t.Fatalf("decode UUID %q: %v", id, err)
		}
		if raw[6]>>4 != 4 {
			t.Fatalf("UUID %q version = %d, want 4", id, raw[6]>>4)
		}
		if raw[8]>>6 != 2 {
			t.Fatalf("UUID %q variant bits = %02b, want RFC 4122", id, raw[8]>>6)
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("newClientMsgID generated duplicate %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestE2EEDiagnostics(t *testing.T) {
	t.Parallel()

	if _, err := (&App{}).E2EEDiagnostics(""); err == nil || err.Error() != "not connected" {
		t.Fatalf("offline E2EEDiagnostics error = %v, want not connected", err)
	}

	cm := newConnManager(context.Background())
	cm.id = mustTempIdentity(t)
	peerPub, _ := genPair(t)
	cm.pubKeys.put("peer", peerPub)
	cm.scopeKeys.put(7, 1, [32]byte{1})
	cm.scopeKeys.putGen(7, 2, [32]byte{2})
	cm.scopeKeys.markRefused(7, []uint32{3, 4})
	if !cm.scopeKeys.claimPull(7, 5) {
		t.Fatal("failed to seed pending key pull")
	}

	d, err := appWithCM(cm).E2EEDiagnostics("peer")
	if err != nil {
		t.Fatalf("E2EEDiagnostics: %v", err)
	}
	if !d.PeerKeyAvailable || d.PeerUniqueID != "peer" {
		t.Fatalf("peer diagnostics = %+v", d)
	}
	if d.CachedPeers != 1 || d.ScopeKeys != 2 || d.RefusedKeys != 2 || d.PendingKeyPulls != 1 {
		t.Fatalf("diagnostic counts = %+v", d)
	}
	groups := strings.Fields(d.SafetyNumber)
	if len(groups) != 12 {
		t.Fatalf("safety number = %q, want 12 groups", d.SafetyNumber)
	}
	for _, group := range groups {
		if len(group) != 5 {
			t.Fatalf("safety number group = %q, want five digits", group)
		}
	}

	unknown, err := appWithCM(cm).E2EEDiagnostics("unknown")
	if err != nil {
		t.Fatalf("unknown peer diagnostics: %v", err)
	}
	if unknown.PeerKeyAvailable || unknown.SafetyNumber != "" {
		t.Fatalf("unknown peer unexpectedly resolved: %+v", unknown)
	}
}

func TestPubKeyCache(t *testing.T) {
	t.Parallel()

	cache := newPubKeyCache()
	if _, ok := cache.get("alice"); ok {
		t.Fatal("empty cache returned a key")
	}
	first := [32]byte{1}
	second := [32]byte{2}
	cache.put("alice", first)
	if got, ok := cache.get("alice"); !ok || got != first {
		t.Fatalf("get after put = %v/%v", got, ok)
	}
	cache.put("alice", second)
	if got, ok := cache.get("alice"); !ok || got != second {
		t.Fatalf("get after replace = %v/%v", got, ok)
	}
}

// TestDMRoundTrip verifies box seal/open for direct messages, including
// wrong-key rejection.
func TestDMRoundTrip(t *testing.T) {
	t.Parallel()

	alicePub, alicePriv := genPair(t)
	bobPub, bobPriv := genPair(t)

	blob, err := sealDM("hello bob", bobPub, alicePriv)
	if err != nil {
		t.Fatalf("sealDM: %v", err)
	}
	if blob == "hello bob" {
		t.Fatal("ciphertext equals plaintext")
	}
	plain, err := openDM(blob, alicePub, bobPriv)
	if err != nil {
		t.Fatalf("openDM: %v", err)
	}
	if plain != "hello bob" {
		t.Fatalf("plaintext = %q, want %q", plain, "hello bob")
	}

	// Wrong recipient key must fail.
	evePub, _ := genPair(t)
	if _, err := openDM(blob, evePub, bobPriv); err == nil {
		t.Fatal("openDM with wrong sender key succeeded")
	}
	// Tampered ciphertext must fail.
	raw := []byte(blob)
	raw[len(raw)-5] ^= 0xFF
	if _, err := openDM(string(raw), alicePub, bobPriv); err == nil {
		t.Fatal("openDM on tampered blob succeeded")
	}
	for _, invalid := range []string{"%%%", base64.StdEncoding.EncodeToString(make([]byte, 23))} {
		if _, err := openDM(invalid, alicePub, bobPriv); err == nil {
			t.Fatalf("openDM(%q) succeeded", invalid)
		}
	}
}

// TestScopeRoundTrip verifies secretbox seal/open for channel/global
// messages, including wrong-key rejection.
func TestScopeRoundTrip(t *testing.T) {
	t.Parallel()

	var key, other [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(other[:]); err != nil {
		t.Fatal(err)
	}

	blob, err := sealScope("channel secret", key)
	if err != nil {
		t.Fatalf("sealScope: %v", err)
	}
	plain, err := openScope(blob, key)
	if err != nil {
		t.Fatalf("openScope: %v", err)
	}
	if plain != "channel secret" {
		t.Fatalf("plaintext = %q", plain)
	}
	if _, err := openScope(blob, other); err == nil {
		t.Fatal("openScope with wrong key succeeded")
	}
	for _, invalid := range []string{"%%%", base64.StdEncoding.EncodeToString(make([]byte, 23))} {
		if _, err := openScope(invalid, key); err == nil {
			t.Fatalf("openScope(%q) succeeded", invalid)
		}
	}
}

// TestScopeKeyStore verifies latest-generation tracking and per-generation
// lookup (key rotation: old generations stay readable).
func TestScopeKeyStore(t *testing.T) {
	t.Parallel()

	s := newScopeKeyStore()
	var k1, k2 [32]byte
	k1[0], k2[0] = 1, 2
	s.put(7, 1, k1)
	s.put(7, 2, k2)

	id, cur, ok := s.current(7)
	if !ok || id != 2 || cur != k2 {
		t.Fatalf("current = %d/%v, want 2/k2", id, ok)
	}
	if got, ok := s.get(7, 1); !ok || got != k1 {
		t.Fatal("generation 1 lost after rotation")
	}
	if _, _, ok := s.current(99); ok {
		t.Fatal("unknown scope has a key")
	}

	// Archival keys remain readable without moving the current send key back.
	s.putGen(7, 3, [32]byte{3})
	if id, _, ok := s.current(7); !ok || id != 2 {
		t.Fatalf("archival key changed current generation to %d", id)
	}

	wake := s.waitCh()
	s.markRefused(7, []uint32{1, 4})
	select {
	case <-wake:
	default:
		t.Fatal("markRefused did not wake waiters")
	}
	if s.isRefused(7, 1) {
		t.Fatal("refusal revoked an installed key")
	}
	if !s.isRefused(7, 4) {
		t.Fatal("missing key refusal was not recorded")
	}
	s.putGen(7, 4, [32]byte{4})
	if s.isRefused(7, 4) {
		t.Fatal("install did not clear stale refusal")
	}

	if !s.claimPull(7, 9) || s.claimPull(7, 9) {
		t.Fatal("key pull ownership was not exclusive")
	}
	wake = s.waitCh()
	s.releasePull(7, 9)
	select {
	case <-wake:
	default:
		t.Fatal("releasePull did not wake waiters")
	}
	if !s.claimPull(7, 9) {
		t.Fatal("released key pull could not be reclaimed")
	}
}

func TestAttachmentLegacyAndFramingValidation(t *testing.T) {
	t.Parallel()

	key := [32]byte{7}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	legacy := append(nonce[:], secretbox.Seal(nil, []byte("legacy attachment"), &nonce, &key)...)
	plain, err := openFile(legacy, key)
	if err != nil {
		t.Fatalf("open legacy attachment: %v", err)
	}
	if string(plain) != "legacy attachment" {
		t.Fatalf("legacy attachment = %q", plain)
	}

	framed := func(size uint32, body []byte) []byte {
		var out bytes.Buffer
		out.Write(attachmentGCMHeader)
		if err := binary.Write(&out, binary.BigEndian, size); err != nil {
			t.Fatal(err)
		}
		out.Write(body)
		return out.Bytes()
	}
	for _, tc := range []struct {
		name string
		blob []byte
	}{
		{name: "zero chunk", blob: framed(0, nil)},
		{name: "oversized chunk", blob: framed(attachmentChunkSize+65, make([]byte, attachmentChunkSize+65))},
		{name: "truncated chunk", blob: framed(32, []byte{1})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := openFile(tc.blob, key); err == nil {
				t.Fatal("openFile accepted invalid GCM framing")
			}
		})
	}
}

func TestEncryptChatScopes(t *testing.T) {
	t.Parallel()

	cm := newConnManager(context.Background())
	cm.id = mustTempIdentity(t)
	ownPub, _, err := cm.id.x25519()
	if err != nil {
		t.Fatalf("identity x25519: %v", err)
	}
	peerPub, peerPriv := genPair(t)
	cm.pubKeys.put("peer", peerPub)
	channelKey := [32]byte{7}
	globalKey := [32]byte{9}
	cm.scopeKeys.put(42, 3, channelKey)
	cm.scopeKeys.put(0, 5, globalKey)

	direct, err := cm.encryptChat("direct", "peer", "private")
	if err != nil {
		t.Fatalf("encrypt direct: %v", err)
	}
	if !direct.Enc || direct.ToUniqueID != "peer" || direct.KeyID != 0 || direct.ClientMsgID == "" {
		t.Fatalf("direct message = %+v", direct)
	}
	if text, err := openDM(direct.Text, ownPub, peerPriv); err != nil || text != "private" {
		t.Fatalf("open direct = %q, %v", text, err)
	}

	channel, err := cm.encryptChat("channel", "42", "room")
	if err != nil {
		t.Fatalf("encrypt channel: %v", err)
	}
	if !channel.Enc || channel.ChannelID != "42" || channel.KeyID != 3 || channel.ClientMsgID == "" {
		t.Fatalf("channel message = %+v", channel)
	}
	if text, err := openScope(channel.Text, channelKey); err != nil || text != "room" {
		t.Fatalf("open channel = %q, %v", text, err)
	}

	global, err := cm.encryptChat("global", "", "world")
	if err != nil {
		t.Fatalf("encrypt global: %v", err)
	}
	if !global.Enc || global.ChannelID != "" || global.KeyID != 5 || global.ClientMsgID == "" {
		t.Fatalf("global message = %+v", global)
	}
	if text, err := openScope(global.Text, globalKey); err != nil || text != "world" {
		t.Fatalf("open global = %q, %v", text, err)
	}

	for _, tc := range []struct {
		name   string
		scope  string
		target string
	}{
		{name: "missing direct key", scope: "direct", target: "missing"},
		{name: "invalid channel id", scope: "channel", target: "nope"},
		{name: "missing channel key", scope: "channel", target: "99"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := cm.encryptChat(tc.scope, tc.target, "text"); err == nil {
				t.Fatal("encryptChat succeeded")
			}
		})
	}
}

func TestEncryptedEventTransformation(t *testing.T) {
	t.Parallel()

	cm := newConnManager(context.Background())
	key := [32]byte{3}
	cm.scopeKeys.put(42, 8, key)

	wrap := func(eventType string, data any) string {
		t.Helper()
		dataJSON, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		envelope, err := json.Marshal(map[string]any{"type": eventType, "data": json.RawMessage(dataJSON)})
		if err != nil {
			t.Fatal(err)
		}
		return string(envelope)
	}

	chatCipher, err := sealScope("hello channel", key)
	if err != nil {
		t.Fatal(err)
	}
	chatPayload := wrap("chat", netproto.ChatBroadcast{
		ChannelID: "42",
		Text:      chatCipher,
		Enc:       true,
		KeyID:     8,
	})
	chatOut := cm.maybeDecryptEvent(chatPayload)
	var chatEnv struct {
		Type string                 `json:"type"`
		Data netproto.ChatBroadcast `json:"data"`
	}
	if err := json.Unmarshal([]byte(chatOut), &chatEnv); err != nil {
		t.Fatalf("decode chat output: %v", err)
	}
	if chatEnv.Type != "chat" || chatEnv.Data.Text != "hello channel" {
		t.Fatalf("chat output = %+v", chatEnv)
	}

	editCipher, err := sealScope("edited", key)
	if err != nil {
		t.Fatal(err)
	}
	const exactID = int64(9_007_199_254_740_993)
	editPayload := `{"type":"chat_edited","data":{"message_id":9007199254740993,"channel_id":42,"body":` +
		string(mustJSON(t, editCipher)) + `,"enc":true,"key_id":8}}`
	editOut := cm.maybeDecryptEvent(editPayload)
	var editEnv struct {
		Data map[string]any `json:"data"`
	}
	dec := json.NewDecoder(strings.NewReader(editOut))
	dec.UseNumber()
	if err := dec.Decode(&editEnv); err != nil {
		t.Fatalf("decode edit output: %v", err)
	}
	if got := jsonInt64(editEnv.Data["message_id"]); got != exactID {
		t.Fatalf("message id = %d, want %d", got, exactID)
	}
	if editEnv.Data["body"] != "edited" || editEnv.Data["enc"] != nil || editEnv.Data["key_id"] != nil {
		t.Fatalf("edit output = %+v", editEnv.Data)
	}

	announcementKey := [32]byte{4}
	cm.scopeKeys.put(0, 2, announcementKey)
	announcementCipher, err := sealScope("maintenance", announcementKey)
	if err != nil {
		t.Fatal(err)
	}
	announcement := wrap("announcement", map[string]any{
		"text":   announcementCipher,
		"enc":    true,
		"key_id": 2,
	})
	if out := cm.maybeDecryptEvent(announcement); !strings.Contains(out, "maintenance") || strings.Contains(out, announcementCipher) {
		t.Fatalf("announcement output = %s", out)
	}
	invalidGeneration := wrap("announcement", map[string]any{
		"text":   "ciphertext",
		"enc":    true,
		"key_id": uint64(1) << 32,
	})
	if out := cm.maybeDecryptEvent(invalidGeneration); !strings.Contains(out, decryptFailedText) ||
		strings.Contains(out, "ciphertext") || strings.Contains(out, `"key_id"`) {
		t.Fatalf("invalid key generation output = %s", out)
	}

	for _, unchanged := range []string{
		"not json",
		wrap("presence", map[string]any{"status": "online"}),
		wrap("chat_edited", map[string]any{"body": "plain"}),
	} {
		if got := cm.maybeDecryptEvent(unchanged); got != unchanged {
			t.Fatalf("unsealed event changed from %q to %q", unchanged, got)
		}
	}

	cm.scopeKeys.markRefused(42, []uint32{99})
	if text, ok := cm.resolveScopeText(42, 99, "unused"); !ok || text != refusedKeyText {
		t.Fatalf("refused key resolved to %q/%v", text, ok)
	}
	if text, ok := cm.resolveScopeText(42, 100, "unused"); ok || text != "" {
		t.Fatalf("missing key resolved to %q/%v", text, ok)
	}
	if text, ok := cm.resolveScopeText(42, 8, "tampered"); !ok || text != decryptFailedText {
		t.Fatalf("tampered ciphertext resolved to %q/%v", text, ok)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEventJSONHelpers(t *testing.T) {
	t.Parallel()

	obj, ok := decodeEventObject(json.RawMessage(`{"id":9007199254740993,"text":"before","enc":true,"key_id":2}`))
	if !ok || jsonInt64(obj["id"]) != 9_007_199_254_740_993 {
		t.Fatalf("decodeEventObject = %#v/%v", obj, ok)
	}
	out := finishEventField("announcement", "text", obj, "after", "fallback")
	if !strings.Contains(out, `"text":"after"`) || strings.Contains(out, `"enc"`) || strings.Contains(out, `"key_id"`) {
		t.Fatalf("finishEventField = %s", out)
	}
	if _, ok := decodeEventObject(json.RawMessage(`[`)); ok {
		t.Fatal("decodeEventObject accepted malformed JSON")
	}

	tests := []struct {
		name string
		in   any
		want int64
	}{
		{name: "json number", in: json.Number("12"), want: 12},
		{name: "float", in: float64(13), want: 13},
		{name: "string", in: "14", want: 14},
		{name: "invalid string", in: "bad", want: 0},
		{name: "unsupported", in: true, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := jsonInt64(tc.in); got != tc.want {
				t.Fatalf("jsonInt64(%#v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}

	uint32Tests := []struct {
		name string
		in   any
		want uint32
		ok   bool
	}{
		{name: "json number", in: json.Number("12"), want: 12, ok: true},
		{name: "maximum", in: json.Number("4294967295"), want: ^uint32(0), ok: true},
		{name: "string", in: " 14 ", want: 14, ok: true},
		{name: "float", in: float64(15), want: 15, ok: true},
		{name: "zero", in: json.Number("0")},
		{name: "negative", in: json.Number("-1")},
		{name: "oversized", in: json.Number("4294967296")},
		{name: "fractional", in: float64(1.5)},
		{name: "unsupported", in: true},
	}
	for _, tc := range uint32Tests {
		t.Run("uint32 "+tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := jsonUint32(tc.in)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("jsonUint32(%#v) = %d/%v, want %d/%v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}

	for _, tc := range []struct {
		name string
		chat netproto.ChatBroadcast
		want int64
	}{
		{name: "global", chat: netproto.ChatBroadcast{}, want: 0},
		{name: "channel", chat: netproto.ChatBroadcast{ChannelID: "42"}, want: 42},
		{name: "invalid", chat: netproto.ChatBroadcast{ChannelID: "bad"}, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := scopeOf(tc.chat); got != tc.want {
				t.Fatalf("scopeOf(%q) = %d, want %d", tc.chat.ChannelID, got, tc.want)
			}
		})
	}
}

// TestIdentityX25519Upgrade verifies an old identity.json (no X25519 fields)
// is upgraded in place and the pair decodes.
func TestIdentityX25519Upgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	// Old-format identity: Ed25519 fields only.
	old := `{"public_key":"-----BEGIN PUBLIC KEY-----\nx\n-----END PUBLIC KEY-----","private_key":"-----BEGIN PRIVATE KEY-----\ny\n-----END PRIVATE KEY-----"}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	id, err := loadOrCreateIdentityAt(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if id.X25519Public == "" || id.X25519Private == "" {
		t.Fatal("old identity not upgraded with X25519 pair")
	}
	if _, _, err := id.x25519(); err != nil {
		t.Fatalf("x25519 decode: %v", err)
	}
	// Reload: the upgrade persisted.
	id2, err := loadOrCreateIdentityAt(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if id2.X25519Public != id.X25519Public {
		t.Fatal("X25519 key changed across reload (upgrade not persisted)")
	}
}

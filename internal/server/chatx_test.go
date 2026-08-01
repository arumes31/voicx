// chatx_test.go covers the wave-5a server-side chat features: history,
// edit/delete, pins, reactions, slow mode, rate limiting, anti-spam,
// word/link filters, mentions, typing, DM receipts, custom emoji, and
// MOTD/announcement.
package server

import (
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"voicx/internal/config"
	"voicx/internal/netproto"
	"voicx/internal/permissions"
)

// chatPair sets up two authed, key-published clients in channel 1 and
// returns them, bob's unsealed scope key, its generation, and bob's X25519
// public key (needed with the private key from testX25519 to unseal further keys).
func chatPair(t *testing.T, env *testEnv) (alice, bob net.Conn, scopeKey [32]byte, keyID uint32, bobPriv [32]byte) {
	t.Helper()
	alice, _ = dialAuthed(t, env.addr, "admin-uid")
	bob, _ = dialAuthed(t, env.addr, "user-uid")
	apub, _ := testX25519(t)
	bpub, bpriv := testX25519(t)
	publishKey(t, alice, apub)
	publishKey(t, bob, bpub)

	env.state.AddChannel(testChannel(1))
	send(t, alice, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 1})
	readChannelKeyFor(t, alice, 1)
	send(t, bob, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 1})
	ck := readChannelKeyFor(t, bob, 1)
	copy(scopeKey[:], unseal(t, ck, bpub, bpriv))
	return alice, bob, scopeKey, ck.KeyID, bpriv
}

// sendEncChat seals and sends a channel message from conn.
func sendEncChat(t *testing.T, conn net.Conn, key [32]byte, keyID uint32, channelID, text string) {
	t.Helper()
	send(t, conn, netproto.MsgChatSend, netproto.ChatSend{
		ChannelID: channelID, Text: sealScopeTest(t, key, text), Enc: true, KeyID: keyID,
	})
}

// TestChatHistoryEncryptedRoundTrip verifies history ships CIPHERTEXT plus
// the generations that page references, and that a member can open it with
// nothing but its own X25519 private key (103/91).
func TestChatHistoryEncryptedRoundTrip(t *testing.T) {
	const canary = "canary-7f3a"
	env := startTestEnv(t, nil)
	defer env.stop()
	alice, bob, key, keyID, bobPriv := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()
	bobPub := x25519Pub(bobPriv)

	sendEncChat(t, bob, key, keyID, "1", canary)

	// Wait for the broadcast (implies processing) then query history.
	readEventOfType(t, bob, eventChat)
	send(t, bob, netproto.MsgChatHistory, netproto.ChatHistory{ChannelID: 1})
	f := readOfType(t, bob, netproto.MsgChatHistoryResponse)
	var resp netproto.ChatHistoryResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if len(resp.Messages) != 1 {
		t.Fatalf("history size = %d, want 1", len(resp.Messages))
	}
	m := resp.Messages[0]
	if m.Body != "" || m.BodyEnc == "" || m.KeyID == 0 || m.FromUniqueID != "user-uid" || m.Deleted || m.EditedAt != 0 {
		t.Fatalf("history entry = %+v", m)
	}
	if len(resp.Keys) != 1 || resp.Keys[0].KeyID != m.KeyID || resp.Truncated || len(resp.Refused) != 0 {
		t.Fatalf("piggybacked keys = %+v truncated=%v refused=%v", resp.Keys, resp.Truncated, resp.Refused)
	}
	gen := unsealScopeKey(t, resp.Keys[0], bobPub, bobPriv)
	if got := openScopeTest(t, gen, m.BodyEnc); got != canary {
		t.Fatalf("history body = %q, want %q", got, canary)
	}

	// A non-member may not read the channel's history.
	conn3, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn3.Close()
	env.state.AddChannel(testChannel(2))
	send(t, conn3, netproto.MsgChatHistory, netproto.ChatHistory{ChannelID: 2})
	// user-uid is in channel 1, asking for channel 2 → denied... but conn3 is
	// a second connection of user-uid, still in channel 1.
	f = readOfType(t, conn3, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("error = %d, want permission denied", e.Code)
	}
}

// TestChatPinsRequiresMembership verifies pins are gated exactly like
// history: without it any authenticated client, guest included, enumerates
// the ids, authors, timestamps and bodies of a channel it never joined.
func TestChatPinsRequiresMembership(t *testing.T) {
	env := startTestEnv(t, permsWithPin())
	defer env.stop()
	alice, bob, key, keyID, _ := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	sendEncChat(t, bob, key, keyID, "1", "pin me")
	data := readEventOfType(t, alice, eventChat)
	var chat netproto.ChatBroadcast
	if err := json.Unmarshal(data, &chat); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}
	send(t, alice, netproto.MsgChatPin, netproto.ChatPin{ChannelID: 1, MessageID: chat.ID, Pinned: true})
	readEventOfType(t, alice, eventChatPinned)

	// An outsider joins channel 2 and asks for channel 1's pins.
	env.state.AddChannel(testChannel(2))
	outsider, _ := dialAuthed(t, env.addr, "admin-uid")
	defer outsider.Close()
	opub, _ := testX25519(t)
	publishKey(t, outsider, opub)
	send(t, outsider, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 2})
	readChannelKeyFor(t, outsider, 2)

	send(t, outsider, netproto.MsgChatPins, netproto.ChatPins{ChannelID: 1})
	f := readOfType(t, outsider, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("pins for a foreign channel: error = %d, want permission denied", e.Code)
	}
}

// TestHistoryRefusedWithoutPublishedKey verifies a client that never
// published an X25519 key is refused outright rather than handed rows it
// provably cannot decrypt.
func TestHistoryRefusedWithoutPublishedKey(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgChatHistory, netproto.ChatHistory{ChannelID: 0})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied || !strings.Contains(e.Message, "publish an encryption key") {
		t.Fatalf("error = %d %q, want permission denied about publishing a key", e.Code, e.Message)
	}
}

// TestHistoryAcrossRotation verifies one history fetch spanning a rotation
// returns BOTH generations in Keys and that both bodies decrypt.
func TestHistoryAcrossRotation(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	alice, bob, key1, gen1, bobPriv := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()
	bobPub := x25519Pub(bobPriv)

	sendEncChat(t, bob, key1, gen1, "1", "before rotation")
	readEventOfType(t, bob, eventChat)

	env.srv.doRotateScopeKey(t.Context(), 1)
	ck2 := readChannelKeyFor(t, bob, 1)
	key2 := unsealScopeKey(t, ck2, bobPub, bobPriv)
	if ck2.KeyID == gen1 {
		t.Fatalf("rotation did not bump the generation (still %d)", gen1)
	}
	sendEncChat(t, bob, key2, ck2.KeyID, "1", "after rotation")
	readEventOfType(t, bob, eventChat)

	send(t, bob, netproto.MsgChatHistory, netproto.ChatHistory{ChannelID: 1})
	f := readOfType(t, bob, netproto.MsgChatHistoryResponse)
	var resp netproto.ChatHistoryResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if len(resp.Messages) != 2 || len(resp.Keys) != 2 {
		t.Fatalf("history = %d messages, %d keys; want 2 and 2", len(resp.Messages), len(resp.Keys))
	}
	keys := map[uint32][32]byte{}
	for _, ck := range resp.Keys {
		keys[ck.KeyID] = unsealScopeKey(t, ck, bobPub, bobPriv)
	}
	got := map[string]bool{}
	for _, m := range resp.Messages {
		k, ok := keys[m.KeyID]
		if !ok {
			t.Fatalf("history references generation %d that the bundle does not carry", m.KeyID)
		}
		got[openScopeTest(t, k, m.BodyEnc)] = true
	}
	if !got["before rotation"] || !got["after rotation"] {
		t.Fatalf("decrypted history = %v", got)
	}
}

// TestKeyBundleTruncationIsNotRefusal verifies a page spanning more than 64
// generations sets Truncated and does NOT list the surplus in Refused —
// conflating the two turns a transient cap into a permanent "[missing key]".
func TestKeyBundleTruncationIsNotRefusal(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	alice, bob, _, _, _ := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	ctx := t.Context()
	for i := 0; i < maxKeysPerResponse+6; i++ {
		gen, _, err := env.srv.chatKeys.rotate(ctx, 1)
		if err != nil {
			t.Fatalf("rotate %d: %v", i, err)
		}
		if _, err := env.chat.StoreChatMessage(ctx, 1, "user-uid", "user", "ct", gen); err != nil {
			t.Fatalf("store: %v", err)
		}
	}

	send(t, bob, netproto.MsgChatHistory, netproto.ChatHistory{ChannelID: 1, Limit: 200})
	f := readOfType(t, bob, netproto.MsgChatHistoryResponse)
	var resp netproto.ChatHistoryResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if len(resp.Keys) != maxKeysPerResponse {
		t.Fatalf("keys = %d, want the %d cap", len(resp.Keys), maxKeysPerResponse)
	}
	if !resp.Truncated {
		t.Fatal("the surplus generations were dropped without setting Truncated")
	}
	if len(resp.Refused) != 0 {
		t.Fatalf("truncation reported as refusal: %v", resp.Refused)
	}
}

// TestPlaintextEscapeHatchStillStoresAndRelaysCiphertext verifies the branch
// where the SERVER is the encryptor: chat_allow_plaintext lets an unencrypted
// send in, and both the stored row and the relay are still ciphertext (91).
func TestPlaintextEscapeHatchStillStoresAndRelaysCiphertext(t *testing.T) {
	const canary = "canary-7f3a"
	env := startTestEnv(t, nil) // the harness sets ChatAllowPlaintext
	defer env.stop()
	alice, bob, key, _, _ := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	send(t, bob, netproto.MsgChatSend, netproto.ChatSend{ChannelID: "1", Text: canary})
	data := readEventOfType(t, alice, eventChat)
	var chat netproto.ChatBroadcast
	if err := json.Unmarshal(data, &chat); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}
	if !chat.Enc || chat.KeyID == 0 {
		t.Fatalf("relay enc/key = %v/%d, want true and a real generation", chat.Enc, chat.KeyID)
	}
	if chat.Text == canary {
		t.Fatal("the relay fanned out plaintext")
	}
	if got := openScopeTest(t, key, chat.Text); got != canary {
		t.Fatalf("relayed ciphertext opens to %q, want %q", got, canary)
	}
	stored, err := env.chat.GetChatMessage(t.Context(), chat.ID)
	if err != nil || stored == nil {
		t.Fatalf("stored message lookup: %v", err)
	}
	if stored.BodyEnc != chat.Text || stored.KeyID != chat.KeyID {
		t.Fatalf("stored row = %+v, want the relayed ciphertext", stored)
	}
}

// TestStorageFailureIsNotRelayed verifies a storage error fails the send:
// relaying anyway would turn a constraint violation into invisible,
// indefinite history loss.
func TestStorageFailureIsNotRelayed(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	alice, bob, key, keyID, _ := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	env.chat.failStores(errors.New("chat_messages_no_plaintext violated"))
	sendEncChat(t, bob, key, keyID, "1", "never stored")

	f := readOfType(t, bob, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodeUnavailable {
		t.Fatalf("error = %d %q, want unavailable", e.Code, e.Message)
	}
	if env.chat.messageCount() != 0 {
		t.Fatalf("messages stored = %d, want 0", env.chat.messageCount())
	}
	expectNoEvent(t, alice, eventChat)
}

// TestEditRejectsStaleKeyID verifies an edit naming a retired generation is
// refused up front rather than failing confusingly deeper in the pipeline.
func TestEditRejectsStaleKeyID(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	alice, bob, key, keyID, bobPriv := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	sendEncChat(t, bob, key, keyID, "1", "original")
	data := readEventOfType(t, bob, eventChat)
	var chat netproto.ChatBroadcast
	if err := json.Unmarshal(data, &chat); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}

	env.srv.doRotateScopeKey(t.Context(), 1)
	readChannelKeyFor(t, bob, 1)
	_ = bobPriv

	send(t, bob, netproto.MsgChatEdit, netproto.ChatEdit{
		MessageID: chat.ID, NewText: sealScopeTest(t, key, "edited"), Enc: true, KeyID: keyID,
	})
	f := readOfType(t, bob, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if !strings.HasPrefix(e.Message, "stale chat key") {
		t.Fatalf("error = %q, want a stale-key rejection", e.Message)
	}
}

// TestEditStoresCiphertextVerbatim verifies the sender's own bytes reach the
// store and the broadcast unchanged: the bytes that were moderated are
// exactly the bytes that end up at rest.
func TestEditStoresCiphertextVerbatim(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	alice, bob, key, keyID, _ := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	sendEncChat(t, bob, key, keyID, "1", "original")
	data := readEventOfType(t, bob, eventChat)
	var chat netproto.ChatBroadcast
	if err := json.Unmarshal(data, &chat); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}

	newCT := sealScopeTest(t, key, "edited body")
	send(t, bob, netproto.MsgChatEdit, netproto.ChatEdit{
		MessageID: chat.ID, NewText: newCT, Enc: true, KeyID: keyID,
	})
	edited := readEventOfType(t, alice, eventChatEdited)
	var ev struct {
		Body  string `json:"body"`
		Enc   bool   `json:"enc"`
		KeyID uint32 `json:"key_id"`
	}
	if err := json.Unmarshal(edited, &ev); err != nil {
		t.Fatalf("unmarshal edit: %v", err)
	}
	if ev.Body != newCT || !ev.Enc || ev.KeyID != keyID {
		t.Fatalf("edit broadcast = %+v, want the caller's bytes under generation %d", ev, keyID)
	}
	stored, err := env.chat.GetChatMessage(t.Context(), chat.ID)
	if err != nil || stored == nil {
		t.Fatalf("stored message lookup: %v", err)
	}
	if stored.BodyEnc != newCT || stored.KeyID != keyID {
		t.Fatalf("stored row = %q/%d, want the caller's bytes under %d", stored.BodyEnc, stored.KeyID, keyID)
	}
}

// TestModerationPipelineOrderUnchanged asserts the invariant the moderation
// tests cannot: the filters (117/118), the spam heuristic (116) and mention
// parsing (105) all run on the SAME decrypted body, in that order, strictly
// before storage — and none of them reads from the store.
func TestModerationPipelineOrderUnchanged(t *testing.T) {
	env := startTestEnvFull(t, nil, func(c *config.Config) { c.ChatWordFilter = "forbidden" })
	defer env.stop()
	alice, bob, key, keyID, _ := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	// A body the word filter rejects must never reach the spam tracker (which
	// runs after it) nor the store (which runs after both).
	rejected := "a forbidden word @admin [file:ab.vcx#SECRETKEY#photo.png]"
	sendEncChat(t, bob, key, keyID, "1", rejected)
	expectChatError(t, bob, "word filter")
	if env.chat.messageCount() != 0 {
		t.Fatalf("a filtered message reached the store")
	}
	if _, seen := env.srv.chatSpam.recent["user-uid"]; seen {
		t.Fatal("the spam tracker ran before the word filter")
	}

	// A clean body: the spam tracker sees the attachment-stripped plaintext,
	// mentions are parsed from the same plaintext, and only then is it stored.
	clean := "hello @admin [file:ab.vcx#SECRETKEY#photo.png]"
	sendEncChat(t, bob, key, keyID, "1", clean)
	data := readEventOfType(t, alice, eventChat)
	var chat netproto.ChatBroadcast
	if err := json.Unmarshal(data, &chat); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}
	if len(chat.Mentions) != 1 || chat.Mentions[0] != "admin-uid" {
		t.Fatalf("mentions = %v, want [admin-uid] parsed from the decrypted body", chat.Mentions)
	}
	entries := env.srv.chatSpam.recent["user-uid"]
	if len(entries) != 1 || entries[0].sum != bodyDigest(stripAttachmentRefs(clean)) {
		t.Fatalf("spam tracker recorded %d entries, want one digest of the stripped body", len(entries))
	}
	if env.chat.messageCount() != 1 {
		t.Fatalf("messages stored = %d, want 1", env.chat.messageCount())
	}
}

// TestChatEditDelete verifies own-message edit (101) and delete (102) with
// events and tombstones, plus the b_chat_delete_any gate.
func TestChatEditDelete(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	alice, bob, key, keyID, _ := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	sendEncChat(t, bob, key, keyID, "1", "original")
	data := readEventOfType(t, alice, eventChat)
	var chat netproto.ChatBroadcast
	if err := json.Unmarshal(data, &chat); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}
	msgID := chat.ID

	// Edit by a non-owner is denied.
	send(t, alice, netproto.MsgChatEdit, netproto.ChatEdit{
		MessageID: msgID, NewText: sealScopeTest(t, key, "hijacked"), Enc: true, KeyID: keyID,
	})
	f := readOfType(t, alice, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("non-owner edit: error = %d, want permission denied", e.Code)
	}

	// Owner edits → chat_edited with re-sealed body + stored update.
	send(t, bob, netproto.MsgChatEdit, netproto.ChatEdit{
		MessageID: msgID, NewText: sealScopeTest(t, key, "edited body"), Enc: true, KeyID: keyID,
	})
	data = readEventOfType(t, alice, "chat_edited")
	var edit struct {
		MessageID int64  `json:"message_id"`
		Body      string `json:"body"`
		Enc       bool   `json:"enc"`
	}
	if err := json.Unmarshal(data, &edit); err != nil {
		t.Fatalf("unmarshal chat_edited: %v", err)
	}
	if edit.MessageID != msgID || !edit.Enc {
		t.Fatalf("chat_edited = %+v", edit)
	}
	env.chat.mu.Lock()
	stored := env.chat.messages[msgID]
	env.chat.mu.Unlock()
	if stored.BodyEnc == "" || stored.EditedAt == nil {
		t.Fatalf("stored after edit = %+v", stored)
	}

	// Delete by a non-owner non-admin is denied: alice (admin) sends a
	// message, bob (no permissions) tries to delete it. Read alice's message
	// specifically (bob's own echoes may still be buffered).
	sendEncChat(t, alice, key, keyID, "1", "admin's message")
	data = readChatFrom(t, bob, "admin")
	if err := json.Unmarshal(data, &chat); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}
	send(t, bob, netproto.MsgChatDelete, netproto.ChatDelete{MessageID: chat.ID})
	f = readOfType(t, bob, netproto.MsgError)
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("non-owner delete: error = %d, want permission denied", e.Code)
	}

	// Owner deletes → chat_deleted + tombstone.
	send(t, bob, netproto.MsgChatDelete, netproto.ChatDelete{MessageID: msgID})
	data = readEventOfType(t, alice, "chat_deleted")
	var del struct {
		MessageID int64 `json:"message_id"`
	}
	if err := json.Unmarshal(data, &del); err != nil {
		t.Fatalf("unmarshal chat_deleted: %v", err)
	}
	if del.MessageID != msgID {
		t.Fatalf("chat_deleted = %+v", del)
	}
	env.chat.mu.Lock()
	stored = env.chat.messages[msgID]
	env.chat.mu.Unlock()
	if stored.BodyEnc != "" || stored.DeletedAt == nil {
		t.Fatalf("stored after delete = %+v", stored)
	}
}

// TestChatDeleteAny verifies b_chat_delete_any lets a moderator delete
// others' messages.
func TestChatDeleteAny(t *testing.T) {
	perms := tieredWith(boolPerm(permissions.PermissionKeyChatDeleteAny, true))
	env := startTestEnv(t, &perms)
	defer env.stop()
	alice, bob, key, keyID, _ := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	sendEncChat(t, bob, key, keyID, "1", "delete me")
	data := readEventOfType(t, alice, eventChat)
	var chat netproto.ChatBroadcast
	if err := json.Unmarshal(data, &chat); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}

	send(t, alice, netproto.MsgChatDelete, netproto.ChatDelete{MessageID: chat.ID})
	readEventOfType(t, bob, "chat_deleted")
}

// TestSlowMode verifies slow mode rejects a quick second message with
// retry-after info, and the bypass permission skips it (114).
func TestSlowMode(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	alice, bob, key, _, bpriv := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	env.state.AddChannel(testChannel(3))
	if ch, ok := env.state.GetChannel(3); ok {
		ch.SlowModeSeconds = 5
	}
	// Move both to the slow channel; re-key for the new scope.
	send(t, alice, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 3})
	readChannelKeyFor(t, alice, 3)
	send(t, bob, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 3})
	ck := readChannelKeyFor(t, bob, 3)
	// bob published earlier; the new scope key is sealed to the same pubkey.
	copy(key[:], unseal(t, ck, x25519Pub(bpriv), bpriv))

	sendEncChat(t, bob, key, ck.KeyID, "3", "first")
	readEventOfType(t, alice, eventChat)
	sendEncChat(t, bob, key, ck.KeyID, "3", "second too fast")
	f := readOfType(t, bob, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if !strings.Contains(e.Message, "slow mode") {
		t.Fatalf("error = %q, want slow mode rejection", e.Message)
	}
}

// readChatOutcome reads frames until a chat event (accepted) or an error
// (rejected) arrives for the sender, skipping unrelated events (user_moved
// etc.). It returns the error message ("" when the message was accepted).
func readChatOutcome(t *testing.T, conn net.Conn) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f := readFrame(t, conn)
		switch netproto.MessageType(f.Type) {
		case netproto.MsgError:
			var e netproto.Error
			if err := netproto.Decode(f, &e); err != nil {
				t.Fatalf("decode error: %v", err)
			}
			return e.Message
		case netproto.MsgEvent:
			var env struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(f.Payload, &env); err == nil && env.Type == "chat" {
				return ""
			}
		}
	}
	t.Fatal("no chat outcome frame")
	return ""
}

// TestChatRateLimit verifies the per-user token bucket rejects bursts (115).
func TestChatRateLimit(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	alice, bob, key, keyID, _ := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	// Default bucket: 5 messages per 3s — the 6th is rejected.
	rejected := false
	for i := 0; i < 6; i++ {
		sendEncChat(t, bob, key, keyID, "1", "burst")
		if msg := readChatOutcome(t, bob); strings.Contains(msg, "rate limit") {
			rejected = true
		}
	}
	if !rejected {
		t.Fatal("burst of 6 messages was not rate limited")
	}
}

// TestChatSpam verifies the third identical message in 30s is rejected (116).
func TestChatSpam(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	alice, bob, key, keyID, _ := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	rejected := false
	for i := 0; i < 3; i++ {
		sendEncChat(t, bob, key, keyID, "1", "same same same")
		if msg := readChatOutcome(t, bob); strings.Contains(msg, "spam") {
			rejected = true
		}
	}
	if !rejected {
		t.Fatal("third identical message was not rejected as spam")
	}
}

// TestWordAndLinkFilters verifies word filter and link blacklist/whitelist
// (117/118).
func TestWordAndLinkFilters(t *testing.T) {
	env := startTestEnvFull(t, nil, func(c *config.Config) {
		c.ChatWordFilter = "badword"
		c.ChatLinkBlacklist = "evil.example"
		c.ChatLinkWhitelist = "good.example"
	})
	defer env.stop()
	alice, bob, key, keyID, _ := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	sendEncChat(t, bob, key, keyID, "1", "this has BadWord in it")
	expectChatError(t, bob, "word filter")
	sendEncChat(t, bob, key, keyID, "1", "visit http://evil.example/x")
	expectChatError(t, bob, "blocked link")
	sendEncChat(t, bob, key, keyID, "1", "visit http://other.example/x")
	expectChatError(t, bob, "allowed domains")
	// A whitelisted link passes.
	sendEncChat(t, bob, key, keyID, "1", "visit http://good.example/x")
	readEventOfType(t, alice, eventChat)
}

func expectChatError(t *testing.T, conn net.Conn, want string) {
	t.Helper()
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if !strings.Contains(e.Message, want) {
		t.Fatalf("error = %q, want substring %q", e.Message, want)
	}
}

// TestMentions verifies @nickname, @channel, and the @everyone gate (105).
func TestMentions(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	alice, bob, key, keyID, _ := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	// @nickname (bob's nickname is "user").
	sendEncChat(t, alice, key, keyID, "1", "hey @user ping")
	data := readEventOfType(t, bob, eventChat)
	var chat netproto.ChatBroadcast
	if err := json.Unmarshal(data, &chat); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}
	if len(chat.Mentions) != 1 || chat.Mentions[0] != "user-uid" {
		t.Fatalf("mentions = %v, want [user-uid]", chat.Mentions)
	}

	// @channel mentions all members except the sender.
	sendEncChat(t, alice, key, keyID, "1", "@channel meeting now")
	data = readEventOfType(t, bob, eventChat)
	if err := json.Unmarshal(data, &chat); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}
	if len(chat.Mentions) != 1 || chat.Mentions[0] != "user-uid" {
		t.Fatalf("@channel mentions = %v, want [user-uid]", chat.Mentions)
	}

	// @everyone without b_chat_mention_all: no broadcast mention. Match the
	// event by sender (alice's own earlier echoes may still be buffered).
	// Fresh struct: absent omitempty fields keep stale values on reuse.
	sendEncChat(t, bob, key, keyID, "1", "@everyone listen")
	data = readChatFrom(t, alice, "user")
	var chat2 netproto.ChatBroadcast
	if err := json.Unmarshal(data, &chat2); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}
	if len(chat2.Mentions) != 0 {
		t.Fatalf("@everyone without permission mentioned %v", chat2.Mentions)
	}
}

// readChatFrom reads chat events until one arrives whose From nickname
// matches, skipping the reader's own earlier echoes.
func readChatFrom(t *testing.T, conn net.Conn, from string) json.RawMessage {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data := readEventOfType(t, conn, eventChat)
		var chat netproto.ChatBroadcast
		if err := json.Unmarshal(data, &chat); err != nil {
			t.Fatalf("unmarshal chat: %v", err)
		}
		if chat.From == from {
			return data
		}
	}
	t.Fatalf("no chat event from %q", from)
	return nil
}

// TestChatPins verifies the pin gate, event, and listing (109).
func TestChatPins(t *testing.T) {
	perms := tieredWith(boolPerm(permissions.PermissionKeyChannelModify, true))
	env := startTestEnv(t, &perms)
	defer env.stop()
	alice, bob, key, keyID, _ := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	sendEncChat(t, bob, key, keyID, "1", "pin me")
	data := readEventOfType(t, alice, eventChat)
	var chat netproto.ChatBroadcast
	if err := json.Unmarshal(data, &chat); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}

	// admin-uid is admin (bypasses); user-uid has b_channel_modify via perms.
	send(t, alice, netproto.MsgChatPin, netproto.ChatPin{ChannelID: 1, MessageID: chat.ID, Pinned: true})
	readEventOfType(t, bob, "chat_pinned")

	send(t, bob, netproto.MsgChatPins, netproto.ChatPins{ChannelID: 1})
	f := readOfType(t, bob, netproto.MsgChatPinsResponse)
	var resp netproto.ChatPinsResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode pins: %v", err)
	}
	if len(resp.Pins) != 1 || resp.Pins[0].MessageID != chat.ID || resp.Pins[0].Message.BodyEnc == "" {
		t.Fatalf("pins = %+v", resp.Pins)
	}

	// Unpin → event + empty list.
	send(t, alice, netproto.MsgChatPin, netproto.ChatPin{ChannelID: 1, MessageID: chat.ID, Pinned: false})
	readEventOfType(t, bob, "chat_unpinned")
	send(t, bob, netproto.MsgChatPins, netproto.ChatPins{ChannelID: 1})
	f = readOfType(t, bob, netproto.MsgChatPinsResponse)
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode pins: %v", err)
	}
	if len(resp.Pins) != 0 {
		t.Fatalf("pins after unpin = %+v", resp.Pins)
	}
}

// TestChatReact verifies reaction toggling and the updated map (97), and
// that history includes reactions.
func TestChatReact(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	alice, bob, key, keyID, _ := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	sendEncChat(t, bob, key, keyID, "1", "react to me")
	data := readEventOfType(t, alice, eventChat)
	var chat netproto.ChatBroadcast
	if err := json.Unmarshal(data, &chat); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}

	send(t, alice, netproto.MsgChatReact, netproto.ChatReact{MessageID: chat.ID, Emoji: "👍"})
	data = readEventOfType(t, bob, "chat_reaction")
	var react struct {
		MessageID int64          `json:"message_id"`
		Reactions map[string]int `json:"reactions"`
		Added     bool           `json:"added"`
	}
	if err := json.Unmarshal(data, &react); err != nil {
		t.Fatalf("unmarshal chat_reaction: %v", err)
	}
	if react.MessageID != chat.ID || react.Reactions["👍"] != 1 || !react.Added {
		t.Fatalf("chat_reaction = %+v", react)
	}

	// Toggle again → removed. Fresh struct: JSON unmarshal merges into
	// existing maps, so reuse would keep the stale 👍:1 entry.
	send(t, alice, netproto.MsgChatReact, netproto.ChatReact{MessageID: chat.ID, Emoji: "👍"})
	data = readEventOfType(t, bob, "chat_reaction")
	var react2 struct {
		MessageID int64          `json:"message_id"`
		Reactions map[string]int `json:"reactions"`
		Added     bool           `json:"added"`
	}
	if err := json.Unmarshal(data, &react2); err != nil {
		t.Fatalf("unmarshal chat_reaction: %v", err)
	}
	if react2.Added || react2.Reactions["👍"] != 0 {
		t.Fatalf("after toggle off = %+v", react2)
	}
}

// TestTyping verifies typing indicators relay to channel members (120).
func TestTyping(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	alice, bob, _, _, _ := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	send(t, bob, netproto.MsgTyping, netproto.Typing{ChannelID: 1})
	data := readEventOfType(t, alice, "typing")
	var ev struct {
		UniqueID string `json:"unique_id"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("unmarshal typing: %v", err)
	}
	if ev.UniqueID != "user-uid" {
		t.Fatalf("typing from = %q, want user-uid", ev.UniqueID)
	}
}

// TestDMReceipts verifies delivery and read receipts relay to the DM sender
// (124).
func TestDMReceipts(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	alice, bob, _, _, _ := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	send(t, bob, netproto.MsgChatDelivered, netproto.ChatDelivered{ToUniqueID: "admin-uid", ClientMsgID: "ref-1"})
	data := readEventOfType(t, alice, "dm_delivered")
	var ev struct {
		FromUniqueID string `json:"from_unique_id"`
		ClientMsgID  string `json:"client_msg_id"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("unmarshal dm_delivered: %v", err)
	}
	if ev.FromUniqueID != "user-uid" || ev.ClientMsgID != "ref-1" {
		t.Fatalf("dm_delivered = %+v", ev)
	}

	send(t, bob, netproto.MsgChatRead, netproto.ChatRead{ToUniqueID: "admin-uid", ClientMsgID: "ref-1"})
	readEventOfType(t, alice, "dm_read")
}

// TestEmojiUpload verifies the emoji upload gate, storage, event, and list
// (96).
func TestEmojiUpload(t *testing.T) {
	perms := tieredWith(boolPerm(permissions.PermissionKeyEmojiManage, true))
	env := startTestEnv(t, &perms)
	defer env.stop()
	alice, bob, _, _, _ := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	// Without the permission (admin bypasses, so test denial on a fresh env).
	env2 := startTestEnv(t, nil)
	defer env2.stop()
	c2, _ := dialAuthed(t, env2.addr, "user-uid")
	defer c2.Close()
	send(t, c2, netproto.MsgEmojiUpload, netproto.EmojiUpload{Name: "party", DataBase64: b64e(tinyPNG)})
	expectChatError(t, c2, "insufficient permission")

	// With permission: upload works, event fires, list contains it.
	send(t, alice, netproto.MsgEmojiUpload, netproto.EmojiUpload{Name: "party", DataBase64: b64e(tinyPNG)})
	readEventOfType(t, bob, "emoji_added")
	send(t, bob, netproto.MsgEmojiList, netproto.EmojiList{})
	f := readOfType(t, bob, netproto.MsgEmojiListResponse)
	var resp netproto.EmojiListResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode emoji list: %v", err)
	}
	if len(resp.Emojis) != 1 || resp.Emojis[0].Name != "party" {
		t.Fatalf("emojis = %+v", resp.Emojis)
	}
}

// TestMOTDSealedInAuthResponse verifies the MOTD travels SEALED under the
// global generation, which the same response carries sealed to the client's
// X25519 key — so the client renders it before Connect() returns, with no
// extra frame and no ordering rule (133).
func TestMOTDSealedInAuthResponse(t *testing.T) {
	const canary = "canary-7f3a"
	env := startTestEnvFull(t, nil, func(c *config.Config) { c.ChatAllowPlaintext = false })
	defer env.stop()
	if err := env.srv.SetServerSettingAndAnnounce(t.Context(), "motd", canary); err != nil {
		t.Fatalf("set motd: %v", err)
	}
	if v, _, _ := env.chat.GetServerSetting(t.Context(), "motd"); v == canary {
		t.Fatal("the MOTD is stored in the clear")
	}

	pub, priv := testX25519(t)
	conn, resp := dialAuthedX25519(t, env.addr, "user-uid", pub)
	defer conn.Close()

	if !resp.MOTDEnc || resp.MOTDKeyID == 0 || resp.MOTD == canary {
		t.Fatalf("motd = %q enc=%v key=%d, want sealed", resp.MOTD, resp.MOTDEnc, resp.MOTDKeyID)
	}
	if len(resp.ChatKeys) != 1 || resp.ChatKeys[0].ChannelID != globalChatScope || resp.ChatKeys[0].KeyID != resp.MOTDKeyID {
		t.Fatalf("chat keys = %+v, want the global generation %d", resp.ChatKeys, resp.MOTDKeyID)
	}
	global := unsealScopeKey(t, resp.ChatKeys[0], pub, priv)
	if got := openScopeTest(t, global, resp.MOTD); got != canary {
		t.Fatalf("motd opens to %q, want %q", got, canary)
	}

	// A client that publishes no encryption key could not open it, so it gets
	// neither the key nor the MOTD.
	plainConn, _ := dialAuthedPlain(t, env.addr, "admin-uid")
	defer plainConn.Close()
}

// dialAuthedPlain authenticates without presenting an encryption key and
// asserts the response carries neither chat keys nor a MOTD.
func dialAuthedPlain(t *testing.T, addr, uniqueID string) (net.Conn, netproto.AuthResponse) {
	t.Helper()
	conn := dialRetry(t, addr)
	send(t, conn, netproto.MsgAuthenticate, netproto.Authenticate{Username: uniqueID, Password: "pw"})
	f := readOfType(t, conn, netproto.MsgAuthResponse)
	var resp netproto.AuthResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode auth response: %v", err)
	}
	if resp.MOTD != "" || len(resp.ChatKeys) != 0 {
		t.Fatalf("keyless client got motd=%q keys=%d, want neither", resp.MOTD, len(resp.ChatKeys))
	}
	readOfType(t, conn, netproto.MsgSnapshot)
	return conn, resp
}

// TestAnnouncementSealed verifies the announcement is sealed at rest and on
// the wire, and that the auth-time replay still reaches a client that never
// publishes a key — moving it behind key publish would drop it for all of
// them (132).
func TestAnnouncementSealed(t *testing.T) {
	const canary = "canary-7f3a"
	env := startTestEnv(t, nil)
	defer env.stop()

	pub, priv := testX25519(t)
	conn, resp := dialAuthedX25519(t, env.addr, "user-uid", pub)
	defer conn.Close()
	if len(resp.ChatKeys) != 1 {
		t.Fatalf("auth response carried %d chat keys, want 1", len(resp.ChatKeys))
	}
	global := unsealScopeKey(t, resp.ChatKeys[0], pub, priv)

	if err := env.srv.SetServerSettingAndAnnounce(t.Context(), "announcement", canary); err != nil {
		t.Fatalf("SetServerSettingAndAnnounce: %v", err)
	}
	if v, _, _ := env.chat.GetServerSetting(t.Context(), "announcement"); v == canary {
		t.Fatal("the announcement is stored in the clear")
	}

	data := readEventOfType(t, conn, eventAnnouncement)
	var ann struct {
		Text  string `json:"text"`
		Enc   bool   `json:"enc"`
		KeyID uint32 `json:"key_id"`
	}
	if err := json.Unmarshal(data, &ann); err != nil {
		t.Fatalf("unmarshal announcement: %v", err)
	}
	if !ann.Enc || ann.KeyID != resp.ChatKeys[0].KeyID {
		t.Fatalf("announcement enc/key = %v/%d, want true/%d", ann.Enc, ann.KeyID, resp.ChatKeys[0].KeyID)
	}
	if got := openScopeTest(t, global, ann.Text); got != canary {
		t.Fatalf("announcement opens to %q, want %q", got, canary)
	}

	// The replay on login still reaches a client that never published a key.
	late, _ := dialAuthed(t, env.addr, "admin-uid")
	defer late.Close()
	replay := readEventOfType(t, late, eventAnnouncement)
	var ann2 struct {
		Text  string `json:"text"`
		Enc   bool   `json:"enc"`
		KeyID uint32 `json:"key_id"`
	}
	if err := json.Unmarshal(replay, &ann2); err != nil {
		t.Fatalf("unmarshal replayed announcement: %v", err)
	}
	if !ann2.Enc || ann2.Text != ann.Text {
		t.Fatalf("replayed announcement = %+v, want the same ciphertext", ann2)
	}
}

// TestServerTextSealed verifies SendServerText seals in all three target
// modes. Mode 1 is a server notice sealed under the GLOBAL key, not an E2EE
// direct message — the server has no DM key and never did.
func TestServerTextSealed(t *testing.T) {
	const canary = "canary-7f3a"
	env := startTestEnv(t, nil)
	defer env.stop()

	apub, apriv := testX25519(t)
	alice, resp := dialAuthedX25519(t, env.addr, "admin-uid", apub)
	defer alice.Close()
	global := unsealScopeKey(t, resp.ChatKeys[0], apub, apriv)
	clientID := resp.ClientID

	publishKey(t, alice, apub)
	env.state.AddChannel(testChannel(1))
	send(t, alice, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 1})
	ck := readChannelKeyFor(t, alice, 1)
	channelKey := unsealScopeKey(t, ck, apub, apriv)

	for _, tc := range []struct {
		name   string
		mode   int
		target string
		key    [32]byte
		keyID  uint32
	}{
		{"direct notice", 1, clientID, global, resp.ChatKeys[0].KeyID},
		{"channel", 2, "1", channelKey, ck.KeyID},
		{"global", 3, "", global, resp.ChatKeys[0].KeyID},
	} {
		if err := env.srv.SendServerText(tc.mode, tc.target, canary); err != nil {
			t.Fatalf("%s: SendServerText: %v", tc.name, err)
		}
		data := readEventOfType(t, alice, eventChat)
		var chat netproto.ChatBroadcast
		if err := json.Unmarshal(data, &chat); err != nil {
			t.Fatalf("%s: unmarshal chat: %v", tc.name, err)
		}
		if !chat.Enc || chat.KeyID != tc.keyID {
			t.Fatalf("%s: enc/key = %v/%d, want true/%d", tc.name, chat.Enc, chat.KeyID, tc.keyID)
		}
		if got := openScopeTest(t, tc.key, chat.Text); got != canary {
			t.Fatalf("%s: opens to %q, want %q", tc.name, got, canary)
		}
	}
}

// TestServerInfoMOTDPlaintextIsOptIn pins the only way a MOTD can leave the
// server unsealed (313): an operator setting server_info_motd. The default
// must stay off, or the server-info reply reopens the plaintext path the rest
// of 91-135 closes.
func TestServerInfoMOTDPlaintextIsOptIn(t *testing.T) {
	const motd = "public welcome text"

	if cfg, err := config.Load(); err != nil {
		t.Fatalf("load config: %v", err)
	} else if cfg.ServerInfoMOTD {
		t.Fatal("server_info_motd defaults to on: the MOTD would be served in the clear")
	}

	env := startTestEnvFull(t, nil, func(c *config.Config) { c.ServerInfoMOTD = true })
	defer env.stop()
	if err := env.srv.SetServerSettingAndAnnounce(t.Context(), "motd", motd); err != nil {
		t.Fatalf("set motd: %v", err)
	}
	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()
	send(t, conn, netproto.MsgServerInfoQuery, netproto.ServerInfoQuery{})
	f := readOfType(t, conn, netproto.MsgServerInfoResponse)
	var info netproto.ServerInfoResponse
	if err := netproto.Decode(f, &info); err != nil {
		t.Fatalf("decode server info: %v", err)
	}
	if info.MOTD != motd {
		t.Fatalf("server info motd = %q, want the plaintext %q", info.MOTD, motd)
	}

	off := startTestEnvFull(t, nil, func(c *config.Config) { c.ServerInfoMOTD = false })
	defer off.stop()
	if err := off.srv.SetServerSettingAndAnnounce(t.Context(), "motd", motd); err != nil {
		t.Fatalf("set motd: %v", err)
	}
	conn2, _ := dialAuthed(t, off.addr, "user-uid")
	defer conn2.Close()
	send(t, conn2, netproto.MsgServerInfoQuery, netproto.ServerInfoQuery{})
	f = readOfType(t, conn2, netproto.MsgServerInfoResponse)
	var info2 netproto.ServerInfoResponse
	if err := netproto.Decode(f, &info2); err != nil {
		t.Fatalf("decode server info: %v", err)
	}
	if info2.MOTD != "" {
		t.Fatalf("server_info_motd=false still served %q", info2.MOTD)
	}
}

// TestLegacyChatBackfillSeals verifies the one-time conversion: pre-012
// plaintext is sealed in place, the audit count reaches zero, both
// constraints are validated, the marker is stamped, and a second run is a
// no-op (91).
func TestLegacyChatBackfillSeals(t *testing.T) {
	const canary = "canary-7f3a"
	env := startTestEnv(t, nil)
	defer env.stop()
	ctx := t.Context()

	env.chat.seedLegacy(1, 0, canary)
	env.chat.seedLegacy(2, 5, "") // the empty-plaintext row 012 must survive
	if err := env.chat.SetServerSetting(ctx, "motd", canary, 0); err != nil {
		t.Fatalf("seed motd: %v", err)
	}

	if err := env.srv.EncryptLegacyChatHistory(ctx, "encrypt"); err != nil {
		t.Fatalf("EncryptLegacyChatHistory: %v", err)
	}
	if n, _ := env.chat.CountPlaintextBodies(ctx); n != 0 {
		t.Fatalf("plaintext bodies left = %d, want 0", n)
	}
	if !env.chat.validated {
		t.Fatal("the plaintext constraints were never validated")
	}
	if stamp, _, _ := env.chat.GetServerSetting(ctx, backfillMarker); stamp == "" {
		t.Fatal("the backfill marker was not stamped")
	}
	motd, motdGen, _ := env.chat.GetServerSetting(ctx, "motd")
	if motd == canary || motdGen == 0 {
		t.Fatalf("motd = %q under generation %d, want sealed", motd, motdGen)
	}
	if got := env.srv.serverSettingPlain(ctx, "motd"); got != canary {
		t.Fatalf("sealed motd opens to %q, want %q", got, canary)
	}
	for _, id := range []int64{1, 2} {
		m, err := env.chat.GetChatMessage(ctx, id)
		if err != nil || m == nil {
			t.Fatalf("legacy row %d: %v", id, err)
		}
		if m.BodyEnc == "" || m.KeyID == 0 {
			t.Fatalf("legacy row %d = %+v, want sealed", id, m)
		}
		plain, err := env.srv.chatKeys.open(ctx, m.ChannelID, m.KeyID, m.BodyEnc)
		if err != nil {
			t.Fatalf("opening legacy row %d: %v", id, err)
		}
		if id == 1 && plain != canary {
			t.Fatalf("legacy row 1 opens to %q, want %q", plain, canary)
		}
	}
	if containsCanary(t, env.chat, canary) {
		t.Fatal("the chat store still holds the plaintext canary")
	}

	// A second run is a no-op that neither re-seals nor errors.
	if err := env.srv.EncryptLegacyChatHistory(ctx, "encrypt"); err != nil {
		t.Fatalf("second EncryptLegacyChatHistory: %v", err)
	}
	if m, _ := env.chat.GetChatMessage(ctx, 1); m.BodyEnc == "" {
		t.Fatal("the second run disturbed an already-sealed row")
	}
}

// TestLegacyChatBackfillPurge verifies chat_legacy_history=purge leaves zero
// plaintext AND zero ciphertext.
func TestLegacyChatBackfillPurge(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	ctx := t.Context()

	env.chat.seedLegacy(1, 0, "canary-7f3a")
	if err := env.srv.EncryptLegacyChatHistory(ctx, "purge"); err != nil {
		t.Fatalf("EncryptLegacyChatHistory(purge): %v", err)
	}
	if n, _ := env.chat.CountPlaintextBodies(ctx); n != 0 {
		t.Fatalf("plaintext bodies left = %d, want 0", n)
	}
	if m, _ := env.chat.GetChatMessage(ctx, 1); m != nil {
		t.Fatalf("purge left a row behind: %+v", m)
	}
	if stamp, _, _ := env.chat.GetServerSetting(ctx, backfillMarker); stamp == "" {
		t.Fatal("the backfill marker was not stamped")
	}
}

// expectNoEvent asserts no event of the given type arrives shortly.
func expectNoEvent(t *testing.T, conn net.Conn, want string) {
	t.Helper()
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		f, err := netproto.ReadFrame(conn)
		if err != nil {
			return // nothing more arrived
		}
		if netproto.MessageType(f.Type) != netproto.MsgEvent {
			continue
		}
		if evType, _ := decodeEvent(t, f); evType == want {
			t.Fatalf("unexpected %s event", want)
		}
	}
}

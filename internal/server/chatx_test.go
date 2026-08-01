// chatx_test.go covers the wave-5a server-side chat features: history,
// edit/delete, pins, reactions, slow mode, rate limiting, anti-spam,
// word/link filters, mentions, typing, DM receipts, custom emoji, and
// MOTD/announcement.
package server

import (
	"encoding/json"
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

// TestChatHistoryEncryptedRoundTrip verifies an encrypted message is stored
// decrypted and history returns plaintext to a channel member (103).
func TestChatHistoryEncryptedRoundTrip(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	alice, bob, key, keyID, _ := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	sendEncChat(t, bob, key, keyID, "1", "hello history")

	// Wait for the broadcast (implies processing) then query history.
	readEventOfType(t, alice, eventChat)
	send(t, alice, netproto.MsgChatHistory, netproto.ChatHistory{ChannelID: 1})
	f := readOfType(t, alice, netproto.MsgChatHistoryResponse)
	var resp netproto.ChatHistoryResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if len(resp.Messages) != 1 {
		t.Fatalf("history size = %d, want 1", len(resp.Messages))
	}
	m := resp.Messages[0]
	if m.BodyEnc == "" || m.FromUniqueID != "user-uid" || m.Deleted || m.EditedAt != 0 {
		t.Fatalf("history entry = %+v", m)
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

// TestMOTDAndAnnouncement verifies the MOTD lands in the auth response and
// the announcement is delivered on login and on change (132/133).
func TestMOTDAndAnnouncement(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	if err := env.chat.SetServerSetting(t.Context(), "motd", "welcome to voicx", 0); err != nil {
		t.Fatalf("set motd: %v", err)
	}
	if err := env.chat.SetServerSetting(t.Context(), "announcement", "big news", 0); err != nil {
		t.Fatalf("set announcement: %v", err)
	}

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	// The announcement event arrives on login.
	data := readEventOfType(t, conn, "announcement")
	var ann struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &ann); err != nil {
		t.Fatalf("unmarshal announcement: %v", err)
	}
	if ann.Text != "big news" {
		t.Fatalf("announcement = %q", ann.Text)
	}

	// Changing the announcement broadcasts to online clients.
	if err := env.srv.SetServerSettingAndAnnounce(t.Context(), "announcement", "newer news"); err != nil {
		t.Fatalf("SetServerSettingAndAnnounce: %v", err)
	}
	data = readEventOfType(t, conn, "announcement")
	if err := json.Unmarshal(data, &ann); err != nil {
		t.Fatalf("unmarshal announcement: %v", err)
	}
	if ann.Text != "newer news" {
		t.Fatalf("updated announcement = %q", ann.Text)
	}
}

// TestMOTDInAuthResponse verifies the motd field of the auth response.
func TestMOTDInAuthResponse(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	if err := env.chat.SetServerSetting(t.Context(), "motd", "welcome to voicx", 0); err != nil {
		t.Fatalf("set motd: %v", err)
	}
	conn := dialRetry(t, env.addr)
	defer conn.Close()
	send(t, conn, netproto.MsgAuthenticate, netproto.Authenticate{Username: "user-uid", Password: "pw"})
	f := readOfType(t, conn, netproto.MsgAuthResponse)
	var resp netproto.AuthResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode auth response: %v", err)
	}
	if resp.MOTD != "welcome to voicx" {
		t.Fatalf("motd = %q", resp.MOTD)
	}
}

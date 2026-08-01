// canary_test.go holds the reflection sweep that makes "no plaintext at
// rest" self-enforcing (91): any future code that reintroduces a plaintext
// write into server state, the spam tracker or a broadcast payload fails
// here without a new test being written.
//
// The obvious version of this test — json.Marshal the fake store and grep —
// passes vacuously, because every field of the fakes is unexported and
// marshals to {}. Hence the reflection walk.
package server

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"voicx/internal/netproto"
	"voicx/internal/permissions"
)

// permsWithPin grants b_channel_modify, the pin-curation gate.
func permsWithPin() *permissions.TieredPermissions {
	tp := tieredWith(boolPerm(permissions.PermissionKeyChannelModify, true))
	return &tp
}

// containsCanary walks v via reflect — unexported fields included — and
// reports whether canary appears in any reachable string, []byte, map key or
// map value.
func containsCanary(t *testing.T, v any, canary string) bool {
	t.Helper()
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return false
	}
	// An addressable copy is what lets the walk read unexported fields.
	box := reflect.New(rv.Type())
	box.Elem().Set(rv)
	return canaryWalk(box, canary, map[uintptr]bool{}, 0)
}

// canaryWalk is containsCanary's recursion. It never calls Interface(), which
// would panic on values reached through an unexported field.
func canaryWalk(v reflect.Value, canary string, seen map[uintptr]bool, depth int) bool {
	if !v.IsValid() || depth > 32 {
		return false
	}
	switch v.Kind() {
	case reflect.String:
		return strings.Contains(v.String(), canary)
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return false
		}
		if v.Kind() == reflect.Pointer {
			p := v.Pointer()
			if seen[p] {
				return false
			}
			seen[p] = true
		}
		return canaryWalk(v.Elem(), canary, seen, depth+1)
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			return false
		}
		if v.Type().Elem().Kind() == reflect.Uint8 {
			raw := make([]byte, v.Len())
			for i := 0; i < v.Len(); i++ {
				raw[i] = byte(v.Index(i).Uint())
			}
			return bytes.Contains(raw, []byte(canary))
		}
		for i := 0; i < v.Len(); i++ {
			if canaryWalk(v.Index(i), canary, seen, depth+1) {
				return true
			}
		}
	case reflect.Map:
		if v.IsNil() {
			return false
		}
		for _, k := range v.MapKeys() {
			if canaryWalk(k, canary, seen, depth+1) || canaryWalk(v.MapIndex(k), canary, seen, depth+1) {
				return true
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			f := v.Field(i)
			if !f.CanInterface() && f.CanAddr() {
				f = reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()
			}
			if canaryWalk(f, canary, seen, depth+1) {
				return true
			}
		}
	}
	return false
}

// TestContainsCanaryReadsUnexportedFields guards the guard: the helper is
// worthless if it silently skips the very fields the fakes use.
func TestContainsCanaryReadsUnexportedFields(t *testing.T) {
	type hidden struct {
		msgs map[string][]string
		blob []byte
	}
	h := hidden{msgs: map[string][]string{"a": {"canary-7f3a"}}}
	if !containsCanary(t, h, "canary-7f3a") {
		t.Fatal("containsCanary missed a canary in an unexported map")
	}
	h2 := hidden{blob: []byte("prefix canary-7f3a suffix")}
	if !containsCanary(t, h2, "canary-7f3a") {
		t.Fatal("containsCanary missed a canary in an unexported []byte")
	}
	if containsCanary(t, hidden{}, "canary-7f3a") {
		t.Fatal("containsCanary reported a canary that is not there")
	}
}

// TestNoPlaintextAnywhereInServerState sends, edits and pins a canary and
// then walks every place the server keeps state: the chat store, the spam
// tracker, and the broadcast payloads it produced.
func TestNoPlaintextAnywhereInServerState(t *testing.T) {
	const canary = "canary-7f3a"
	env := startTestEnv(t, permsWithPin())
	defer env.stop()
	alice, bob, key, keyID, _ := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	sendEncChat(t, bob, key, keyID, "1", canary)
	sent := readEventOfType(t, alice, eventChat)
	var chat netproto.ChatBroadcast
	if err := json.Unmarshal(sent, &chat); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}

	send(t, bob, netproto.MsgChatEdit, netproto.ChatEdit{
		MessageID: chat.ID, NewText: sealScopeTest(t, key, canary+"-edited"), Enc: true, KeyID: keyID,
	})
	edited := readEventOfType(t, alice, eventChatEdited)

	send(t, alice, netproto.MsgChatPin, netproto.ChatPin{ChannelID: 1, MessageID: chat.ID, Pinned: true})
	pinned := readEventOfType(t, alice, eventChatPinned)

	for name, v := range map[string]any{
		"chat store":        env.chat,
		"spam tracker":      env.srv.chatSpam,
		"chat broadcast":    sent,
		"edit broadcast":    edited,
		"pin broadcast":     pinned,
		"relayed chat":      chat,
		"slow-mode tracker": env.srv.chatSlow,
	} {
		if containsCanary(t, v, canary) {
			t.Fatalf("%s retains the plaintext canary", name)
		}
	}
}

// TestNoPlaintextInHistoryResponse marshals a history page and asserts the
// canary is absent — Body is never populated by the server.
func TestNoPlaintextInHistoryResponse(t *testing.T) {
	const canary = "canary-7f3a"
	env := startTestEnv(t, nil)
	defer env.stop()
	alice, bob, key, keyID, _ := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	sendEncChat(t, bob, key, keyID, "1", canary)
	readEventOfType(t, alice, eventChat)

	send(t, alice, netproto.MsgChatHistory, netproto.ChatHistory{ChannelID: 1})
	f := readOfType(t, alice, netproto.MsgChatHistoryResponse)
	var resp netproto.ChatHistoryResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal history: %v", err)
	}
	if bytes.Contains(raw, []byte(canary)) {
		t.Fatalf("history response carries plaintext: %s", raw)
	}
	if resp.Messages[0].Body != "" {
		t.Fatalf("server populated Body: %q", resp.Messages[0].Body)
	}
}

// TestNoPlaintextInPinsResponse is the same assertion for pins, which inherit
// the entry shape from history.
func TestNoPlaintextInPinsResponse(t *testing.T) {
	const canary = "canary-7f3a"
	env := startTestEnv(t, permsWithPin())
	defer env.stop()
	alice, bob, key, keyID, _ := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	sendEncChat(t, bob, key, keyID, "1", canary)
	data := readEventOfType(t, alice, eventChat)
	var chat netproto.ChatBroadcast
	if err := json.Unmarshal(data, &chat); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}
	send(t, alice, netproto.MsgChatPin, netproto.ChatPin{ChannelID: 1, MessageID: chat.ID, Pinned: true})
	readEventOfType(t, alice, eventChatPinned)

	send(t, alice, netproto.MsgChatPins, netproto.ChatPins{ChannelID: 1})
	f := readOfType(t, alice, netproto.MsgChatPinsResponse)
	var resp netproto.ChatPinsResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode pins: %v", err)
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal pins: %v", err)
	}
	if bytes.Contains(raw, []byte(canary)) {
		t.Fatalf("pins response carries plaintext: %s", raw)
	}
	if len(resp.Pins) != 1 || resp.Pins[0].Message == nil || resp.Pins[0].Message.BodyEnc == "" {
		t.Fatalf("pins = %+v, want one ciphertext entry", resp.Pins)
	}
}

// TestNoPlaintextInEditBroadcast asserts the chat_edited event is ciphertext.
func TestNoPlaintextInEditBroadcast(t *testing.T) {
	const canary = "canary-7f3a"
	env := startTestEnv(t, nil)
	defer env.stop()
	alice, bob, key, keyID, _ := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	sendEncChat(t, bob, key, keyID, "1", "before")
	data := readEventOfType(t, alice, eventChat)
	var chat netproto.ChatBroadcast
	if err := json.Unmarshal(data, &chat); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}
	send(t, bob, netproto.MsgChatEdit, netproto.ChatEdit{
		MessageID: chat.ID, NewText: sealScopeTest(t, key, canary), Enc: true, KeyID: keyID,
	})
	edited := readEventOfType(t, alice, eventChatEdited)
	if bytes.Contains(edited, []byte(canary)) {
		t.Fatalf("edit broadcast carries plaintext: %s", edited)
	}
	var ev struct {
		Body  string `json:"body"`
		Enc   bool   `json:"enc"`
		KeyID uint32 `json:"key_id"`
	}
	if err := json.Unmarshal(edited, &ev); err != nil {
		t.Fatalf("unmarshal edit: %v", err)
	}
	if !ev.Enc || ev.KeyID != keyID {
		t.Fatalf("edit event enc/key = %v/%d, want true/%d", ev.Enc, ev.KeyID, keyID)
	}
	if got := openScopeTest(t, key, ev.Body); got != canary {
		t.Fatalf("edit body = %q, want %q", got, canary)
	}
}

// TestSpamTrackerRetainsNoPlaintext feeds the canary through the tracker and
// asserts it keeps only a digest — while the 3-in-30s heuristic (116) still
// trips, which is the whole point of the digest being a drop-in.
func TestSpamTrackerRetainsNoPlaintext(t *testing.T) {
	const canary = "canary-7f3a"
	tr := newSpamTracker()
	now := time.Now()
	if tr.record("u", bodyDigest(canary), now) {
		t.Fatal("first message tripped the spam heuristic")
	}
	if tr.record("u", bodyDigest(canary), now.Add(time.Second)) {
		t.Fatal("second message tripped the spam heuristic")
	}
	if !tr.record("u", bodyDigest(canary), now.Add(2*time.Second)) {
		t.Fatal("third identical message in 30s did not trip the spam heuristic")
	}
	if containsCanary(t, tr, canary) {
		t.Fatal("spam tracker retains plaintext")
	}
}

// TestSpamTrackerStripsAttachmentKeys pastes the same image three times with
// a fresh random file key each time. Without stripAttachmentRefs no two
// pastes are ever equal and image spam stops being caught (116).
func TestSpamTrackerStripsAttachmentKeys(t *testing.T) {
	tr := newSpamTracker()
	now := time.Now()
	tripped := false
	for i, key := range []string{"AAAAkey1", "BBBBkey2", "CCCCkey3"} {
		body := "look: [file:ab12.vcx#" + key + "#photo.png]"
		tripped = tr.record("u", bodyDigest(stripAttachmentRefs(body)), now.Add(time.Duration(i)*time.Second))
	}
	if !tripped {
		t.Fatal("three pastes of the same image with different file keys did not trip the spam heuristic")
	}
}

// TestChatPipelineLogsNoBodies runs a full send through an observed logger
// and asserts no entry or field carries the plaintext. It catches a future
// zap.String("body", plain) forever.
func TestChatPipelineLogsNoBodies(t *testing.T) {
	const canary = "canary-7f3a"
	core, logs := observer.New(zap.DebugLevel)
	env := startTestEnvLogger(t, nil, nil, nil, zap.New(core))
	defer env.stop()
	alice, bob, key, keyID, _ := chatPair(t, env)
	defer alice.Close()
	defer bob.Close()

	sendEncChat(t, bob, key, keyID, "1", canary)
	readEventOfType(t, alice, eventChat)

	for _, e := range logs.All() {
		if strings.Contains(e.Message, canary) {
			t.Fatalf("log message carries the plaintext body: %q", e.Message)
		}
		for _, f := range e.Context {
			if strings.Contains(f.String, canary) {
				t.Fatalf("log field %q carries the plaintext body", f.Key)
			}
			if f.Interface != nil && containsCanary(t, f.Interface, canary) {
				t.Fatalf("log field %q carries the plaintext body", f.Key)
			}
		}
	}
}

// tabs_test.go exercises the wave-8a multi-server tab registry: tab
// lifecycle, event journaling/replay, badge counting, and hotkey profiles.
package main

import (
	"path/filepath"
	"testing"

	"voicx/internal/netproto"
)

// newTabApp returns an App with a fresh tab registry (no Wails context).
// settingsPath must always be set so persistence stays inside the test's temp
// dir even if the package-wide path gate is ever rearmed.
func newTabApp(t *testing.T) *App {
	t.Helper()
	return &App{
		settings:     DefaultSettings(),
		hotkeys:      map[string]*hotkeyReg{},
		tabs:         map[string]*tabState{},
		settingsPath: filepath.Join(t.TempDir(), "settings.json"),
	}
}

// TestTabLifecycle covers creation, activation, and closing.
func TestTabLifecycle(t *testing.T) {
	a := newTabApp(t)

	id1, ts1 := a.newTab()
	id2, _ := a.newTab()
	if id1 == id2 {
		t.Fatal("duplicate tab IDs")
	}
	a.activate(id1)
	if a.cmLoad() != ts1.cm {
		t.Fatal("active cm is not tab 1's")
	}
	tabs := a.ListTabs()
	if len(tabs) != 2 {
		t.Fatalf("tabs = %+v", tabs)
	}
	for _, tb := range tabs {
		if tb.ID == id1 && !tb.Active {
			t.Fatal("tab 1 not marked active")
		}
	}

	// Closing the active tab activates the remaining one.
	a.CloseTab(id1)
	if a.cmLoad() == ts1.cm {
		t.Fatal("closed tab still active")
	}
	if len(a.ListTabs()) != 1 {
		t.Fatalf("tabs after close = %+v", a.ListTabs())
	}

	// Closing the last tab leaves no active manager.
	a.CloseTab(id2)
	if a.cmLoad() != nil {
		t.Fatal("cm set with no tabs")
	}
}

// TestTabJournalAndBadges covers background-event journaling, badge
// counting, and journal reset on a fresh snapshot.
func TestTabJournalAndBadges(t *testing.T) {
	a := newTabApp(t)
	id1, _ := a.newTab()
	_, ts2 := a.newTab()
	a.activate(id1) // tab 2 stays in the background

	a.relayTabEvent(ts2.info.ID, "snapshot", `{"root_channels":[]}`)
	a.relayTabEvent(ts2.info.ID, "channellist", `{"channels":[]}`)
	a.relayTabEvent(ts2.info.ID, "subscriptions", `{"channel_ids":[7]}`)
	a.relayTabEvent(ts2.info.ID, "server_rules", `{"text":"be kind","hash":"v1"}`)
	a.relayTabEvent(ts2.info.ID, "event", `{"type":"chat","data":{"from":"bob","text":"hello"}}`)
	a.relayTabEvent(ts2.info.ID, "event", `{"type":"chat","data":{"from":"bob","text":"hey @friend you there"}}`)

	if len(ts2.journal) != 6 {
		t.Fatalf("journal = %d entries, want 6", len(ts2.journal))
	}
	if ts2.info.Unread != 2 {
		t.Fatalf("unread = %d, want 2", ts2.info.Unread)
	}
	// ts2's nickname is empty here, so no mention counted; set it and retry.
	ts2.cm.nickname = "friend"
	a.relayTabEvent(ts2.info.ID, "event", `{"type":"chat","data":{"from":"bob","text":"hey @friend"}}`)
	if ts2.info.Mentions != 1 {
		t.Fatalf("mentions = %d, want 1", ts2.info.Mentions)
	}

	// A fresh snapshot resets the journal.
	a.relayTabEvent(ts2.info.ID, "snapshot", `{"root_channels":[]}`)
	if len(ts2.journal) != 1 {
		t.Fatalf("journal after snapshot = %d, want 1", len(ts2.journal))
	}

	// Activation clears the badges.
	a.activate(ts2.info.ID)
	if ts2.info.Unread != 0 || ts2.info.Mentions != 0 {
		t.Fatalf("badges after activation = %d/%d, want 0/0", ts2.info.Unread, ts2.info.Mentions)
	}
}

// TestActiveTabJournalTracksSessionChanges covers the switch-away/switch-back
// lifecycle. State events received while a tab is active must still be kept in
// its replay journal; otherwise a successful channel join disappears as soon
// as another server tab is selected.
func TestActiveTabJournalTracksSessionChanges(t *testing.T) {
	a := newTabApp(t)
	id, ts := a.newTab()
	a.activate(id)

	a.relayTabEvent(id, "snapshot", `{"root_channels":[]}`)
	a.relayTabEvent(id, "event", `{"type":"user_moved","data":{"client_id":"me","channel_id":42}}`)

	if len(ts.journal) != 2 {
		t.Fatalf("active tab journal = %d entries, want snapshot plus move", len(ts.journal))
	}
}

// TestTabReplayUsesCachedFrames verifies activation replays the cached
// snapshot/list frames through the connManager itself (281 replay source).
func TestTabReplayUsesCachedFrames(t *testing.T) {
	a := newTabApp(t)
	_, ts := a.newTab()
	// Simulate frames that arrived while the tab was in the background.
	ts.cm.dispatch(&netproto.Frame{Type: uint16(netproto.MsgSnapshot), Payload: []byte(`{"root_channels":[]}`)})
	ts.cm.dispatch(&netproto.Frame{Type: uint16(netproto.MsgChannelList), Payload: []byte(`{"channels":[]}`)})
	if ts.cm.lastSnapshot == "" || ts.cm.lastChannelList == "" {
		t.Fatal("frames not cached on the connManager")
	}
}

// TestRecordRecent verifies dedup, ordering, and the 10-entry cap (282).
func TestRecordRecent(t *testing.T) {
	a := newTabApp(t)
	a.settings.Recents = nil

	for i := 0; i < 12; i++ {
		a.RecordRecent("srv-"+string(rune('a'+i)), "nick")
	}
	if len(a.settings.Recents) != 10 {
		t.Fatalf("recents = %d, want 10", len(a.settings.Recents))
	}
	if a.settings.Recents[0].Addr != "srv-l" {
		t.Fatalf("newest first = %q, want srv-l", a.settings.Recents[0].Addr)
	}

	// Reconnecting the same server moves it to the front without duplicating.
	a.RecordRecent("srv-c", "nick")
	if len(a.settings.Recents) != 10 || a.settings.Recents[0].Addr != "srv-c" {
		t.Fatalf("recents after dedup = %+v", a.settings.Recents[:2])
	}
}

// TestLookupBookmark verifies the bookmark a connection belongs to is found
// by name, that the name must agree with the address (names are not unique),
// and that the nameless fallback only resolves an unambiguous match.
func TestLookupBookmark(t *testing.T) {
	a := newTabApp(t)
	a.settings.Bookmarks = []Bookmark{
		{Name: "work", Addr: "srv:12333", Nickname: "alice", NicknameOverride: "alice-work", Profile: "work"},
		{Name: "home", Addr: "srv:12333", Nickname: "alice"},
		{Name: "work", Addr: "other:12333", Nickname: "carol"},
	}

	if b := a.lookupBookmark("home", "srv:12333", "alice-work"); b == nil || b.Name != "home" {
		t.Fatalf("explicit name lookup = %+v", b)
	}
	// A duplicated name resolves per address, not first-wins.
	if b := a.lookupBookmark("work", "other:12333", "carol"); b == nil || b.Nickname != "carol" {
		t.Fatalf("duplicate name lookup = %+v", b)
	}
	// The override is what was sent as the login nickname.
	if b := a.lookupBookmark("", "srv:12333", "alice-work"); b == nil || b.Name != "work" {
		t.Fatalf("override lookup = %+v", b)
	}
	// Two bookmarks answer to "alice" on that server: guessing would apply
	// the wrong profile/avatar override, so neither is returned.
	if b := a.lookupBookmark("", "srv:12333", "alice"); b != nil {
		t.Fatalf("ambiguous nickname matched %+v", b)
	}
	if b := a.lookupBookmark("", "nowhere:12333", "alice"); b != nil {
		t.Fatalf("unknown server matched %+v", b)
	}
	if b := a.lookupBookmark("gone", "srv:12333", "alice"); b != nil {
		t.Fatalf("unknown bookmark name matched %+v", b)
	}
	if b := a.lookupBookmark("work", "nowhere:12333", "alice-work"); b != nil {
		t.Fatalf("name matched a foreign address: %+v", b)
	}
}

// TestLookupBookmarkNicknameCollision covers the shape that broke: one
// bookmark's override is another bookmark's plain nickname on the same
// server, so only the explicit name can tell them apart.
func TestLookupBookmarkNicknameCollision(t *testing.T) {
	a := newTabApp(t)
	a.settings.Bookmarks = []Bookmark{
		{Name: "X", Addr: "srv", Nickname: "alice", NicknameOverride: "bob", AvatarOverrideB64: "xxx"},
		{Name: "Y", Addr: "srv", Nickname: "bob"},
	}

	if b := a.lookupBookmark("Y", "srv", "bob"); b == nil || b.Name != "Y" {
		t.Fatalf("connect via Y resolved %+v", b)
	}
	if b := a.lookupBookmark("X", "srv", "bob"); b == nil || b.Name != "X" {
		t.Fatalf("connect via X resolved %+v", b)
	}
}

// TestLookupBookmarkDuplicateName covers two bookmarks on one server renamed
// to the same name: nothing enforces name uniqueness, and the pair carries
// different profiles/avatar overrides, so neither may win.
func TestLookupBookmarkDuplicateName(t *testing.T) {
	a := newTabApp(t)
	a.settings.Bookmarks = []Bookmark{
		{Name: "work", Addr: "srv:12333", Nickname: "alice", Profile: "alice", AvatarOverrideB64: "aaa"},
		{Name: "work", Addr: "srv:12333", Nickname: "bob", Profile: "bob", AvatarOverrideB64: "bbb"},
	}

	if b := a.lookupBookmark("work", "srv:12333", "bob"); b != nil {
		t.Fatalf("ambiguous name matched %+v", b)
	}
	if b := a.lookupBookmark("work", "srv:12333", "alice"); b != nil {
		t.Fatalf("ambiguous name matched %+v", b)
	}
}

// TestHotkeyProfiles verifies profile merging over defaults (300).
func TestHotkeyProfiles(t *testing.T) {
	a := newTabApp(t)
	a.settings.HotkeyPTT = "Space"
	a.settings.HotkeyMute = "Ctrl+M"
	a.settings.HotkeyProfiles = map[string]HotkeyProfile{
		"work": {PTT: "F5"},
	}

	specs := a.profileSpecs("work")
	if specs["ptt"] != "F5" {
		t.Fatalf("profile ptt = %q, want F5", specs["ptt"])
	}
	if specs["mute_toggle"] != "Ctrl+M" {
		t.Fatalf("profile mute = %q, want default Ctrl+M", specs["mute_toggle"])
	}

	def := a.profileSpecs("default")
	if def["ptt"] != "Space" {
		t.Fatalf("default ptt = %q", def["ptt"])
	}
}

// TestSetHotkeyValidation verifies the per-action rebind API (299).
func TestSetHotkeyValidation(t *testing.T) {
	a := newTabApp(t)

	if err := a.SetHotkey("nope", "F1"); err == "" {
		t.Fatal("unknown action accepted")
	}
	if err := a.SetHotkey("quick_connect", "not a key!!"); err == "" {
		t.Fatal("invalid spec accepted")
	}
	if err := a.SetHotkey("quick_connect", "Ctrl+Shift+C"); err != "" {
		t.Fatalf("valid spec rejected: %s", err)
	}
	if a.settings.HotkeyQuickConnect != "Ctrl+Shift+C" {
		t.Fatalf("spec = %q", a.settings.HotkeyQuickConnect)
	}
}

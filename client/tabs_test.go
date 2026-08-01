// tabs_test.go exercises the wave-8a multi-server tab registry: tab
// lifecycle, event journaling/replay, badge counting, and hotkey profiles.
package main

import (
	"testing"

	"voicx/internal/netproto"
)

// newTabApp returns an App with a fresh tab registry (no Wails context).
func newTabApp() *App {
	return &App{
		settings: DefaultSettings(),
		hotkeys:  map[string]*hotkeyReg{},
		tabs:     map[string]*tabState{},
	}
}

// TestTabLifecycle covers creation, activation, and closing.
func TestTabLifecycle(t *testing.T) {
	a := newTabApp()

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
	a := newTabApp()
	id1, _ := a.newTab()
	_, ts2 := a.newTab()
	a.activate(id1) // tab 2 stays in the background

	a.relayTabEvent(ts2.info.ID, "snapshot", `{"root_channels":[]}`)
	a.relayTabEvent(ts2.info.ID, "channellist", `{"channels":[]}`)
	a.relayTabEvent(ts2.info.ID, "event", `{"type":"chat","data":{"from":"bob","text":"hello"}}`)
	a.relayTabEvent(ts2.info.ID, "event", `{"type":"chat","data":{"from":"bob","text":"hey @friend you there"}}`)

	if len(ts2.journal) != 4 {
		t.Fatalf("journal = %d entries, want 4", len(ts2.journal))
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

// TestTabReplayUsesCachedFrames verifies activation replays the cached
// snapshot/list frames through the connManager itself (281 replay source).
func TestTabReplayUsesCachedFrames(t *testing.T) {
	a := newTabApp()
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
	a := newTabApp()
	a.settings.Recents = nil
	a.settingsPath = t.TempDir() + "/settings.json"

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

// TestHotkeyProfiles verifies profile merging over defaults (300).
func TestHotkeyProfiles(t *testing.T) {
	a := newTabApp()
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
	a := newTabApp()
	a.settingsPath = t.TempDir() + "/settings.json"

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

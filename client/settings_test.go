package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestSettingsRoundTrip verifies defaults, save, and reload.
func TestSettingsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voicx", "settings.json")

	// Missing file: defaults.
	s := loadSettingsAt(path)
	if s.HotkeyPTT != "Space" || s.HotkeyMute != "Ctrl+M" || s.Volume != 100 ||
		s.ActivationMode != "ptt" || s.ChatMaxLines != 200 {
		t.Fatalf("defaults = %+v", s)
	}

	s.ChatMaxLines = 42
	s.Volume = 150
	s.HotkeyPTT = "F5"
	s.Bookmarks = []Bookmark{{Name: "local", Addr: "127.0.0.1:10011", Nickname: "alice"}}
	s.WhisperClients = []string{"uid-1"}

	if err := saveSettingsAt(path, s); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("settings file is empty")
	}

	loaded := loadSettingsAt(path)
	if loaded.ChatMaxLines != 42 || loaded.Volume != 150 || loaded.HotkeyPTT != "F5" {
		t.Fatalf("reloaded = %+v", loaded)
	}
	if len(loaded.Bookmarks) != 1 || loaded.Bookmarks[0].Addr != "127.0.0.1:10011" {
		t.Fatalf("bookmarks = %+v", loaded.Bookmarks)
	}
	if len(loaded.WhisperClients) != 1 || loaded.WhisperClients[0] != "uid-1" {
		t.Fatalf("whisper clients = %+v", loaded.WhisperClients)
	}
}

// TestSaveSettingsKeepsGoOwnedFields verifies a frontend save carrying a
// stale cached blob cannot wipe fields the Go side maintains on its own
// (282 recents, 330 what's-new marker).
func TestSaveSettingsKeepsGoOwnedFields(t *testing.T) {
	a := &App{
		settings:     DefaultSettings(),
		hotkeys:      map[string]*hotkeyReg{},
		settingsPath: filepath.Join(t.TempDir(), "settings.json"),
	}
	// What the frontend cached before the connect happened.
	stale := a.GetSettings()

	a.RecordRecent("127.0.0.1:10011", "alice")
	a.settings.LastSeenVersion = "9.9"

	stale.Volume = 120
	if err := a.SaveSettings(stale); err != "" {
		t.Fatalf("save: %s", err)
	}
	if len(a.settings.Recents) != 1 || a.settings.Recents[0].Addr != "127.0.0.1:10011" {
		t.Fatalf("recents clobbered by frontend save: %+v", a.settings.Recents)
	}
	if a.settings.LastSeenVersion != "9.9" {
		t.Fatalf("last_seen_version = %q, want 9.9", a.settings.LastSeenVersion)
	}
	if a.settings.Volume != 120 {
		t.Fatalf("frontend-owned volume = %d, want 120", a.settings.Volume)
	}
	loaded := loadSettingsAt(a.settingsPath)
	if len(loaded.Recents) != 1 || loaded.LastSeenVersion != "9.9" {
		t.Fatalf("persisted settings = %+v / %q", loaded.Recents, loaded.LastSeenVersion)
	}
}

// TestSettingsCorruptFile verifies a corrupt file falls back to defaults.
func TestSettingsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	s := loadSettingsAt(path)
	if s.HotkeyPTT != "Space" {
		t.Fatalf("corrupt file did not fall back to defaults: %+v", s)
	}
}

// TestSettingsMergeMissingFields verifies old files (missing new fields)
// merge onto defaults.
func TestSettingsMergeMissingFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"volume": 175}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	s := loadSettingsAt(path)
	if s.Volume != 175 {
		t.Fatalf("volume = %d, want 175", s.Volume)
	}
	if s.HotkeyPTT != "Space" || s.ChatMaxLines != 200 {
		t.Fatalf("missing fields not defaulted: %+v", s)
	}
}

// TestSettingsWave1Fields verifies the wave-1 fields round-trip and merge
// with defaults.
func TestSettingsWave1Fields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")

	s := DefaultSettings()
	if !s.WarnMutedTalking || !s.WarnEmptyChannel || !s.VoiceLimiter {
		t.Fatal("wave-1 defaults wrong (toggles should be on)")
	}
	if s.SoundPack != "soft" || s.SoundVolume != 100 || s.WhisperReplyHotkey != "Ctrl+R" {
		t.Fatalf("wave-1 defaults: %+v", s)
	}
	if !s.EventSounds["join"] || !s.EventSounds["whisper"] {
		t.Fatal("event_sounds defaults incomplete")
	}

	s.UserVolumes = map[string]int{"uid-1": 150}
	s.MutedUsers = []string{"uid-2"}
	s.PTTReleaseDelayMs = 300
	s.WarnMutedTalking = false
	s.SoundPack = "retro"
	s.EventSounds["join"] = false
	s.VoiceLimiter = false

	if err := saveSettingsAt(path, s); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded := loadSettingsAt(path)
	if loaded.UserVolumes["uid-1"] != 150 {
		t.Fatalf("user_volumes = %+v", loaded.UserVolumes)
	}
	if len(loaded.MutedUsers) != 1 || loaded.MutedUsers[0] != "uid-2" {
		t.Fatalf("muted_users = %+v", loaded.MutedUsers)
	}
	if loaded.PTTReleaseDelayMs != 300 || loaded.WarnMutedTalking {
		t.Fatalf("ptt delay / warn: %+v", loaded)
	}
	if loaded.SoundPack != "retro" || loaded.EventSounds["join"] {
		t.Fatalf("sound fields: %+v", loaded)
	}
	if loaded.VoiceLimiter {
		t.Fatal("voice_limiter should be false after round-trip")
	}

	// Older file without wave-1 fields merges onto defaults.
	old := filepath.Join(t.TempDir(), "old.json")
	if err := os.WriteFile(old, []byte(`{"volume": 120}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	merged := loadSettingsAt(old)
	if merged.Volume != 120 || !merged.WarnMutedTalking || merged.SoundPack != "soft" {
		t.Fatalf("merge with defaults broken: %+v", merged)
	}
}

// TestSettingsWave8c verifies the wave-8c settings fields persist through a
// JSON round trip and get sane defaults.
func TestSettingsWave8c(t *testing.T) {
	s := DefaultSettings()
	if s.Language != "" && s.Language != "system" {
		t.Fatalf("default language = %q", s.Language)
	}
	if !s.IdleVideoPause {
		t.Fatal("idle video pause should default on")
	}
	if s.HotkeyZen != "Ctrl+Shift+Z" {
		t.Fatalf("zen hotkey default = %q", s.HotkeyZen)
	}

	s.Language = "de"
	s.ReduceMotion = true
	s.SidebarWidth = 280
	s.DetailsWidth = 320
	s.DNDEnabled = true
	s.DNDFrom = "22:00"
	s.DNDTo = "07:00"

	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Settings
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Language != "de" || !back.ReduceMotion || back.SidebarWidth != 280 ||
		back.DetailsWidth != 320 || !back.DNDEnabled || back.DNDFrom != "22:00" || back.DNDTo != "07:00" {
		t.Fatalf("round trip lost fields: %+v", back)
	}

	// Old settings files without the new fields still load with defaults.
	var legacy Settings
	if err := json.Unmarshal([]byte(`{"theme":"light"}`), &legacy); err != nil {
		t.Fatalf("legacy unmarshal: %v", err)
	}
	if legacy.Theme != "light" || legacy.Language != "" {
		t.Fatalf("legacy load = %+v", legacy)
	}
}

// TestSettingsWave9 verifies the wave-9 notification settings shapes persist
// through a JSON round trip.
func TestSettingsWave9(t *testing.T) {
	s := DefaultSettings()
	s.Contacts = []Contact{{UniqueID: "uid-1", Label: "alice", NotifyOnline: true}}
	s.NotifyMatrix = map[string]NotifyChannels{
		"mention": {Toast: true, Sound: false, Flash: true, Native: true},
	}
	s.CustomSounds = map[string]BeepSpec{"mention": {Freq: 880, DurationMs: 150}}
	s.ChannelNotify = map[string]ChannelOverride{
		"srv#42": {Messages: "off", Muted: true, WatchThreshold: 3},
	}
	s.Keywords = map[string][]string{"srv": {"deploy", "incident"}}
	s.AlphaDismissed = "0.4.0"

	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Settings
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !back.Contacts[0].NotifyOnline {
		t.Fatal("contact notify_online lost")
	}
	m := back.NotifyMatrix["mention"]
	if !m.Toast || m.Sound || !m.Flash || !m.Native {
		t.Fatalf("matrix row = %+v", m)
	}
	if back.CustomSounds["mention"].Freq != 880 {
		t.Fatalf("beep = %+v", back.CustomSounds["mention"])
	}
	ov := back.ChannelNotify["srv#42"]
	if ov.Messages != "off" || !ov.Muted || ov.WatchThreshold != 3 {
		t.Fatalf("channel override = %+v", ov)
	}
	if len(back.Keywords["srv"]) != 2 || back.AlphaDismissed != "0.4.0" {
		t.Fatalf("keywords/alpha = %+v %q", back.Keywords, back.AlphaDismissed)
	}
}

// TestDefaultsThatMustNotBeZero pins the settings whose zero value disables a
// feature. A reader added without its default is how the master sound gate
// silenced every notification (28); merging a file onto the defaults means an
// older file's false wins, which is what migrateSettings repairs.
func TestDefaultsThatMustNotBeZero(t *testing.T) {
	raw, err := json.Marshal(DefaultSettings())
	if err != nil {
		t.Fatalf("marshal defaults: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal defaults: %v", err)
	}
	for _, key := range []string{"play_sounds", "warn_muted_talking", "warn_empty_channel", "voice_limiter"} {
		if m[key] != true {
			t.Errorf("%s default = %v, want true", key, m[key])
		}
	}
}

// TestMigrateSettingsRestoresPlaySounds covers both directions: a pre-version
// file gets sound back, a current file keeps a deliberate opt-out.
func TestMigrateSettingsRestoresPlaySounds(t *testing.T) {
	old := DefaultSettings()
	old.SettingsVersion = 0
	old.PlaySounds = false
	if got := migrateSettings(old); !got.PlaySounds {
		t.Error("pre-version file did not get play_sounds restored")
	} else if got.SettingsVersion != settingsVersion {
		t.Errorf("version = %d, want %d", got.SettingsVersion, settingsVersion)
	}

	cur := DefaultSettings()
	cur.PlaySounds = false
	if migrateSettings(cur).PlaySounds {
		t.Error("migration overrode a deliberate opt-out")
	}
}

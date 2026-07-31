package main

import (
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

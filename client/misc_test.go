package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConfigDirCreatesAppDirectory(t *testing.T) {
	original := miscUserConfigDir
	miscUserConfigDir = func() (string, error) { return t.TempDir(), nil }
	t.Cleanup(func() { miscUserConfigDir = original })

	dir, err := configDir()
	if err != nil {
		t.Fatalf("configDir: %v", err)
	}
	if want := filepath.Join(filepath.Dir(dir), "voicx"); dir != want {
		t.Fatalf("configDir = %q, want %q", dir, want)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat configDir: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("configDir created %q, want directory", dir)
	}
}

func TestConfigDirReturnsUserConfigFailure(t *testing.T) {
	original := miscUserConfigDir
	miscUserConfigDir = func() (string, error) { return "", errors.New("no user config directory") }
	t.Cleanup(func() { miscUserConfigDir = original })

	if _, err := configDir(); err == nil {
		t.Fatal("configDir succeeded despite UserConfigDir failure")
	}
}

func TestLogChatWritesTimestampedDailyLog(t *testing.T) {
	closeDailyLogs()
	t.Cleanup(closeDailyLogs)
	originalDir, originalNow := miscUserConfigDir, chatLogNow
	base := t.TempDir()
	miscUserConfigDir = func() (string, error) { return base, nil }
	chatLogNow = func() time.Time { return time.Date(2026, time.August, 22, 12, 34, 56, 0, time.UTC) }
	t.Cleanup(func() {
		miscUserConfigDir = originalDir
		chatLogNow = originalNow
	})

	(&App{}).LogChat("hello from test")
	closeDailyLogs()
	raw, err := os.ReadFile(filepath.Join(base, "voicx", "chat.log"))
	if err != nil {
		t.Fatalf("read chat log: %v", err)
	}
	if got, want := string(raw), "[2026-08-22 12:34:56] hello from test\n"; got != want {
		t.Fatalf("chat log = %q, want %q", got, want)
	}
}

func TestLogChatIgnoresConfigDirectoryFailure(t *testing.T) {
	original := miscUserConfigDir
	miscUserConfigDir = func() (string, error) { return "", errors.New("no user config directory") }
	t.Cleanup(func() { miscUserConfigDir = original })

	(&App{}).LogChat("must not reach the filesystem")
}

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardCrashWritesLogAndRepans(t *testing.T) {
	original := metaUserConfigDir
	base := t.TempDir()
	metaUserConfigDir = func() (string, error) { return base, nil }
	t.Cleanup(func() { metaUserConfigDir = original })

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		guardCrash("test handler", func() { panic("crash canary") })
	}()
	if recovered != "crash canary" {
		t.Fatalf("recovered panic = %#v, want crash canary", recovered)
	}
	raw, err := os.ReadFile(filepath.Join(base, "voicx", crashLogName))
	if err != nil {
		t.Fatalf("read crash log: %v", err)
	}
	if text := string(raw); !strings.Contains(text, "panic in test handler: crash canary") || !strings.Contains(text, "goroutine") {
		t.Fatalf("crash log = %q, want context, panic, and stack", text)
	}
}

func TestLastCrashReturnsTailAndConsumesIt(t *testing.T) {
	original := metaUserConfigDir
	base := t.TempDir()
	metaUserConfigDir = func() (string, error) { return base, nil }
	t.Cleanup(func() { metaUserConfigDir = original })
	dir := filepath.Join(base, "voicx")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("make log directory: %v", err)
	}
	raw := strings.Repeat("x", 4_100) + "tail marker"
	if err := os.WriteFile(filepath.Join(dir, crashLogName), []byte(raw), 0o600); err != nil {
		t.Fatalf("write crash log: %v", err)
	}

	got := (&App{}).LastCrash()
	if len(got) != 4_000 || !strings.HasSuffix(got, "tail marker") {
		t.Fatalf("LastCrash tail = %q (len %d), want 4000-byte tail with marker", got, len(got))
	}
	if again := (&App{}).LastCrash(); again != "" {
		t.Fatalf("LastCrash after consume = %q, want empty", again)
	}
}

func TestWhatsNewMarksVersionOnlyAfterDurableSave(t *testing.T) {
	originalVersion := shortVersion
	shortVersion = func() string { return "0.4" }
	t.Cleanup(func() { shortVersion = originalVersion })

	t.Run("save failure leaves marker uncommitted", func(t *testing.T) {
		originalWriter := settingsSnapshotWriter
		settingsSnapshotWriter = func(string, Settings) error { return errors.New("injected settings write failure") }
		t.Cleanup(func() { settingsSnapshotWriter = originalWriter })
		a := &App{settings: DefaultSettings(), settingsPath: filepath.Join(t.TempDir(), "settings.json")}
		if got := a.WhatsNew(); got == "" {
			t.Fatal("WhatsNew returned no notes after a failed first save")
		}
		if got := a.GetSettings().LastSeenVersion; got != "" {
			t.Fatalf("last seen version advanced after failed save: %q", got)
		}
	})

	t.Run("durable save suppresses a second display", func(t *testing.T) {
		a := &App{settings: DefaultSettings(), settingsPath: filepath.Join(t.TempDir(), "settings.json")}
		if got := a.WhatsNew(); got == "" {
			t.Fatal("first WhatsNew returned no notes")
		}
		if got := a.GetSettings().LastSeenVersion; got != "0.4" {
			t.Fatalf("last seen version = %q, want 0.4", got)
		}
		if got := a.WhatsNew(); got != "" {
			t.Fatalf("second WhatsNew = %q, want empty", got)
		}
	})
}

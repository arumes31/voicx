// main_test.go holds package-wide test setup.
package main

import (
	"io"
	"log"
	"os"
	"testing"
)

// TestMain disarms the settings-path fallback for the whole package. An App
// built without settingsPath (a plain &App{}) otherwise writes through to the
// developer's own <UserConfigDir>/voicx/settings.json. Hotkey registration is
// disarmed with it: SaveSettings/SetHotkey otherwise grab configured shortcuts
// system-wide for the whole test run.
func TestMain(m *testing.M) {
	allowDefaultSettingsPath = false
	allowEmptySettingsSnapshot = true
	allowHotkeyRegistration = false
	os.Exit(m.Run())
}

func TestConfigureStandardLogger(t *testing.T) {
	flags := log.Flags()
	prefix := log.Prefix()
	writer := log.Writer()
	t.Cleanup(func() {
		log.SetFlags(flags)
		log.SetPrefix(prefix)
		log.SetOutput(writer)
	})
	log.SetOutput(io.Discard)
	log.SetFlags(0)
	log.SetPrefix("old-prefix")
	configureStandardLogger()
	want := log.Ldate | log.Ltime | log.Lmicroseconds | log.LUTC
	if got := log.Flags(); got != want {
		t.Fatalf("log flags = %d, want %d", got, want)
	}
	if got := log.Prefix(); got != "" {
		t.Fatalf("log prefix = %q, want empty", got)
	}
}

// TestSideEffectsDisarmed guards the two package-wide test gates: removing
// either silently gives the suite back its real-world side effects.
func TestSideEffectsDisarmed(t *testing.T) {
	if allowDefaultSettingsPath {
		t.Error("settings-path fallback armed: tests can overwrite the real settings.json")
	}
	if !allowEmptySettingsSnapshot {
		t.Error("empty settings snapshot test gate is not armed")
	}
	if allowHotkeyRegistration {
		t.Error("hotkey registration armed: tests grab global hotkeys")
	}
}

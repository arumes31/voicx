// main_test.go holds package-wide test setup.
package main

import (
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
	allowHotkeyRegistration = false
	os.Exit(m.Run())
}

// TestSideEffectsDisarmed guards the two package-wide test gates: removing
// either silently gives the suite back its real-world side effects.
func TestSideEffectsDisarmed(t *testing.T) {
	if allowDefaultSettingsPath {
		t.Error("settings-path fallback armed: tests can overwrite the real settings.json")
	}
	if allowHotkeyRegistration {
		t.Error("hotkey registration armed: tests grab global hotkeys")
	}
}

package main

import "testing"

import "golang.design/x/hotkey"

func TestParseHotkeySpec(t *testing.T) {
	cases := []struct {
		spec    string
		wantErr bool
	}{
		{"Space", false},
		{"Ctrl+M", false},
		{"Ctrl+Shift+F5", false},
		{"Alt+F4", false},
		{"F1", false},
		{"a", false},
		{"Shift+A", false},
		{"Ctrl+Alt+Shift+Left", false},
		{"Win+Tab", false},
		{"", true},
		{"Ctrl+", true},
		{"Ctrl+Banana", true},
		{"Hyper+M", true},
	}
	for _, tc := range cases {
		mods, key, err := parseHotkeySpec(tc.spec)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseHotkeySpec(%q) = %v, %v, nil; want error", tc.spec, mods, key)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseHotkeySpec(%q): %v", tc.spec, err)
		}
	}

	// Spot-check specifics.
	mods, key, err := parseHotkeySpec("Ctrl+M")
	if err != nil {
		t.Fatalf("Ctrl+M: %v", err)
	}
	if len(mods) != 1 || mods[0] != hotkey.ModCtrl || key != hotkey.KeyM {
		t.Errorf("Ctrl+M parsed badly: mods=%v key=%v", mods, key)
	}
}

func TestValidateHotkeySpecAllowsUnbound(t *testing.T) {
	if err := validateHotkeySpec(""); err != nil {
		t.Fatalf("empty hotkey should unbind: %v", err)
	}
	if err := validateHotkeySpec("not-a-key"); err == nil {
		t.Fatal("invalid non-empty hotkey accepted")
	}
}

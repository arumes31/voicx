// hotkeyspec.go parses canonical hotkey specs ("Space", "Ctrl+M",
// "Ctrl+Shift+F5") into golang.design/x/hotkey modifiers and keys.
package main

import (
	"fmt"
	"strings"

	"golang.design/x/hotkey"
)

// specModifiers maps modifier names to hotkey modifiers.
var specModifiers = map[string]hotkey.Modifier{
	"ctrl":  hotkey.ModCtrl,
	"alt":   hotkey.ModAlt,
	"shift": hotkey.ModShift,
	"win":   hotkey.ModWin,
}

// specKeys maps key names to hotkey keys (only keys the hotkey package
// defines on Windows are listed).
var specKeys = map[string]hotkey.Key{
	"space":  hotkey.KeySpace,
	"tab":    hotkey.KeyTab,
	"enter":  hotkey.KeyReturn,
	"return": hotkey.KeyReturn,
	"escape": hotkey.KeyEscape,
	"esc":    hotkey.KeyEscape,
	"delete": hotkey.KeyDelete,
	"del":    hotkey.KeyDelete,
	"up":     hotkey.KeyUp,
	"down":   hotkey.KeyDown,
	"left":   hotkey.KeyLeft,
	"right":  hotkey.KeyRight,
}

// functionKeys maps F1..F20 names to keys.
var functionKeys = map[string]hotkey.Key{
	"f1": hotkey.KeyF1, "f2": hotkey.KeyF2, "f3": hotkey.KeyF3, "f4": hotkey.KeyF4,
	"f5": hotkey.KeyF5, "f6": hotkey.KeyF6, "f7": hotkey.KeyF7, "f8": hotkey.KeyF8,
	"f9": hotkey.KeyF9, "f10": hotkey.KeyF10, "f11": hotkey.KeyF11, "f12": hotkey.KeyF12,
	"f13": hotkey.KeyF13, "f14": hotkey.KeyF14, "f15": hotkey.KeyF15, "f16": hotkey.KeyF16,
	"f17": hotkey.KeyF17, "f18": hotkey.KeyF18, "f19": hotkey.KeyF19, "f20": hotkey.KeyF20,
}

// letterKey maps a lowercase ASCII letter to its hotkey key.
func letterKey(c byte) (hotkey.Key, bool) {
	if c >= 'a' && c <= 'z' {
		return hotkey.Key(hotkey.KeyA + hotkey.Key(c-'a')), true
	}
	if c >= '0' && c <= '9' {
		return hotkey.Key(hotkey.Key0 + hotkey.Key(c-'0')), true
	}
	return 0, false
}

// parseHotkeySpec parses a spec like "Ctrl+Shift+F5" or "Space" into
// modifiers and a key. It returns an error for unknown or empty specs.
func parseHotkeySpec(spec string) ([]hotkey.Modifier, hotkey.Key, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, 0, fmt.Errorf("empty hotkey spec")
	}
	parts := strings.Split(spec, "+")
	var mods []hotkey.Modifier
	for _, p := range parts[:len(parts)-1] {
		m, ok := specModifiers[strings.ToLower(strings.TrimSpace(p))]
		if !ok {
			return nil, 0, fmt.Errorf("unknown modifier %q", p)
		}
		mods = append(mods, m)
	}

	name := strings.ToLower(strings.TrimSpace(parts[len(parts)-1]))
	if name == "" {
		return nil, 0, fmt.Errorf("missing key in spec %q", spec)
	}
	if k, ok := specKeys[name]; ok {
		return mods, k, nil
	}
	if k, ok := functionKeys[name]; ok {
		return mods, k, nil
	}
	if len(name) == 1 {
		if k, ok := letterKey(name[0]); ok {
			return mods, k, nil
		}
	}
	return nil, 0, fmt.Errorf("unsupported key %q", parts[len(parts)-1])
}

// validateHotkeySpec accepts an empty spec as an explicit unbind. Parsing
// itself remains strict so callers that require a binding still get a useful
// error for an empty value.
func validateHotkeySpec(spec string) error {
	if strings.TrimSpace(spec) == "" {
		return nil
	}
	_, _, err := parseHotkeySpec(spec)
	return err
}

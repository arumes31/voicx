//go:build !windows

package main

import "golang.design/x/hotkey"

// specModifiers maps modifier names to hotkey modifiers on non-Windows platforms (Linux/macOS).
var specModifiers = map[string]hotkey.Modifier{
	"ctrl":  hotkey.ModCtrl,
	"alt":   hotkey.Mod1,
	"shift": hotkey.ModShift,
	"win":   hotkey.Mod4,
}

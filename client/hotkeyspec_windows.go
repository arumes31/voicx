//go:build windows

package main

import "golang.design/x/hotkey"

// specModifiers maps modifier names to hotkey modifiers on Windows.
var specModifiers = map[string]hotkey.Modifier{
	"ctrl":  hotkey.ModCtrl,
	"alt":   hotkey.ModAlt,
	"shift": hotkey.ModShift,
	"win":   hotkey.ModWin,
}

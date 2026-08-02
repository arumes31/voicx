//go:build windows

package main

import (
	"fmt"
	"time"

	"golang.design/x/hotkey"
	"golang.org/x/sys/windows"
)

const (
	vkShift   = 0x10
	vkControl = 0x11
	vkMenu    = 0x12
	vkLWin    = 0x5b
	vkRWin    = 0x5c
)

var getAsyncKeyState = windows.NewLazySystemDLL("user32.dll").NewProc("GetAsyncKeyState")
var keyPressed = windowsKeyPressed

func passiveHotkeyAvailable() bool { return true }

// monitorPassiveHotkey polls the configured chord without RegisterHotKey. The
// high bit returned by GetAsyncKeyState is observational: it does not remove
// the key from the foreground application's input queue.
func monitorPassiveHotkey(
	mods []hotkey.Modifier,
	key hotkey.Key,
	cancel <-chan struct{},
	onDown, onUp func(),
) error {
	if err := getAsyncKeyState.Find(); err != nil {
		return fmt.Errorf("GetAsyncKeyState: %w", err)
	}

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	down := false
	for {
		select {
		case <-cancel:
			if down {
				onUp()
			}
			return nil
		case <-ticker.C:
			pressed := windowsChordPressed(mods, key)
			if pressed == down {
				continue
			}
			down = pressed
			if down {
				onDown()
			} else {
				onUp()
			}
		}
	}
}

func windowsChordPressed(mods []hotkey.Modifier, key hotkey.Key) bool {
	vk, ok := windowsVirtualKey(key)
	if !ok || !keyPressed(vk) {
		return false
	}
	for _, mod := range mods {
		switch mod {
		case hotkey.ModShift:
			if !keyPressed(vkShift) {
				return false
			}
		case hotkey.ModCtrl:
			if !keyPressed(vkControl) {
				return false
			}
		case hotkey.ModAlt:
			if !keyPressed(vkMenu) {
				return false
			}
		case hotkey.ModWin:
			if !keyPressed(vkLWin) && !keyPressed(vkRWin) {
				return false
			}
		}
	}
	return true
}

func windowsVirtualKey(key hotkey.Key) (uintptr, bool) {
	if key >= hotkey.KeyA && key <= hotkey.KeyZ {
		return 0x41 + uintptr(key-hotkey.KeyA), true
	}
	if key >= hotkey.Key0 && key <= hotkey.Key9 {
		return 0x30 + uintptr(key-hotkey.Key0), true
	}
	if key >= hotkey.KeyF1 && key <= hotkey.KeyF20 {
		return 0x70 + uintptr(key-hotkey.KeyF1), true
	}
	switch key {
	case hotkey.KeySpace:
		return 0x20, true
	case hotkey.KeyTab:
		return 0x09, true
	case hotkey.KeyReturn:
		return 0x0D, true
	case hotkey.KeyEscape:
		return 0x1B, true
	case hotkey.KeyDelete:
		return 0x2E, true
	case hotkey.KeyUp:
		return 0x26, true
	case hotkey.KeyDown:
		return 0x28, true
	case hotkey.KeyLeft:
		return 0x25, true
	case hotkey.KeyRight:
		return 0x27, true
	default:
		return 0, false
	}
}

func windowsKeyPressed(key uintptr) bool {
	state, _, _ := getAsyncKeyState.Call(key)
	return state&0x8000 != 0
}

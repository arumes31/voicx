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
	if !keyPressed(uintptr(key)) {
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

func windowsKeyPressed(key uintptr) bool {
	state, _, _ := getAsyncKeyState.Call(key)
	return state&0x8000 != 0
}

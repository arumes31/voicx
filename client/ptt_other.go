//go:build !windows

package main

import (
	"errors"

	"golang.design/x/hotkey"
)

func passiveHotkeyAvailable() bool { return false }

func monitorPassiveHotkey(
	_ []hotkey.Modifier,
	_ hotkey.Key,
	_ <-chan struct{},
	_, _ func(),
) error {
	return errors.New("passive hotkeys are unavailable on this platform")
}

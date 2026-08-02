//go:build windows

package main

import (
	"testing"
	"time"

	"golang.design/x/hotkey"
)

func TestWindowsChordPressedRequiresKeyAndModifiers(t *testing.T) {
	original := keyPressed
	t.Cleanup(func() { keyPressed = original })
	pressed := map[uintptr]bool{}
	keyPressed = func(key uintptr) bool { return pressed[key] }

	mods := []hotkey.Modifier{hotkey.ModCtrl, hotkey.ModShift}
	key := hotkey.KeyM
	if windowsChordPressed(mods, key) {
		t.Fatal("empty keyboard state matched the chord")
	}
	pressed[uintptr(key)] = true
	if windowsChordPressed(mods, key) {
		t.Fatal("main key without modifiers matched the chord")
	}
	pressed[vkControl] = true
	if windowsChordPressed(mods, key) {
		t.Fatal("partially held modifiers matched the chord")
	}
	pressed[vkShift] = true
	if !windowsChordPressed(mods, key) {
		t.Fatal("fully held chord did not match")
	}
	delete(pressed, uintptr(key))
	if windowsChordPressed(mods, key) {
		t.Fatal("released main key still matched the chord")
	}
}

func TestWindowsChordPressedAcceptsEitherWinKey(t *testing.T) {
	original := keyPressed
	t.Cleanup(func() { keyPressed = original })
	pressed := map[uintptr]bool{uintptr(hotkey.KeyTab): true, vkRWin: true}
	keyPressed = func(key uintptr) bool { return pressed[key] }

	if !windowsChordPressed([]hotkey.Modifier{hotkey.ModWin}, hotkey.KeyTab) {
		t.Fatal("right Windows key did not satisfy Win modifier")
	}
}

func TestPassiveHotkeyLoopStopsWhenUnbound(t *testing.T) {
	a := &App{hotkeys: map[string]*hotkeyReg{}}
	done := make(chan struct{})
	go func() {
		a.passiveHotkeyLoop("ptt", nil, hotkey.KeyF20)
		close(done)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		a.hkMu.Lock()
		_, registered := a.hotkeys["ptt"]
		a.hkMu.Unlock()
		if registered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("passive hotkey did not start")
		}
		time.Sleep(time.Millisecond)
	}

	a.applyHotkey("ptt", "")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("passive hotkey did not stop after unbind")
	}
}

// hotkeys.go registers global hotkeys for push-to-talk and mute toggle.
// Bindings come from settings (defaults: Space, Ctrl+M) and can be changed
// at runtime via SetHotkeys. Registration failures are retried once, then
// surfaced to the frontend as hotkey_status events (and to the client log
// file).
package main

import (
	"log"
	"time"

	"golang.design/x/hotkey"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// hotkeyStatus is emitted as a hotkey_status event so the UI can show
// whether global capture is live.
type hotkeyStatus struct {
	Action     string `json:"action"`
	Registered bool   `json:"registered"`
	Error      string `json:"error,omitempty"`
}

// hotkeyReg tracks a live registration so it can be torn down on rebinding.
type hotkeyReg struct {
	hk     *hotkey.Hotkey
	cancel chan struct{}
}

// registerHotkeys installs the configured global hotkeys (called at startup).
func (a *App) registerHotkeys() {
	a.applyHotkey("ptt", a.settings.HotkeyPTT)
	a.applyHotkey("mute_toggle", a.settings.HotkeyMute)
}

// applyHotkey tears down any existing registration for action and registers
// the new spec.
func (a *App) applyHotkey(action, spec string) {
	a.hkMu.Lock()
	if reg, ok := a.hotkeys[action]; ok {
		_ = reg.hk.Unregister()
		close(reg.cancel)
		delete(a.hotkeys, action)
	}
	a.hkMu.Unlock()

	mods, key, err := parseHotkeySpec(spec)
	if err != nil {
		log.Printf("hotkey %s spec %q invalid: %v", action, spec, err)
		a.emitHotkeyStatus(hotkeyStatus{Action: action, Error: err.Error()})
		return
	}
	go a.hotkeyLoop(action, mods, key)
}

// hotkeyLoop registers one hotkey (retrying once on failure) and forwards
// its events to the frontend until cancelled.
func (a *App) hotkeyLoop(action string, mods []hotkey.Modifier, key hotkey.Key) {
	hk, err := registerHotkey(action, mods, key)
	if err != nil {
		log.Printf("hotkey %s disabled: %v", action, err)
		a.emitHotkeyStatus(hotkeyStatus{Action: action, Error: err.Error()})
		return
	}
	log.Printf("hotkey %s registered", action)

	cancel := make(chan struct{})
	a.hkMu.Lock()
	// A newer registration may already have replaced us.
	if _, exists := a.hotkeys[action]; exists {
		a.hkMu.Unlock()
		_ = hk.Unregister()
		return
	}
	a.hotkeys[action] = &hotkeyReg{hk: hk, cancel: cancel}
	a.hkMu.Unlock()

	a.emitHotkeyStatus(hotkeyStatus{Action: action, Registered: true})

	for {
		select {
		case <-cancel:
			return
		case <-hk.Keydown():
			if action == "ptt" {
				log.Printf("hotkey ptt_down fired")
				a.emitHotkey("ptt_down")
			} else {
				log.Printf("hotkey %s fired", action)
				a.emitHotkey(action)
			}
		case <-hk.Keyup():
			if action == "ptt" {
				log.Printf("hotkey ptt_up fired")
				a.emitHotkey("ptt_up")
			}
		}
	}
}

// registerHotkey attempts registration once, then retries after a short
// delay (first-run races with the window/message loop are common).
func registerHotkey(action string, mods []hotkey.Modifier, key hotkey.Key) (*hotkey.Hotkey, error) {
	hk := hotkey.New(mods, key)
	if err := hk.Register(); err != nil {
		log.Printf("hotkey %s registration failed, retrying: %v", action, err)
		time.Sleep(500 * time.Millisecond)
		hk = hotkey.New(mods, key)
		if err := hk.Register(); err != nil {
			return nil, err
		}
	}
	return hk, nil
}

// SetHotkeys rebinds the global hotkeys at runtime. It returns "" on success
// or an error describing the unsupported spec.
func (a *App) SetHotkeys(pttSpec, muteSpec string) string {
	if _, _, err := parseHotkeySpec(pttSpec); err != nil {
		return "ptt: " + err.Error()
	}
	if _, _, err := parseHotkeySpec(muteSpec); err != nil {
		return "mute: " + err.Error()
	}
	a.settings.HotkeyPTT = pttSpec
	a.settings.HotkeyMute = muteSpec
	if err := saveSettings(a.settings); err != nil {
		return err.Error()
	}
	a.applyHotkey("ptt", pttSpec)
	a.applyHotkey("mute_toggle", muteSpec)
	return ""
}

// emitHotkey sends a hotkey event to the frontend.
func (a *App) emitHotkey(action string) {
	if a.ctx != nil {
		wailsRuntime.EventsEmit(a.ctx, "hotkey", action)
	}
}

// emitHotkeyStatus sends a hotkey_status event to the frontend.
func (a *App) emitHotkeyStatus(status hotkeyStatus) {
	if a.ctx != nil {
		wailsRuntime.EventsEmit(a.ctx, "hotkey_status", status)
	}
}

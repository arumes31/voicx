// hotkeys.go registers global hotkeys for push-to-talk and mute toggle.
// Bindings come from settings (defaults: Space, Ctrl+M) and can be changed
// at runtime via SetHotkeys. Registration failures are retried once, then
// surfaced to the frontend as hotkey_status events (and to the client log
// file).
package main

import (
	"fmt"
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
	for _, action := range hotkeyActions {
		a.applyHotkey(action, a.specFor(action))
	}
}

// hotkeyActions is the full bindable action set (299/300).
var hotkeyActions = []string{
	"ptt", "mute_toggle", "deafen_toggle", "whisper_reply", "quick_connect", "compact_toggle", "zen_toggle",
}

// specFor returns the configured spec for an action (empty = unbound).
func (a *App) specFor(action string) string {
	s := a.settings
	switch action {
	case "ptt":
		return s.HotkeyPTT
	case "mute_toggle":
		return s.HotkeyMute
	case "deafen_toggle":
		return s.HotkeyDeafen
	case "whisper_reply":
		return s.WhisperReplyHotkey
	case "quick_connect":
		return s.HotkeyQuickConnect
	case "compact_toggle":
		return s.HotkeyCompact
	case "zen_toggle":
		return s.HotkeyZen
	}
	return ""
}

// profileSpecs merges a named hotkey profile over the defaults (300).
func (a *App) profileSpecs(name string) map[string]string {
	specs := map[string]string{}
	for _, action := range hotkeyActions {
		specs[action] = a.specFor(action)
	}
	p, ok := a.settings.HotkeyProfiles[name]
	if !ok || name == "" || name == "default" {
		return specs
	}
	overrides := map[string]string{
		"ptt": p.PTT, "mute_toggle": p.Mute, "deafen_toggle": p.Deafen,
		"whisper_reply": p.WhisperReply, "quick_connect": p.QuickConnect, "compact_toggle": p.Compact,
		"zen_toggle": p.Zen,
	}
	for action, spec := range overrides {
		if spec != "" {
			specs[action] = spec
		}
	}
	return specs
}

// ApplyHotkeyProfile switches the live registrations to a profile (called
// when a tab with a profiled bookmark connects).
func (a *App) ApplyHotkeyProfile(name string) {
	for action, spec := range a.profileSpecs(name) {
		a.applyHotkey(action, spec)
	}
}

// applyHotkey tears down any existing registration for action and registers
// the new spec. An empty spec unbinds the action.
func (a *App) applyHotkey(action, spec string) {
	a.hkMu.Lock()
	if reg, ok := a.hotkeys[action]; ok {
		_ = reg.hk.Unregister()
		close(reg.cancel)
		delete(a.hotkeys, action)
	}
	a.hkMu.Unlock()
	if spec == "" {
		return
	}

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
// delay (first-run races with the window/message loop are common). The error
// names the action and hints at external holders (301 conflict detection).
func registerHotkey(action string, mods []hotkey.Modifier, key hotkey.Key) (*hotkey.Hotkey, error) {
	hk := hotkey.New(mods, key)
	if err := hk.Register(); err != nil {
		log.Printf("hotkey %s registration failed, retrying: %v", action, err)
		time.Sleep(500 * time.Millisecond)
		hk = hotkey.New(mods, key)
		if err := hk.Register(); err != nil {
			return nil, fmt.Errorf("%s: registration failed (%v) — another app or a duplicate binding may hold this key", action, err)
		}
	}
	return hk, nil
}

// SetHotkeys rebinds the global hotkeys at runtime. It returns "" on success
// or an error describing the unsupported spec.
func (a *App) SetHotkeys(pttSpec, muteSpec, whisperReplySpec string) string {
	if _, _, err := parseHotkeySpec(pttSpec); err != nil {
		return "ptt: " + err.Error()
	}
	if _, _, err := parseHotkeySpec(muteSpec); err != nil {
		return "mute: " + err.Error()
	}
	if _, _, err := parseHotkeySpec(whisperReplySpec); err != nil {
		return "whisper reply: " + err.Error()
	}
	a.settings.HotkeyPTT = pttSpec
	a.settings.HotkeyMute = muteSpec
	a.settings.WhisperReplyHotkey = whisperReplySpec
	if err := a.save(); err != nil {
		return err.Error()
	}
	a.applyHotkey("ptt", pttSpec)
	a.applyHotkey("mute_toggle", muteSpec)
	a.applyHotkey("whisper_reply", whisperReplySpec)
	return ""
}

// SetHotkey rebinds ONE action (299 hotkey map editor). Empty spec unbinds.
func (a *App) SetHotkey(action, spec string) string {
	valid := false
	for _, x := range hotkeyActions {
		if x == action {
			valid = true
		}
	}
	if !valid {
		return "unknown action " + action
	}
	if spec != "" {
		if _, _, err := parseHotkeySpec(spec); err != nil {
			return err.Error()
		}
	}
	switch action {
	case "ptt":
		a.settings.HotkeyPTT = spec
	case "mute_toggle":
		a.settings.HotkeyMute = spec
	case "deafen_toggle":
		a.settings.HotkeyDeafen = spec
	case "whisper_reply":
		a.settings.WhisperReplyHotkey = spec
	case "quick_connect":
		a.settings.HotkeyQuickConnect = spec
	case "compact_toggle":
		a.settings.HotkeyCompact = spec
	case "zen_toggle":
		a.settings.HotkeyZen = spec
	}
	if err := a.save(); err != nil {
		return err.Error()
	}
	a.applyHotkey(action, spec)
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

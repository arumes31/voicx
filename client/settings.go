// settings.go implements the client settings model and JSON persistence at
// <UserConfigDir>/voicx/settings.json. Settings are loaded at startup and
// saved on every change (OK/Apply in the settings dialog).
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Bookmark is a saved server (address + nickname; never passwords).
type Bookmark struct {
	Name     string `json:"name"`
	Addr     string `json:"addr"`
	Nickname string `json:"nickname"`
}

// Settings holds all user preferences.
type Settings struct {
	Bookmarks []Bookmark `json:"bookmarks"`

	// Capture (input / microphone).
	CaptureDeviceID  string `json:"capture_device_id"`
	ActivationMode   string `json:"activation_mode"` // "ptt" | "vad" | "continuous"
	VADThreshold     int    `json:"vad_threshold"`   // 0..100
	EchoCancellation bool   `json:"echo_cancellation"`
	NoiseSuppression bool   `json:"noise_suppression"`

	// Playback (output).
	PlaybackDeviceID string `json:"playback_device_id"`
	Volume           int    `json:"volume"` // 0..200 (percent)

	// Hotkeys (canonical specs like "Space", "Ctrl+M", "Ctrl+Shift+F5").
	HotkeyPTT  string `json:"hotkey_ptt"`
	HotkeyMute string `json:"hotkey_mute"`

	// Chat.
	ChatMaxLines   int  `json:"chat_max_lines"`
	LogChannelChat bool `json:"log_channel_chat"`
	LogPrivateChat bool `json:"log_private_chat"`
	LogServerChat  bool `json:"log_server_chat"`

	// Notifications.
	NotifyJoinLeave  bool `json:"notify_join_leave"`
	NotifyConnection bool `json:"notify_connection"`
	PlaySounds       bool `json:"play_sounds"`
	WhisperSound     bool `json:"whisper_sound"`

	// Whisper lists (re-applied on connect and via the Whisper settings page).
	WhisperClients  []string `json:"whisper_clients"`
	WhisperChannels []int64  `json:"whisper_channels"`
	WhisperActive   bool     `json:"whisper_active"`

	// Misc.
	DownloadFolder  string `json:"download_folder"`
	ReconnectOnLoss bool   `json:"reconnect_on_loss"`
}

// DefaultSettings returns the defaults used when no settings file exists.
func DefaultSettings() Settings {
	return Settings{
		ActivationMode:   "ptt",
		VADThreshold:     50,
		EchoCancellation: true,
		NoiseSuppression: true,
		Volume:           100,
		HotkeyPTT:        "Space",
		HotkeyMute:       "Ctrl+M",
		ChatMaxLines:     200,
		NotifyJoinLeave:  true,
		NotifyConnection: true,
		WhisperSound:     true,
	}
}

// settingsPath returns the default settings file location.
func settingsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "voicx", "settings.json"), nil
}

// loadSettingsAt loads settings from path, falling back to defaults for a
// missing or unreadable file.
func loadSettingsAt(path string) Settings {
	s := DefaultSettings()
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	// Merge onto defaults so new fields added in later versions get their
	// default values.
	if err := json.Unmarshal(data, &s); err != nil {
		return DefaultSettings()
	}
	return s
}

// saveSettingsAt writes settings to path with 0600 permissions.
func saveSettingsAt(path string, s Settings) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// loadSettings loads from the default path.
func loadSettings() Settings {
	path, err := settingsPath()
	if err != nil {
		return DefaultSettings()
	}
	return loadSettingsAt(path)
}

// saveSettings saves to the default path.
func saveSettings(s Settings) error {
	path, err := settingsPath()
	if err != nil {
		return err
	}
	return saveSettingsAt(path, s)
}

// GetSettings returns the current settings to the frontend.
func (a *App) GetSettings() Settings {
	return a.settings
}

// SaveSettings replaces and persists the settings. The frontend sends the
// whole object; hotkey specs are validated and re-applied.
func (a *App) SaveSettings(s Settings) string {
	if _, _, err := parseHotkeySpec(s.HotkeyPTT); err != nil {
		return "ptt hotkey: " + err.Error()
	}
	if _, _, err := parseHotkeySpec(s.HotkeyMute); err != nil {
		return "mute hotkey: " + err.Error()
	}
	if s.ChatMaxLines < 1 {
		return "chat max lines must be >= 1"
	}
	if s.Volume < 0 || s.Volume > 200 {
		return "volume must be 0..200"
	}
	a.settings = s
	if err := saveSettings(s); err != nil {
		return err.Error()
	}
	a.applyHotkey("ptt", s.HotkeyPTT)
	a.applyHotkey("mute_toggle", s.HotkeyMute)
	return ""
}

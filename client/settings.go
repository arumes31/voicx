// settings.go implements the client settings model and JSON persistence at
// <UserConfigDir>/voicx/settings.json. Settings are loaded at startup and
// saved on every change (OK/Apply in the settings dialog).
package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

// allowDefaultSettingsPath gates the fallback to the real
// <UserConfigDir>/voicx/settings.json when an App carries no settingsPath.
// Tests clear it (see main_test.go) so no App built without one can write
// over the developer's real bookmarks.
var allowDefaultSettingsPath = true

// Bookmark is a saved server (address + nickname; never passwords).
type Bookmark struct {
	Name        string `json:"name"`
	Addr        string `json:"addr"`
	Nickname    string `json:"nickname"`
	Folder      string `json:"folder,omitempty"`       // (283) grouping
	Order       int    `json:"order,omitempty"`        // (284) manual sort
	Color       string `json:"color,omitempty"`        // (284) tab/dot color
	AutoConnect bool   `json:"auto_connect,omitempty"` // (286) connect on startup (guest/prefill)
	Profile     string `json:"profile,omitempty"`      // (300) hotkey profile name
	// Per-server overrides (334/335): NicknameOverride replaces the login
	// nickname; AvatarOverrideB64 (base64 image) is uploaded after connect.
	NicknameOverride  string `json:"nickname_override,omitempty"`
	AvatarOverrideB64 string `json:"avatar_override_b64,omitempty"`
}

// Contact is a stored contact (316): unique ID + label; NickHistory tracks
// nicknames seen online (318, capped at 8).
type Contact struct {
	UniqueID     string   `json:"unique_id"`
	Label        string   `json:"label,omitempty"`
	NickHistory  []string `json:"nick_history,omitempty"`
	NotifyOnline bool     `json:"notify_online,omitempty"` // (383) buddy alert
}

// NotifyChannels is one row of the notification matrix (385): which outputs
// fire for an event class.
type NotifyChannels struct {
	Toast  bool `json:"toast"`
	Sound  bool `json:"sound"`
	Flash  bool `json:"flash"`
	Native bool `json:"native"`
}

// BeepSpec is a custom synthesized notification sound (384).
type BeepSpec struct {
	Freq       int `json:"freq"`        // Hz
	DurationMs int `json:"duration_ms"` // milliseconds
}

// ChannelOverride holds per-channel notification overrides (386/387/389):
// Messages/Mentions/Joins are "inherit" | "on" | "off"; Muted silences the
// channel entirely (messages still visible when selected); WatchThreshold >
// 0 toasts when the channel's user count crosses from below to at least the
// threshold.
type ChannelOverride struct {
	Messages       string `json:"messages,omitempty"`
	Mentions       string `json:"mentions,omitempty"`
	Joins          string `json:"joins,omitempty"`
	Muted          bool   `json:"muted,omitempty"`
	WatchThreshold int    `json:"watch_threshold,omitempty"`
}

// RecentServer is one entry of the connect history (282; max 10, no
// passwords).
type RecentServer struct {
	Addr     string `json:"addr"`
	Nickname string `json:"nickname"`
	LastUsed int64  `json:"last_used"` // unix
}

// HotkeyProfile is a named set of hotkey specs (300): per-server profiles
// override the default profile's actions.
type HotkeyProfile struct {
	PTT          string `json:"ptt,omitempty"`
	Mute         string `json:"mute_toggle,omitempty"`
	WhisperReply string `json:"whisper_reply,omitempty"`
	QuickConnect string `json:"quick_connect,omitempty"`
	Compact      string `json:"compact_toggle,omitempty"`
	Zen          string `json:"zen_toggle,omitempty"`
	Deafen       string `json:"deafen_toggle,omitempty"`
}

// settingsVersion is the generation of the settings file. Bump it whenever a
// default flips from the zero value, and add the repair to migrateSettings:
// loading merges the file ONTO the defaults, so a field an older client always
// wrote wins over the new default unless it is explicitly repaired.
const settingsVersion = 1

// Settings holds all user preferences.
type Settings struct {
	// SettingsVersion is the generation this file was written by (0 = before
	// versioning). See migrateSettings.
	SettingsVersion int `json:"settings_version"`

	Bookmarks []Bookmark     `json:"bookmarks"`
	Recents   []RecentServer `json:"recents"` // (282) last 10 servers

	// Window / system integration (wave 8a).
	AlwaysOnTop    bool   `json:"always_on_top"`    // (291)
	MinimizeToTray bool   `json:"minimize_to_tray"` // (287)
	CloseToTray    bool   `json:"close_to_tray"`    // (287) close button hides instead of quitting
	CompactMode    bool   `json:"compact_mode"`     // (293)
	Theme          string `json:"theme"`            // (294/295) "dark" | "light" | "hc"
	AccentColor    string `json:"accent_color"`     // (296) custom accent hex ("" = theme default)
	UserCSS        string `json:"user_css"`         // (296) scoped overrides
	UIFont         string `json:"ui_font"`          // (297) "outfit" | "sora" | "jetbrains"
	UIFontSize     int    `json:"ui_font_size"`     // (297) base px, default 14

	// Accessibility & polish (wave 8c).
	Language       string `json:"language"`         // (336) "system" | "en" | "de"
	ReduceMotion   bool   `json:"reduce_motion"`    // (344)
	SidebarWidth   int    `json:"sidebar_width"`    // (338) px, 0 = default
	DetailsWidth   int    `json:"details_width"`    // (338) px, 0 = default
	IdleVideoPause bool   `json:"idle_video_pause"` // (342) default on
	DNDEnabled     bool   `json:"dnd_enabled"`      // (347)
	DNDFrom        string `json:"dnd_from"`         // (348) quiet hours start "22:00" ("" = off)
	DNDTo          string `json:"dnd_to"`           // (348) quiet hours end "07:00"

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
	HotkeyPTT          string `json:"hotkey_ptt"`
	HotkeyMute         string `json:"hotkey_mute"`
	HotkeyDeafen       string `json:"hotkey_deafen"`        // (299) deafen toggle
	HotkeyQuickConnect string `json:"hotkey_quick_connect"` // (285) default "Ctrl+Shift+C"
	HotkeyZen          string `json:"hotkey_zen"`           // (341) zen mode toggle, default "Ctrl+Shift+Z"
	HotkeyCompact      string `json:"hotkey_compact"`       // (293/299) compact-mode toggle
	// HotkeyProfiles maps profile name -> overrides (300). The bookmark's
	// profile applies on connect; "default" is the base set above.
	HotkeyProfiles map[string]HotkeyProfile `json:"hotkey_profiles,omitempty"`

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
	DownloadFolder   string `json:"download_folder"`
	ReconnectOnLoss  bool   `json:"reconnect_on_loss"`
	UpdatesAutoCheck bool   `json:"updates_auto_check"`

	// Voice UX (wave 1).
	UserVolumes        map[string]int  `json:"user_volumes"`         // uniqueID -> volume 0..200
	MutedUsers         []string        `json:"muted_users"`          // uniqueIDs muted locally
	PTTReleaseDelayMs  int             `json:"ptt_release_delay_ms"` // 0..2000
	WarnMutedTalking   bool            `json:"warn_muted_talking"`   // default on
	WarnEmptyChannel   bool            `json:"warn_empty_channel"`   // default on
	SoundPack          string          `json:"sound_pack"`           // "soft" | "bright" | "retro"
	SoundVolume        int             `json:"sound_volume"`         // 0..200
	EventSounds        map[string]bool `json:"event_sounds"`         // event name -> enabled
	WhisperReplyHotkey string          `json:"whisper_reply_hotkey"` // default "Ctrl+R"
	VoiceLimiter       bool            `json:"voice_limiter"`        // default on
	GainNormalize      bool            `json:"gain_normalize"`

	// Video (wave 3).
	CameraFPS    int  `json:"camera_fps"`    // 15 | 30 | 60 (default 30)
	LowBandwidth bool `json:"low_bandwidth"` // (88) low-bandwidth mode

	// Security (wave 4a).
	AllowPlaintext bool `json:"allow_plaintext"` // allow plaintext control connections (dev servers)

	// Chat display (wave 5b).
	ChatTimestamps        string           `json:"chat_timestamps"`        // "absolute" | "relative" | "off"
	ChatDensity           string           `json:"chat_density"`           // "comfortable" | "compact"
	ChatFontSize          int              `json:"chat_font_size"`         // px, default 14
	ChatLayout            string           `json:"chat_layout"`            // "irc" | "bubbles"
	SysJoinLeave          bool             `json:"sys_join_leave"`         // show join/leave system lines (default on)
	SysKick               bool             `json:"sys_kick"`               // show kick system lines (default on)
	LastReadChannels      map[string]int64 `json:"last_read_channels"`     // channelID -> last-read message id (121)
	DismissedAnnouncement string           `json:"dismissed_announcement"` // hash of the dismissed announcement (132)

	// Social & meta (wave 8b).
	Contacts        []Contact          `json:"contacts,omitempty"`        // (316)
	BlockedUsers    []string           `json:"blocked_users,omitempty"`   // (317) unique IDs
	UserNotes       map[string]string  `json:"user_notes,omitempty"`      // (315) uniqueID -> local note
	RecentChannels  map[string][]int64 `json:"recent_channels,omitempty"` // (320) server addr -> last 5 channel IDs
	AutoAwayMinutes int                `json:"auto_away_minutes"`         // (308) 0 = off, default 15
	OnboardingDone  bool               `json:"onboarding_done"`           // (329)
	LastSeenVersion string             `json:"last_seen_version"`         // (330) what's-new tracking

	// Notifications (wave 9).
	NotifyMatrix   map[string]NotifyChannels  `json:"notify_matrix,omitempty"`  // (385) event -> outputs
	CustomSounds   map[string]BeepSpec        `json:"custom_sounds,omitempty"`  // (384) event -> beep
	ChannelNotify  map[string]ChannelOverride `json:"channel_notify,omitempty"` // (386-389) "addr#channelID" -> override
	Keywords       map[string][]string        `json:"keywords,omitempty"`       // (388) server addr -> keywords
	AlphaDismissed string                     `json:"alpha_dismissed"`          // (215) version whose notice was dismissed
}

// DefaultSettings returns the defaults used when no settings file exists.
func DefaultSettings() Settings {
	return Settings{
		SettingsVersion:    settingsVersion,
		PlaySounds:         true,
		ActivationMode:     "ptt",
		VADThreshold:       50,
		EchoCancellation:   true,
		NoiseSuppression:   true,
		Volume:             100,
		HotkeyPTT:          "Space",
		HotkeyMute:         "Ctrl+M",
		HotkeyQuickConnect: "Ctrl+Shift+C",
		HotkeyZen:          "Ctrl+Shift+Z",
		IdleVideoPause:     true,
		Theme:              "dark",
		UIFont:             "outfit",
		UIFontSize:         14,
		ChatMaxLines:       200,
		NotifyJoinLeave:    true,
		NotifyConnection:   true,
		WhisperSound:       true,
		UpdatesAutoCheck:   true,

		PTTReleaseDelayMs: 0,
		WarnMutedTalking:  true,
		WarnEmptyChannel:  true,
		SoundPack:         "soft",
		SoundVolume:       100,
		EventSounds: map[string]bool{
			"join": true, "leave": true, "mention": true, "dm": true,
			"whisper": true, "poke": true, "mic_on": true, "mic_off": true,
		},
		WhisperReplyHotkey: "Ctrl+R",
		VoiceLimiter:       true,
		CameraFPS:          30,
		ChatTimestamps:     "absolute",
		ChatDensity:        "comfortable",
		ChatFontSize:       14,
		ChatLayout:         "irc",
		SysJoinLeave:       true,
		SysKick:            true,
		AutoAwayMinutes:    15,
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
	return migrateSettings(s)
}

// migrateSettings repairs a file written by an older client. A field the older
// client always serialised takes precedence over its new default, so any
// default that flips from the zero value has to be restored explicitly here.
func migrateSettings(s Settings) Settings {
	if s.SettingsVersion < 1 {
		// Every client before the master sound gate wrote play_sounds:false,
		// which would now mute every notification sound (28).
		s.PlaySounds = true
	}
	s.SettingsVersion = settingsVersion
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

// settingsFile returns the effective settings path: the per-App override
// (tests) or the default location.
func (a *App) settingsFile() string {
	if a.settingsPath != "" {
		return a.settingsPath
	}
	if !allowDefaultSettingsPath {
		return ""
	}
	path, err := settingsPath()
	if err != nil {
		return ""
	}
	return path
}

// save persists the App's settings to its settings file.
func (a *App) save() error {
	if path := a.settingsFile(); path != "" {
		return saveSettingsAt(path, a.settings)
	}
	return nil
}

// GetSettings returns the current settings to the frontend.
func (a *App) GetSettings() Settings {
	return a.settings
}

// emitSettingsUpdate hands the frontend the merged settings blob. Every field
// the Go side writes on its own (282 recents, 330 what's-new marker) must be
// announced through here; the frontend has no other way to learn of it before
// the next restart.
func (a *App) emitSettingsUpdate() {
	a.emitPlain("settings_update", a.settings)
}

// mergeGoOwned returns incoming with the fields the Go side maintains on its
// own taken from cur. The frontend caches the whole settings blob and ships
// it back on every save, so a copy taken before the Go side wrote them would
// otherwise wipe them (282 recents, 330 what's-new marker).
func mergeGoOwned(cur, incoming Settings) Settings {
	incoming.Recents = cur.Recents
	incoming.LastSeenVersion = cur.LastSeenVersion
	return incoming
}

// SaveSettings replaces and persists the settings. The frontend sends the
// whole object (Go-owned fields are kept, see mergeGoOwned); hotkey specs
// are validated and re-applied.
func (a *App) SaveSettings(s Settings) string {
	if _, _, err := parseHotkeySpec(s.HotkeyPTT); err != nil {
		return "ptt hotkey: " + err.Error()
	}
	if _, _, err := parseHotkeySpec(s.HotkeyMute); err != nil {
		return "mute hotkey: " + err.Error()
	}
	if _, _, err := parseHotkeySpec(s.WhisperReplyHotkey); err != nil {
		return "whisper reply hotkey: " + err.Error()
	}
	if s.ChatMaxLines < 1 {
		return "chat max lines must be >= 1"
	}
	if s.Volume < 0 || s.Volume > 200 {
		return "volume must be 0..200"
	}
	a.settings = mergeGoOwned(a.settings, s)
	if err := a.save(); err != nil {
		return err.Error()
	}
	a.emitSettingsUpdate()
	a.applyHotkey("ptt", s.HotkeyPTT)
	a.applyHotkey("mute_toggle", s.HotkeyMute)
	a.applyHotkey("deafen_toggle", s.HotkeyDeafen)
	a.applyHotkey("quick_connect", s.HotkeyQuickConnect)
	a.applyHotkey("compact_toggle", s.HotkeyCompact)
	a.applyHotkey("whisper_reply", s.WhisperReplyHotkey)
	return ""
}

// RecordRecent prepends addr+nickname to the connect history (282): most
// recent first, deduped, capped at 10. Called after a successful connect.
func (a *App) RecordRecent(addr, nickname string) {
	if addr == "" {
		return
	}
	rec := RecentServer{Addr: addr, Nickname: nickname, LastUsed: time.Now().Unix()}
	out := []RecentServer{rec}
	for _, r := range a.settings.Recents {
		if r.Addr == addr && r.Nickname == nickname {
			continue
		}
		out = append(out, r)
	}
	if len(out) > 10 {
		out = out[:10]
	}
	a.settings.Recents = out
	if err := a.save(); err != nil {
		log.Printf("saving settings recents failed: %v", err)
		return
	}
	a.emitSettingsUpdate()
}

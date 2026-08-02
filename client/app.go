// app.go defines the Wails-bound application API. Methods on App are
// callable from the frontend; server state lives in the connManager.
package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"voicx/internal/netproto"
	"voicx/internal/version"
)

// App is the Wails application.
type App struct {
	ctx context.Context
	// cm is the ACTIVE tab's connManager (281 multi-server tabs): all
	// bindings keep operating on it. Background tabs live in tabs and their
	// events are journaled/replayed by tabs.go. Access via cmLoad/cmStore
	// (atomic; tab switches race with bindings).
	cm       atomic.Pointer[connManager]
	tabs     map[string]*tabState
	tabsMu   sync.Mutex
	activeID string
	settings Settings
	// settingsPath overrides the settings file location (tests; empty =
	// default UserConfigDir path).
	settingsPath string
	// knownServers is the shared TOFU store for all tabs.
	knownServers *knownServers

	hkMu    sync.Mutex
	hotkeys map[string]*hotkeyReg

	// readMu guards settings.LastReadChannels: server pushes from the read
	// loop race the frontend's own mark-read calls (121).
	readMu sync.Mutex
	// dmMu serialises the local DM logs — every append rewrites a whole
	// file, so two concurrent DMs with one peer would otherwise lose one (122).
	dmMu sync.Mutex
}

// NewApp creates a new App.
func NewApp() *App {
	return &App{
		settings: loadSettings(),
		hotkeys:  make(map[string]*hotkeyReg),
		tabs:     make(map[string]*tabState),
	}
}

// cmLoad returns the active tab's connManager (may be nil).
func (a *App) cmLoad() *connManager { return a.cm.Load() }

// cmStore sets the active tab's connManager.
func (a *App) cmStore(cm *connManager) { a.cm.Store(cm) }

// startup is called when the app starts.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	if path, err := knownServersPath(); err == nil {
		a.knownServers = loadKnownServersAt(path)
	}
	a.registerHotkeys()
	// (291) restore always-on-top.
	if a.settings.AlwaysOnTop {
		wailsRuntime.WindowSetAlwaysOnTop(ctx, true)
	}
	// recover is per-goroutine (331).
	go guardCrash("window-watch", a.windowWatch)
}

// windowWatch restores the persisted opacity once the native window exists
// (292) and polls the minimised state (288). Wails v2 emits no minimize
// event, but WindowIsMinimised is a live query, so acting on the rising edge
// is what puts a minimised window in the tray instead of the taskbar.
func (a *App) windowWatch() {
	tick := time.NewTicker(400 * time.Millisecond)
	defer tick.Stop()
	applied := false
	wasMin := false
	for range tick.C {
		if a.ctx == nil {
			continue
		}
		// (292) the layered-window call needs an HWND, which only exists once
		// the window is up; the first success ends the retry.
		if !applied && setWindowOpacity(a.settings.WindowOpacity) == nil {
			applied = true
		}
		min := wailsRuntime.WindowIsMinimised(a.ctx)
		if min && !wasMin && a.settings.MinimizeToTray {
			// unminimise before hiding: a window hidden while minimised comes
			// back minimised when the tray shows it again.
			wailsRuntime.WindowUnminimise(a.ctx)
			wailsRuntime.WindowHide(a.ctx)
			trayMarkHidden()
			min = false
		}
		wasMin = min
	}
}

// SetWindowOpacity sets the window transparency in percent (292) and persists
// it. The value is clamped so the window can never be made invisible and
// therefore unclickable.
func (a *App) SetWindowOpacity(pct int) string {
	if pct < 20 {
		pct = 20
	}
	if pct > 100 {
		pct = 100
	}
	a.settings.WindowOpacity = pct
	_ = a.save()
	if err := setWindowOpacity(pct); err != nil {
		return err.Error()
	}
	return ""
}

// shutdown is called when the app closes.
func (a *App) shutdown(_ context.Context) {
	for _, ts := range a.tabsRegistry() {
		ts.cm.disconnect()
	}
}

// Connect dials the server and authenticates. nickname is the account
// nickname (or unique ID); with an empty password the client logs in as a
// guest using its own generated identity. It returns "" on success or the
// failure reason.
func (a *App) Connect(addr, nickname, password, serverPassword string) string {
	return a.ConnectTab(addr, nickname, password, serverPassword)
}

// ConnectGuest dials the server and authenticates as a guest using the
// client's own Ed25519 identity (key-derived unique ID). It returns "" on
// success or the failure reason.
func (a *App) ConnectGuest(addr, nickname string) string {
	return a.ConnectGuestTab(addr, nickname)
}

// Disconnect closes the server connection.
func (a *App) Disconnect() {
	if a.activeID != "" {
		a.CloseTab(a.activeID)
	}
}

// Connected reports whether the client is connected.
func (a *App) Connected() bool {
	return a.cmLoad() != nil && a.cmLoad().connected()
}

// JoinChannel joins (moves into) a channel.
func (a *App) JoinChannel(channelID int64) string {
	if err := a.cmLoad().write(netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: channelID}); err != nil {
		return err.Error()
	}
	return ""
}

// MoveClient moves another client into a channel (gated server-side by
// i_client_move_power; 305 drag & drop).
func (a *App) MoveClient(clientID string, channelID int64) string {
	if err := a.cmLoad().write(netproto.MsgMoveClient, netproto.MoveClient{
		ClientID: clientID, ChannelID: channelID,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// GetICEServers returns the ICE servers the server provided at connect
// (STUN/TURN), or nil when the client should use its built-in defaults.
func (a *App) GetICEServers() []netproto.ICEServer {
	return a.cmLoad().iceServersSnapshot()
}

// ServerFingerprint returns the SHA-256 fingerprint of the current (or last
// attempted) server's control-channel certificate, or "" for plaintext.
func (a *App) ServerFingerprint() string {
	if a.cmLoad() == nil {
		return ""
	}
	_, fp, _ := a.cmLoad().securitySnapshot()
	return fp
}

// TrustServerFingerprint pins fp for addr in the TOFU store (explicit user
// action after a fingerprint-mismatch warning).
func (a *App) TrustServerFingerprint(addr, fp string) string {
	if a.cmLoad() == nil || a.cmLoad().knownServers == nil {
		return "trust store unavailable"
	}
	if addr == "" || fp == "" {
		return "address and fingerprint are required"
	}
	if err := a.cmLoad().knownServers.trust(addr, fp); err != nil {
		return err.Error()
	}
	return ""
}

// ConnectionSecurity describes how the current connection is secured, for
// display in the UI: "TLS (fingerprint …[, first seen])", "PLAINTEXT", or
// "offline".
func (a *App) ConnectionSecurity() string {
	if a.cmLoad() == nil || !a.cmLoad().connected() {
		return "offline"
	}
	tlsUsed, fp, newServer := a.cmLoad().securitySnapshot()
	if !tlsUsed {
		return "PLAINTEXT — traffic is NOT encrypted"
	}
	if newServer {
		return "TLS (new server fingerprint pinned: " + fp + ")"
	}
	return "TLS (fingerprint " + fp + ")"
}

// MOTD returns the server's message of the day delivered in the AuthResponse
// ("" when unset or offline). Surfaced in chat once per connect (133).
func (a *App) MOTD() string {
	if a.cmLoad() == nil {
		return ""
	}
	return a.cmLoad().motdSnapshot()
}

// ClientID returns the server-assigned client ID of the current connection
// ("" when not connected).
func (a *App) ClientID() string {
	// the tab-switch identity refresh (281) can reach this while no manager is
	// stored, and clientIDSnapshot takes its mutex before reading.
	if a.cmLoad() == nil {
		return ""
	}
	return a.cmLoad().clientIDSnapshot()
}

// SetPrioritySpeaker toggles the calling client's priority-speaker flag
// (gated server-side by b_client_priority_speaker). It returns "" on success
// or the failure reason.
func (a *App) SetPrioritySpeaker(active bool) string {
	if err := a.cmLoad().write(netproto.MsgPrioritySpeaker, netproto.PrioritySpeaker{Active: active}); err != nil {
		return err.Error()
	}
	return ""
}

// ChannelEdit edits a channel's settings (gated server-side by
// b_channel_modify). The dialog always sends the full form, so every field
// is set explicitly. It returns "" on success or the failure reason.
func (a *App) ChannelEdit(channelID int64, topic string, maxClients int, opusBitrate int, opusFEC bool, opusDTX bool, opusStereo bool, description string, slowModeSeconds int) string {
	if err := a.cmLoad().write(netproto.MsgChannelEdit, netproto.ChannelEdit{
		ChannelID:   channelID,
		Topic:       &topic,
		MaxClients:  &maxClients,
		OpusBitrate: &opusBitrate,
		OpusFEC:     &opusFEC,
		OpusDTX:     &opusDTX,
		OpusStereo:  &opusStereo,
		Description: &description,
		// (114) the dialog always sends the full form, so 0 means "off"
		// rather than "unchanged".
		SlowModeSeconds: &slowModeSeconds,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// SendChat sends a chat message. scope is "global", "channel", or "direct"
// (target = channel ID for "channel", unique ID for "direct"). Messages are
// encrypted in the backend (4b): DMs are true E2EE (nacl/box), channel and
// global use the server-distributed scope keys (secretbox).
func (a *App) SendChat(scope, target, text string) string {
	if text == "" {
		return "empty message"
	}
	msg, err := a.cmLoad().encryptChat(scope, target, text)
	if err != nil {
		return "encryption failed: " + err.Error()
	}
	if err := a.cmLoad().write(netproto.MsgChatSend, msg); err != nil {
		return err.Error()
	}
	return ""
}

// WhisperSet configures the whisper list and mode.
func (a *App) WhisperSet(uniqueIDs []string, channelIDs []int64, active bool) string {
	if err := a.cmLoad().write(netproto.MsgWhisperSet, netproto.WhisperSet{
		UniqueIDs:  uniqueIDs,
		ChannelIDs: channelIDs,
		Active:     active,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// GetPermissions returns the caller's resolved permission set.
func (a *App) GetPermissions() ([]netproto.PermissionEntry, error) {
	f, err := a.cmLoad().request(netproto.MsgPermissionsQuery, netproto.MsgPermissionsResponse,
		netproto.PermissionsQuery{}, 5*time.Second)
	if err != nil {
		return nil, err
	}
	var resp netproto.PermissionsResponse
	if err := decodeJSON(f, &resp); err != nil {
		return nil, err
	}
	return resp.Entries, nil
}

// WebRTCOffer sends the browser's SDP offer and returns the server's answer.
// tracks declares which slot every outbound track occupies (70): the router
// carries one source per slot, so an undeclared second audio track would
// contend with the microphone for the default slot and one of them would be
// dropped. The declaration REPLACES the previous one, so every offer must
// carry the complete list.
func (a *App) WebRTCOffer(sdp string, tracks []netproto.TrackSlot) (string, error) {
	f, err := a.cmLoad().request(netproto.MsgWebRTCOffer, netproto.MsgWebRTCAnswer,
		netproto.WebRTCOffer{SDP: sdp, Tracks: tracks}, 10*time.Second)
	if err != nil {
		return "", err
	}
	var answer netproto.WebRTCAnswer
	if err := decodeJSON(f, &answer); err != nil {
		return "", err
	}
	return answer.SDP, nil
}

// WebRTCAnswer forwards a renegotiation answer to the server.
func (a *App) WebRTCAnswer(sdp string) {
	_ = a.cmLoad().write(netproto.MsgWebRTCAnswer, netproto.WebRTCAnswer{SDP: sdp})
}

// SendICECandidate forwards a browser ICE candidate to the server.
func (a *App) SendICECandidate(candidate, sdpMid string, sdpMLineIndex uint16) {
	_ = a.cmLoad().write(netproto.MsgICECandidate, netproto.ICECandidate{
		Candidate:     candidate,
		SDPMid:        sdpMid,
		SDPMLineIndex: sdpMLineIndex,
	})
}

// SetScreenShare declares screen-share state to the channel.
func (a *App) SetScreenShare(active bool) string {
	// the voice teardown declares "not sharing" while disconnecting, and a
	// tab switch stores a nil manager: write takes its mutex before looking
	// at the connection, so a nil manager panics rather than erroring.
	if a.cmLoad() == nil {
		return "not connected"
	}
	if err := a.cmLoad().write(netproto.MsgScreenShare, netproto.ScreenShare{Active: active}); err != nil {
		return err.Error()
	}
	return ""
}

// SetVideoQuality requests a simulcast layer for the video this client
// receives: "high", "mid", or "low" (server maps to RID f/h/q).
func (a *App) SetVideoQuality(quality string) string {
	if a.cmLoad() == nil {
		return "not connected"
	}
	if err := a.cmLoad().write(netproto.MsgVideoQuality, netproto.VideoQuality{Quality: quality}); err != nil {
		return err.Error()
	}
	return ""
}

// SetMuted records the mute state (drives the status display; the actual
// track toggling happens in the frontend).
func (a *App) SetMuted(muted bool) {
	traySetMuted(muted)
	a.cmLoad().emit("muted", muted)
}

// SetPTT records the push-to-talk state.
func (a *App) SetPTT(active bool) {
	traySetPTT(active)
	a.cmLoad().emit("ptt", active)
}

// SetAvatar uploads an avatar image (base64).
func (a *App) SetAvatar(dataBase64 string) string {
	if err := a.cmLoad().write(netproto.MsgAvatarSet, netproto.AvatarSet{DataBase64: dataBase64}); err != nil {
		return err.Error()
	}
	return ""
}

// SetAlwaysOnTop toggles always-on-top (291) and persists the setting.
func (a *App) SetAlwaysOnTop(on bool) {
	a.settings.AlwaysOnTop = on
	_ = a.save()
	if a.ctx != nil {
		wailsRuntime.WindowSetAlwaysOnTop(a.ctx, on)
	}
}

// Greet is kept from the scaffold as a binding smoke test.
func (a *App) Greet(name string) string {
	return fmt.Sprintf("Hello %s, welcome to voicx!", name)
}

// ClientVersion returns the full embedded version string.
func (a *App) ClientVersion() string {
	return version.String()
}

// ClientVersionShort returns the short version (base + build number).
func (a *App) ClientVersionShort() string {
	return version.Short()
}

// IdentityUID returns the unique ID derived from the client's identity key
// (used by the login screen to show the auto-generated identity).
func (a *App) IdentityUID() string {
	if a.cmLoad() == nil {
		return ""
	}
	id, err := a.cmLoad().identity()
	if err != nil {
		return ""
	}
	uid, err := id.uniqueID()
	if err != nil {
		return ""
	}
	return uid
}

// GetAvatar fetches a user's avatar from the server.
func (a *App) GetAvatar(uniqueID string) (netproto.AvatarData, error) {
	f, err := a.cmLoad().request(netproto.MsgAvatarGet, netproto.MsgAvatarData,
		netproto.AvatarGet{UniqueID: uniqueID}, 5*time.Second)
	if err != nil {
		return netproto.AvatarData{}, err
	}
	var data netproto.AvatarData
	if err := decodeJSON(f, &data); err != nil {
		return netproto.AvatarData{}, err
	}
	return data, nil
}

// GetClientInfo fetches the connection info of an online client (TS3-style
// Client Info dialog).
func (a *App) GetClientInfo(clientID string) (netproto.ClientInfoResponse, error) {
	f, err := a.cmLoad().request(netproto.MsgClientInfoQuery, netproto.MsgClientInfoResponse,
		netproto.ClientInfoQuery{ClientID: clientID}, 5*time.Second)
	if err != nil {
		return netproto.ClientInfoResponse{}, err
	}
	var resp netproto.ClientInfoResponse
	if err := decodeJSON(f, &resp); err != nil {
		return netproto.ClientInfoResponse{}, err
	}
	return resp, nil
}

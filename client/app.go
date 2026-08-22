// app.go defines the Wails-bound application API. Methods on App are
// callable from the frontend; server state lives in the connManager.
package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"voicx/internal/netproto"
	"voicx/internal/version"
)

// windowOpacityApply is replaceable in ordering tests. Production uses the
// platform implementation.
var windowOpacityApply = setWindowOpacity

// alwaysOnTopApply is replaceable in effect-ordering tests. Production uses
// Wails only when a live application context is available.
var alwaysOnTopApply = func(ctx context.Context, on bool) {
	if ctx != nil {
		wailsRuntime.WindowSetAlwaysOnTop(ctx, on)
	}
}

var (
	lifecyclePollInterval = 400 * time.Millisecond
	windowIsMinimized     = wailsRuntime.WindowIsMinimised
	windowUnminimize      = wailsRuntime.WindowUnminimise
	windowHide            = wailsRuntime.WindowHide
	windowMarkHidden      = trayMarkHidden
)

// App is the Wails application.
type App struct {
	ctx context.Context
	// Attachment seams keep the native dialog, transfer, and final replacement
	// independently testable without putting plaintext or destination paths on
	// the Wails/JavaScript boundary. Nil fields use production implementations.
	chatAttachmentFetch      func(*connManager, int64, string) ([]byte, error)
	chatAttachmentSaveDialog func(context.Context, wailsRuntime.SaveDialogOptions) (string, error)
	chatAttachmentWrite      func(string, []byte) error
	lifecycleMu              sync.Mutex
	lifecycleCancel          context.CancelFunc
	// cm is the ACTIVE tab's connManager (281 multi-server tabs): all
	// bindings keep operating on it. Background tabs live in tabs and their
	// events are journaled/replayed by tabs.go. Access via cmLoad/cmStore
	// (atomic; tab switches race with bindings).
	cm       atomic.Pointer[connManager]
	tabs     map[string]*tabState
	tabOrder []string
	tabsMu   sync.Mutex
	// activationPublishMu serializes frontend reset/replay batches. The
	// generation itself is guarded by tabsMu; publication never holds tabsMu
	// across Wails event delivery.
	activationPublishMu  sync.Mutex
	activationGeneration uint64
	activeID             string
	tabSeq               atomic.Uint64
	settings             Settings
	// settingsMu guards settings fields accessed by background goroutines.
	settingsMu sync.Mutex
	// settingsTxMu serializes the complete durable-settings transaction. It
	// deliberately remains separate from settingsMu: a transaction reads or
	// publishes memory under settingsMu, but holds no settings lock while it
	// writes the snapshot to disk.
	settingsTxMu sync.Mutex
	// settingsEffectMu serializes OS/Wails/hotkey effects independently of
	// settingsMu. Commit generations are per effect family: an always-on-top
	// update must not suppress a pending opacity application (and vice versa).
	settingsEffectMu            sync.Mutex
	hotkeyEffectGeneration      uint64
	opacityEffectGeneration     uint64
	alwaysOnTopEffectGeneration uint64
	settingsGeneration          uint64
	// settingsPath overrides the settings file location (tests; empty =
	// default UserConfigDir path).
	settingsPath string
	// knownServers is the shared TOFU store for all tabs.
	knownServers *knownServers
	// eventEmit is a test seam for observing App-level Wails events. Nil uses
	// the real runtime event emitter.
	eventEmit func(name string, payload any)
	// beforeSettingsEffect is a deterministic test barrier after an effect has
	// read its first snapshot and before it can claim the serialized commit.
	beforeSettingsEffect func(uint64, Settings)
	// beforeSettingsTransaction is a deterministic test seam immediately
	// before a caller queues for the durable-settings transaction.
	beforeSettingsTransaction func()

	hkMu    sync.Mutex
	hotkeys map[string]*hotkeyReg

	// opacityMu serialises OS-level setWindowOpacity calls so the background
	// watcher and SetWindowOpacity cannot interleave and leave persisted vs.
	// visible opacity divergent (292).
	opacityMu sync.Mutex

	// dmMu serialises the local DM logs — every append rewrites a whole
	// file, so two concurrent DMs with one peer would otherwise lose one (122).
	dmMu sync.Mutex

	// identityMu serializes identity-store reads and mutations. Identity files
	// are account credentials, so interleaving a switch/regeneration/delete
	// must never select one path and write another.
	identityMu sync.Mutex
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

// requireCM captures the active manager exactly once for an operation. A tab
// switch after this point must not redirect the rest of that operation.
func (a *App) requireCM() (*connManager, error) {
	cm := a.cmLoad()
	if cm == nil {
		return nil, fmt.Errorf("not connected")
	}
	return cm, nil
}

// request and write are the one-frame operation boundaries used by bindings
// that do not need to retain the manager for a later local step. They still
// capture it once through requireCM, so an active-tab switch cannot split a
// request between managers.
func (a *App) request(send, reply netproto.MessageType, msg any, timeout time.Duration) (*netproto.Frame, error) {
	cm, err := a.requireCM()
	if err != nil {
		return nil, err
	}
	return cm.request(send, reply, msg, timeout)
}

func (a *App) write(mt netproto.MessageType, msg any) error {
	cm, err := a.requireCM()
	if err != nil {
		return err
	}
	return cm.write(mt, msg)
}

// cmStore sets the active tab's connManager.
func (a *App) cmStore(cm *connManager) { a.cm.Store(cm) }

// startup is called when the app starts.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	if path, err := knownServersPath(); err == nil {
		a.knownServers = loadKnownServersAt(path)
	} else {
		a.knownServers = &knownServers{Servers: map[string]string{}, loadErr: trustStoreUnavailable("locate path: %v", err)}
	}
	a.registerHotkeys()
	// (291) restore always-on-top.
	a.settingsMu.Lock()
	alwaysOnTop := a.settings.AlwaysOnTop
	a.settingsMu.Unlock()
	if alwaysOnTop {
		wailsRuntime.WindowSetAlwaysOnTop(ctx, true)
	}
	a.lifecycleMu.Lock()
	if a.lifecycleCancel != nil {
		a.lifecycleCancel()
	}
	watchCtx, cancel := context.WithCancel(ctx)
	a.lifecycleCancel = cancel
	a.lifecycleMu.Unlock()
	go guardCrash("window-opacity", func() { a.restoreWindowOpacity(watchCtx) })
	go guardCrash("window-watch", func() { a.watchMinimized(watchCtx) })
}

// windowWatch restores the persisted opacity once the native window exists
// (292) and polls the minimised state (288). Wails v2 emits no minimize
// event, but WindowIsMinimised is a live query, so acting on the rising edge
// is what puts a minimised window in the tray instead of the taskbar.
func (a *App) restoreWindowOpacity(ctx context.Context) {
	tick := time.NewTicker(lifecyclePollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		// (292) the layered-window call needs an HWND, which only exists once
		// the window is up; the first success ends the retry.
		a.settingsMu.Lock()
		opacity := a.settings.WindowOpacity
		a.settingsMu.Unlock()
		a.opacityMu.Lock()
		err := windowOpacityApply(opacity)
		a.opacityMu.Unlock()
		if err == nil {
			return
		}
	}
}

func (a *App) watchMinimized(ctx context.Context) {
	tick := time.NewTicker(lifecyclePollInterval)
	defer tick.Stop()
	wasMin := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if a.ctx == nil {
			continue
		}
		a.settingsMu.Lock()
		minimizeToTray := a.settings.MinimizeToTray
		a.settingsMu.Unlock()
		min := windowIsMinimized(a.ctx)
		if min && !wasMin && minimizeToTray {
			// unminimise before hiding: a window hidden while minimised comes
			// back minimised when the tray shows it again.
			windowUnminimize(a.ctx)
			windowHide(a.ctx)
			windowMarkHidden()
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
	generation, err := a.updateSettings(func(settings Settings) Settings {
		settings.WindowOpacity = pct
		return settings
	})
	if err != nil {
		return err.Error()
	}
	if err := a.applyOpacityEffect(generation); err != nil {
		return err.Error()
	}
	return ""
}

// shutdown is called when the app closes.
func (a *App) shutdown(_ context.Context) {
	a.lifecycleMu.Lock()
	if a.lifecycleCancel != nil {
		a.lifecycleCancel()
		a.lifecycleCancel = nil
	}
	a.lifecycleMu.Unlock()
	a.hkMu.Lock()
	for action, reg := range a.hotkeys {
		reg.stop()
		delete(a.hotkeys, action)
	}
	a.hkMu.Unlock()
	for _, ts := range a.tabsRegistry() {
		ts.cm.disconnect()
	}
	closeDailyLogs()
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
	a.tabsMu.Lock()
	activeID := a.activeID
	a.tabsMu.Unlock()
	if activeID != "" {
		a.CloseTab(activeID)
	}
}

// Connected reports whether the client is connected.
func (a *App) Connected() bool {
	cm := a.cmLoad()
	return cm != nil && cm.connected()
}

// JoinChannel joins (moves into) a channel.
func (a *App) JoinChannel(channelID int64) string {
	cm, err := a.requireCM()
	if err != nil {
		return err.Error()
	}
	if err := cm.write(netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: channelID}); err != nil {
		return err.Error()
	}
	return ""
}

// MoveClient moves another client into a channel (gated server-side by
// i_client_move_power; 305 drag & drop).
func (a *App) MoveClient(clientID string, channelID int64) string {
	cm, err := a.requireCM()
	if err != nil {
		return err.Error()
	}
	if err := cm.write(netproto.MsgMoveClient, netproto.MoveClient{
		ClientID: clientID, ChannelID: channelID,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// GetICEServers returns the ICE servers the server provided at connect
// (STUN/TURN), or nil when the client should use its built-in defaults.
func (a *App) GetICEServers() []netproto.ICEServer {
	cm, err := a.requireCM()
	if err != nil {
		return nil
	}
	return cm.iceServersSnapshot()
}

// ServerFingerprint returns the SHA-256 fingerprint of the current (or last
// attempted) server's control-channel certificate, or "" for plaintext.
func (a *App) ServerFingerprint() string {
	cm := a.cmLoad()
	if cm == nil {
		return ""
	}
	_, fp, _ := cm.securitySnapshot()
	return fp
}

const certificateClockSkewTolerance = 10 * time.Minute

// CertificateClockWarning returns an advisory warning when the local system
// time is well outside the peer certificate's validity window. Fingerprint
// verification remains authoritative; this warning never changes trust state.
func (a *App) CertificateClockWarning() string {
	cm := a.cmLoad()
	if cm == nil || !cm.connected() {
		return ""
	}
	notBefore, notAfter, trusted := cm.certificateValiditySnapshot()
	if !trusted {
		return ""
	}
	return certificateClockWarning(time.Now(), notBefore, notAfter)
}

func certificateClockWarning(now, notBefore, notAfter time.Time) string {
	const trustContext = "VoicX connected using fingerprint pinning; certificate dates did not decide trust. "
	if now.IsZero() || notBefore.IsZero() || notAfter.IsZero() {
		return ""
	}
	if !notAfter.After(notBefore) {
		return trustContext + "The server certificate has an invalid validity window; " +
			"certificate-validity checks are unreliable"
	}
	// Certificate dates come from the peer, not an authenticated time source.
	// Keep the diagnosis conditional and advisory; TOFU remains authoritative.
	if now.Before(notBefore.Add(-certificateClockSkewTolerance)) {
		return fmt.Sprintf(
			trustContext+"The local system clock may be inaccurate, or the server certificate dates may be unusual: "+
				"certificate validity starts at %s; check date, time, and time zone before relying on certificate validity",
			notBefore.UTC().Format(time.RFC3339),
		)
	}
	if now.After(notAfter.Add(certificateClockSkewTolerance)) {
		return fmt.Sprintf(
			trustContext+"The local system clock may be inaccurate, or the server certificate may be expired: "+
				"certificate validity ended at %s; check date, time, and time zone before relying on certificate validity",
			notAfter.UTC().Format(time.RFC3339),
		)
	}
	return ""
}

// TrustServerFingerprint pins fp for addr in the TOFU store (explicit user
// action after a fingerprint-mismatch warning).
func (a *App) TrustServerFingerprint(addr, fp string) string {
	ks := a.knownServers
	if ks == nil {
		if cm := a.cmLoad(); cm != nil {
			ks = cm.knownServers
		}
	}
	if ks == nil {
		return "trust store unavailable"
	}
	if addr == "" || fp == "" {
		return "address and fingerprint are required"
	}
	normalized, err := normalizeFingerprint(fp)
	if err != nil {
		return "invalid fingerprint: " + err.Error()
	}
	if err := ks.trust(addr, normalized); err != nil {
		return err.Error()
	}
	return ""
}

// ConnectionSecurity describes how the current connection is secured, for
// display in the UI: "TLS (fingerprint …[, first seen])", "PLAINTEXT", or
// "offline".
func (a *App) ConnectionSecurity() string {
	cm := a.cmLoad()
	if cm == nil || !cm.connected() {
		return "offline"
	}
	tlsUsed, fp, newServer := cm.securitySnapshot()
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
	cm, err := a.requireCM()
	if err != nil {
		return ""
	}
	return cm.motdSnapshot()
}

// AcceptServerRules accepts exactly the rules revision displayed by the
// blocking first-join prompt (216). The server answers with another
// ServerRules frame: an empty payload closes the gate, while changed rules
// replace the prompt and require a fresh click.
func (a *App) AcceptServerRules(hash string) string {
	if hash == "" {
		return "rules hash is required"
	}
	m := a.cmLoad()
	if m == nil {
		return "not connected"
	}
	if err := m.write(netproto.MsgServerRulesAccept, netproto.ServerRulesAccept{Hash: hash}); err != nil {
		return err.Error()
	}
	return ""
}

// ClientID returns the server-assigned client ID of the current connection
// ("" when not connected).
func (a *App) ClientID() string {
	// the tab-switch identity refresh (281) can reach this while no manager is
	// stored, and clientIDSnapshot takes its mutex before reading.
	m := a.cmLoad()
	if m == nil {
		return ""
	}
	return m.clientIDSnapshot()
}

// SetPrioritySpeaker toggles the calling client's priority-speaker flag
// (gated server-side by b_client_priority_speaker). It returns "" on success
// or the failure reason.
func (a *App) SetPrioritySpeaker(active bool) string {
	cm, err := a.requireCM()
	if err != nil {
		return err.Error()
	}
	if err := cm.write(netproto.MsgPrioritySpeaker, netproto.PrioritySpeaker{Active: active}); err != nil {
		return err.Error()
	}
	return ""
}

// ChannelEdit edits a channel's settings (gated server-side by
// b_channel_modify). The dialog always sends the full form, so every field
// is set explicitly. It returns "" on success or the failure reason.
func (a *App) ChannelEdit(channelID int64, topic string, maxClients int, opusBitrate int, opusFEC bool, opusDTX bool, opusStereo bool, description string, slowModeSeconds int) string {
	cm, err := a.requireCM()
	if err != nil {
		return err.Error()
	}
	if err := cm.write(netproto.MsgChannelEdit, netproto.ChannelEdit{
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
	return a.sendChat(scope, target, text, 0)
}

// SendChatReply sends a channel/global reply with an explicit parent id.
func (a *App) SendChatReply(scope, target, text string, replyToID int64) string {
	return a.sendChat(scope, target, text, replyToID)
}

func (a *App) sendChat(scope, target, text string, replyToID int64) string {
	if text == "" {
		return "empty message"
	}
	cm, err := a.requireCM()
	if err != nil {
		return err.Error()
	}
	msg, err := cm.encryptChat(scope, target, text)
	if err != nil {
		return "encryption failed: " + err.Error()
	}
	if scope != "direct" {
		msg.ReplyToID = replyToID
	}
	if err := cm.write(netproto.MsgChatSend, msg); err != nil {
		return err.Error()
	}
	return ""
}

// WhisperSet configures the whisper list and mode.
func (a *App) WhisperSet(uniqueIDs []string, channelIDs []int64, active bool) string {
	cm, err := a.requireCM()
	if err != nil {
		return err.Error()
	}
	if err := cm.write(netproto.MsgWhisperSet, netproto.WhisperSet{
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
	cm, err := a.requireCM()
	if err != nil {
		return nil, err
	}
	f, err := cm.request(netproto.MsgPermissionsQuery, netproto.MsgPermissionsResponse,
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
	cm, err := a.requireCM()
	if err != nil {
		return "", err
	}
	f, err := cm.request(netproto.MsgWebRTCOffer, netproto.MsgWebRTCAnswer,
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
	if cm, err := a.requireCM(); err == nil {
		_ = cm.write(netproto.MsgWebRTCAnswer, netproto.WebRTCAnswer{SDP: sdp})
	}
}

// SendICECandidate forwards a browser ICE candidate to the server.
func (a *App) SendICECandidate(candidate, sdpMid string, sdpMLineIndex uint16) {
	cm, err := a.requireCM()
	if err != nil {
		return
	}
	_ = cm.write(netproto.MsgICECandidate, netproto.ICECandidate{
		Candidate:     candidate,
		SDPMid:        sdpMid,
		SDPMLineIndex: sdpMLineIndex,
	})
}

// SetScreenShare declares screen-share state to the channel.
func (a *App) SetScreenShare(active bool) string {
	return a.SetScreenShareQuality(active, 0)
}

// SetScreenShareQuality declares state plus the requested capture height so
// the server can apply the granular 1080p permission.
func (a *App) SetScreenShareQuality(active bool, maxHeight int) string {
	// the voice teardown declares "not sharing" while disconnecting, and a
	// tab switch stores a nil manager: write takes its mutex before looking
	// at the connection, so a nil manager panics rather than erroring.
	m := a.cmLoad()
	if m == nil {
		return "not connected"
	}
	if err := m.write(netproto.MsgScreenShare, netproto.ScreenShare{Active: active, MaxHeight: maxHeight}); err != nil {
		return err.Error()
	}
	return ""
}

// SetVideoQuality requests a simulcast layer for the video this client
// receives: "high", "mid", or "low" (server maps to RID f/h/q).
func (a *App) SetVideoQuality(quality string) string {
	m := a.cmLoad()
	if m == nil {
		return "not connected"
	}
	if err := m.write(netproto.MsgVideoQuality, netproto.VideoQuality{Quality: quality}); err != nil {
		return err.Error()
	}
	return ""
}

// SetMuted records the mute state (drives the status display; the actual
// track toggling happens in the frontend).
func (a *App) SetMuted(muted bool) {
	traySetMuted(muted)
	if cm := a.cmLoad(); cm != nil {
		cm.emit("muted", muted)
	}
}

// SetPTT records the push-to-talk state.
func (a *App) SetPTT(active bool) {
	traySetPTT(active)
	if cm := a.cmLoad(); cm != nil {
		cm.emit("ptt", active)
	}
}

// SetAvatar uploads an avatar image (base64).
func (a *App) SetAvatar(dataBase64 string) string {
	cm, err := a.requireCM()
	if err != nil {
		return err.Error()
	}
	if err := cm.write(netproto.MsgAvatarSet, netproto.AvatarSet{DataBase64: dataBase64}); err != nil {
		return err.Error()
	}
	return ""
}

// SetAlwaysOnTop toggles always-on-top (291) and persists the setting.
func (a *App) SetAlwaysOnTop(on bool) {
	generation, err := a.updateSettings(func(settings Settings) Settings {
		settings.AlwaysOnTop = on
		return settings
	})
	if err != nil {
		log.Printf("saving always-on-top setting failed: %v", err)
		return
	}
	if err := a.applyAlwaysOnTopEffect(generation); err != nil {
		log.Printf("applying always-on-top setting failed: %v", err)
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

// ClientVersionShort returns the canonical semantic build identity.
func (a *App) ClientVersionShort() string {
	return version.Short()
}

// IdentityUID returns the unique ID derived from the client's identity key
// (used by the login screen to show the auto-generated identity).
func (a *App) IdentityUID() string {
	a.identityMu.Lock()
	defer a.identityMu.Unlock()
	id, _, err := a.activeIdentityLocked()
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
	cm, err := a.requireCM()
	if err != nil {
		return netproto.AvatarData{}, err
	}
	f, err := cm.request(netproto.MsgAvatarGet, netproto.MsgAvatarData,
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
	cm, err := a.requireCM()
	if err != nil {
		return netproto.ClientInfoResponse{}, err
	}
	f, err := cm.request(netproto.MsgClientInfoQuery, netproto.MsgClientInfoResponse,
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

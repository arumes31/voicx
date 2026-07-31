// app.go defines the Wails-bound application API. Methods on App are
// callable from the frontend; server state lives in the connManager.
package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"voicx/internal/netproto"
	"voicx/internal/version"
)

// App is the Wails application.
type App struct {
	ctx      context.Context
	cm       *connManager
	settings Settings

	hkMu    sync.Mutex
	hotkeys map[string]*hotkeyReg
}

// NewApp creates a new App.
func NewApp() *App {
	return &App{
		settings: loadSettings(),
		hotkeys:  make(map[string]*hotkeyReg),
	}
}

// startup is called when the app starts.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.cm = newConnManager(ctx)
	a.registerHotkeys()
}

// shutdown is called when the app closes.
func (a *App) shutdown(_ context.Context) {
	if a.cm != nil {
		a.cm.disconnect()
	}
}

// Connect dials the server and authenticates. nickname is the account
// nickname (or unique ID); with an empty password the client logs in as a
// guest using its own generated identity. It returns "" on success or the
// failure reason.
func (a *App) Connect(addr, nickname, password, serverPassword string) string {
	if a.cm == nil {
		return "app not ready"
	}
	if addr == "" || nickname == "" {
		return "server address and nickname are required"
	}
	if err := a.cm.connect(addr, nickname, password, serverPassword); err != "" {
		return err
	}
	return ""
}

// ConnectGuest dials the server and authenticates as a guest using the
// client's own Ed25519 identity (key-derived unique ID). It returns "" on
// success or the failure reason.
func (a *App) ConnectGuest(addr, nickname string) string {
	if a.cm == nil {
		return "app not ready"
	}
	if addr == "" || nickname == "" {
		return "server address and nickname are required"
	}
	if err := a.cm.connect(addr, nickname, "", ""); err != "" {
		return err
	}
	return ""
}

// Disconnect closes the server connection.
func (a *App) Disconnect() {
	if a.cm != nil {
		a.cm.disconnect()
	}
}

// Connected reports whether the client is connected.
func (a *App) Connected() bool {
	return a.cm != nil && a.cm.connected()
}

// JoinChannel joins (moves into) a channel.
func (a *App) JoinChannel(channelID int64) string {
	if err := a.cm.write(netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: channelID}); err != nil {
		return err.Error()
	}
	return ""
}

// SendChat sends a chat message. scope is "global", "channel", or "direct"
// (target = channel ID for "channel", unique ID for "direct").
func (a *App) SendChat(scope, target, text string) string {
	if text == "" {
		return "empty message"
	}
	msg := netproto.ChatSend{Text: text}
	switch scope {
	case "global":
	case "channel":
		msg.ChannelID = target
	case "direct":
		msg.ToUniqueID = target
	default:
		return "unknown scope: " + scope
	}
	if err := a.cm.write(netproto.MsgChatSend, msg); err != nil {
		return err.Error()
	}
	return ""
}

// WhisperSet configures the whisper list and mode.
func (a *App) WhisperSet(uniqueIDs []string, channelIDs []int64, active bool) string {
	if err := a.cm.write(netproto.MsgWhisperSet, netproto.WhisperSet{
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
	f, err := a.cm.request(netproto.MsgPermissionsQuery, netproto.MsgPermissionsResponse,
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
func (a *App) WebRTCOffer(sdp string) (string, error) {
	f, err := a.cm.request(netproto.MsgWebRTCOffer, netproto.MsgWebRTCAnswer,
		netproto.WebRTCOffer{SDP: sdp}, 10*time.Second)
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
	_ = a.cm.write(netproto.MsgWebRTCAnswer, netproto.WebRTCAnswer{SDP: sdp})
}

// SendICECandidate forwards a browser ICE candidate to the server.
func (a *App) SendICECandidate(candidate, sdpMid string, sdpMLineIndex uint16) {
	_ = a.cm.write(netproto.MsgICECandidate, netproto.ICECandidate{
		Candidate:     candidate,
		SDPMid:        sdpMid,
		SDPMLineIndex: sdpMLineIndex,
	})
}

// SetScreenShare declares screen-share state to the channel.
func (a *App) SetScreenShare(active bool) string {
	if err := a.cm.write(netproto.MsgScreenShare, netproto.ScreenShare{Active: active}); err != nil {
		return err.Error()
	}
	return ""
}

// SetMuted records the mute state (drives the status display; the actual
// track toggling happens in the frontend).
func (a *App) SetMuted(muted bool) {
	a.cm.emit("muted", muted)
}

// SetPTT records the push-to-talk state.
func (a *App) SetPTT(active bool) {
	a.cm.emit("ptt", active)
}

// SetAvatar uploads an avatar image (base64).
func (a *App) SetAvatar(dataBase64 string) string {
	if err := a.cm.write(netproto.MsgAvatarSet, netproto.AvatarSet{DataBase64: dataBase64}); err != nil {
		return err.Error()
	}
	return ""
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
	if a.cm == nil {
		return ""
	}
	id, err := a.cm.identity()
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
	f, err := a.cm.request(netproto.MsgAvatarGet, netproto.MsgAvatarData,
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
	f, err := a.cm.request(netproto.MsgClientInfoQuery, netproto.MsgClientInfoResponse,
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

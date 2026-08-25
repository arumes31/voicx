package main

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"voicx/internal/netproto"
	"voicx/internal/tlscert"
)

func nextFrame(t *testing.T, frames <-chan *netproto.Frame, want netproto.MessageType) *netproto.Frame {
	t.Helper()
	select {
	case frame := <-frames:
		if got := netproto.MessageType(frame.Type); got != want {
			t.Fatalf("frame type = %v, want %v", got, want)
		}
		return frame
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %v", want)
		return nil
	}
}

func TestAppOfflineContracts(t *testing.T) {
	oldRoot, oldProtection := identityRootOverride, keyProtectionSetting
	identityRootOverride = t.TempDir()
	keyProtectionSetting = func() string { return "off" }
	t.Cleanup(func() {
		identityRootOverride = oldRoot
		keyProtectionSetting = oldProtection
	})

	constructed := NewApp()
	if constructed.tabs == nil || constructed.hotkeys == nil {
		t.Fatal("NewApp did not initialize runtime registries")
	}

	a := &App{settingsPath: filepath.Join(t.TempDir(), "settings.json")}
	if a.Connected() {
		t.Fatal("zero App reports connected")
	}
	if got := a.ServerFingerprint(); got != "" {
		t.Fatalf("ServerFingerprint = %q", got)
	}
	if got := a.CertificateClockWarning(); got != "" {
		t.Fatalf("CertificateClockWarning = %q", got)
	}
	if got := a.ConnectionSecurity(); got != "offline" {
		t.Fatalf("ConnectionSecurity = %q", got)
	}
	if got := a.MOTD(); got != "" {
		t.Fatalf("MOTD = %q", got)
	}
	if got := a.ClientID(); got != "" {
		t.Fatalf("ClientID = %q", got)
	}
	if got := a.IdentityUID(); got == "" {
		t.Fatal("offline IdentityUID did not generate the active identity")
	}
	if got := a.TrustServerFingerprint("addr", "fp"); got != "trust store unavailable" {
		t.Fatalf("TrustServerFingerprint offline = %q", got)
	}
	if got := a.AcceptServerRules(""); got != "rules hash is required" {
		t.Fatalf("AcceptServerRules empty = %q", got)
	}
	if got := a.AcceptServerRules("hash"); got != "not connected" {
		t.Fatalf("AcceptServerRules offline = %q", got)
	}
	if got := a.SetScreenShareQuality(true, 1080); got != "not connected" {
		t.Fatalf("SetScreenShareQuality offline = %q", got)
	}
	if got := a.SetVideoQuality("high"); got != "not connected" {
		t.Fatalf("SetVideoQuality offline = %q", got)
	}
	// Global mute/PTT hotkeys remain available while offline; they must update
	// tray state without dereferencing a missing active connection.
	a.SetMuted(true)
	a.SetPTT(true)
	if got := a.SendChat("global", "", ""); got != "empty message" {
		t.Fatalf("SendChat empty = %q", got)
	}
	if got := a.Greet("Ada"); got != "Hello Ada, welcome to voicx!" {
		t.Fatalf("Greet = %q", got)
	}
	for name, value := range map[string]string{
		"full":  a.ClientVersion(),
		"short": a.ClientVersionShort(),
	} {
		if value == "" || strings.Contains(value, "0.0.0") {
			t.Fatalf("%s client version = %q", name, value)
		}
	}

	a.SetAlwaysOnTop(true)
	if !a.settings.AlwaysOnTop {
		t.Fatal("SetAlwaysOnTop did not update settings")
	}
}

func TestCertificateClockWarning(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name             string
		currentTime      time.Time
		notBefore        time.Time
		notAfter         time.Time
		expectedFragment string
	}{
		{
			name:      "missing current time",
			notBefore: now.Add(-time.Hour),
			notAfter:  now.Add(time.Hour),
		},
		{
			name: "missing certificate window",
		},
		{
			name:        "currently valid",
			currentTime: now,
			notBefore:   now.Add(-time.Hour),
			notAfter:    now.Add(time.Hour),
		},
		{
			name:        "early skew at tolerance is accepted",
			currentTime: now,
			notBefore:   now.Add(certificateClockSkewTolerance),
			notAfter:    now.Add(time.Hour),
		},
		{
			name:             "early skew beyond tolerance warns",
			currentTime:      now,
			notBefore:        now.Add(certificateClockSkewTolerance + time.Nanosecond),
			notAfter:         now.Add(time.Hour),
			expectedFragment: "The local system clock may be inaccurate",
		},
		{
			name:        "late skew at tolerance is accepted",
			currentTime: now,
			notBefore:   now.Add(-time.Hour),
			notAfter:    now.Add(-certificateClockSkewTolerance),
		},
		{
			name:             "late skew beyond tolerance warns",
			currentTime:      now,
			notBefore:        now.Add(-time.Hour),
			notAfter:         now.Add(-certificateClockSkewTolerance - time.Nanosecond),
			expectedFragment: "server certificate may be expired",
		},
		{
			name:             "invalid validity window",
			currentTime:      now,
			notBefore:        now.Add(time.Hour),
			notAfter:         now.Add(-time.Hour),
			expectedFragment: "invalid validity window",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := certificateClockWarning(tt.currentTime, tt.notBefore, tt.notAfter)
			if tt.expectedFragment == "" && got != "" {
				t.Fatalf("certificateClockWarning() = %q, want no warning", got)
			}
			if tt.expectedFragment != "" && !strings.Contains(got, tt.expectedFragment) {
				t.Fatalf("certificateClockWarning() = %q, want fragment %q", got, tt.expectedFragment)
			}
		})
	}
}

func TestCertificateClockWarningRequiresPinnedCertificate(t *testing.T) {
	t.Parallel()

	now := time.Now()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	cm := &connManager{
		conn:          clientConn,
		tlsUsed:       true,
		fingerprint:   "AA:BB",
		certNotBefore: now.Add(time.Hour),
		certNotAfter:  now.Add(2 * time.Hour),
	}
	app := &App{}
	app.cmStore(cm)
	if got := app.CertificateClockWarning(); got != "" {
		t.Fatalf("unverified certificate produced clock warning %q", got)
	}

	cm.mu.Lock()
	cm.certValidityTrusted = true
	cm.mu.Unlock()
	if got := app.CertificateClockWarning(); !strings.Contains(got, "The local system clock may be inaccurate") {
		t.Fatalf("pinned certificate warning = %q", got)
	}
}

func TestAppConnectedBindings(t *testing.T) {
	frames := make(chan *netproto.Frame, 32)
	app, cm := newPipedApp(t, func(frame *netproto.Frame) (netproto.MessageType, any, bool) {
		frames <- frame
		switch netproto.MessageType(frame.Type) {
		case netproto.MsgPermissionsQuery:
			return netproto.MsgPermissionsResponse, netproto.PermissionsResponse{
				Entries: []netproto.PermissionEntry{{Key: "i_client_talk_power", Value: 42}},
			}, true
		case netproto.MsgWebRTCOffer:
			return netproto.MsgWebRTCAnswer, netproto.WebRTCAnswer{SDP: "answer-sdp"}, true
		case netproto.MsgAvatarGet:
			return netproto.MsgAvatarData, netproto.AvatarData{
				UniqueID: "alice", DataBase64: "cG5n", ContentType: "image/png",
			}, true
		case netproto.MsgClientInfoQuery:
			return netproto.MsgClientInfoResponse, netproto.ClientInfoResponse{
				ClientID: "c1", UniqueID: "u1", Nickname: "Alice", ChannelID: 7,
			}, true
		default:
			return 0, nil, false
		}
	})

	cm.mu.Lock()
	cm.addr = "127.0.0.1:12333"
	cm.clientID = "self"
	cm.iceServers = []netproto.ICEServer{{URLs: []string{"stun:example.test"}}}
	cm.motd = "Welcome"
	cm.tlsUsed = true
	cm.fingerprint = "AA:BB"
	cm.newServer = true
	cm.knownServers = loadKnownServersAt(filepath.Join(t.TempDir(), "known_servers.json"))
	cm.mu.Unlock()
	cm.scopeKeys.put(0, 1, [32]byte{1})
	cm.scopeKeys.put(7, 2, [32]byte{2})

	if !app.Connected() {
		t.Fatal("connected App reports offline")
	}
	if got := app.ServerFingerprint(); got != "AA:BB" {
		t.Fatalf("ServerFingerprint = %q", got)
	}
	if got := app.ConnectionSecurity(); got != "TLS (new server fingerprint pinned: AA:BB)" {
		t.Fatalf("ConnectionSecurity = %q", got)
	}
	if got := app.MOTD(); got != "Welcome" {
		t.Fatalf("MOTD = %q", got)
	}
	if got := app.ClientID(); got != "self" {
		t.Fatalf("ClientID = %q", got)
	}
	if got := app.IdentityUID(); got == "" {
		t.Fatal("IdentityUID is empty")
	}
	ice := app.GetICEServers()
	if len(ice) != 1 || len(ice[0].URLs) != 1 || ice[0].URLs[0] != "stun:example.test" {
		t.Fatalf("GetICEServers = %+v", ice)
	}
	if got := app.TrustServerFingerprint("", "AA:BB"); got != "address and fingerprint are required" {
		t.Fatalf("TrustServerFingerprint empty = %q", got)
	}
	replacementFingerprint := tlscert.FingerprintDER([]byte("replacement certificate"))
	if got := app.TrustServerFingerprint("server.test:12333", strings.ToUpper(replacementFingerprint)); got != "" {
		t.Fatalf("TrustServerFingerprint = %q", got)
	}
	if got, err := cm.knownServers.verify("server.test:12333", replacementFingerprint); err != nil || got != trustOK {
		t.Fatalf("trusted fingerprint status = %v", got)
	}
	if got := app.TrustServerFingerprint("server.test:12333", "CC:DD"); !strings.HasPrefix(got, "invalid fingerprint:") {
		t.Fatalf("TrustServerFingerprint invalid = %q", got)
	}

	if got := app.JoinChannel(7); got != "" {
		t.Fatalf("JoinChannel = %q", got)
	}
	var join netproto.JoinChannel
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgJoinChannel), &join); err != nil || join.ChannelID != 7 {
		t.Fatalf("JoinChannel payload = %+v, %v", join, err)
	}

	if got := app.MoveClient("c2", 8); got != "" {
		t.Fatalf("MoveClient = %q", got)
	}
	var move netproto.MoveClient
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgMoveClient), &move); err != nil || move.ClientID != "c2" || move.ChannelID != 8 {
		t.Fatalf("MoveClient payload = %+v, %v", move, err)
	}

	if got := app.AcceptServerRules("rules-v2"); got != "" {
		t.Fatalf("AcceptServerRules = %q", got)
	}
	var accept netproto.ServerRulesAccept
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgServerRulesAccept), &accept); err != nil || accept.Hash != "rules-v2" {
		t.Fatalf("AcceptServerRules payload = %+v, %v", accept, err)
	}

	if got := app.SetPrioritySpeaker(true); got != "" {
		t.Fatalf("SetPrioritySpeaker = %q", got)
	}
	var priority netproto.PrioritySpeaker
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgPrioritySpeaker), &priority); err != nil || !priority.Active {
		t.Fatalf("SetPrioritySpeaker payload = %+v, %v", priority, err)
	}

	if got := app.ChannelEdit(7, "topic", 25, 64_000, true, false, true, "description", 3); got != "" {
		t.Fatalf("ChannelEdit = %q", got)
	}
	var edit netproto.ChannelEdit
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgChannelEdit), &edit); err != nil {
		t.Fatalf("decode ChannelEdit: %v", err)
	}
	if edit.ChannelID != 7 || edit.Topic == nil || *edit.Topic != "topic" || edit.MaxClients == nil || *edit.MaxClients != 25 ||
		edit.OpusBitrate == nil || *edit.OpusBitrate != 64_000 || edit.OpusFEC == nil || !*edit.OpusFEC ||
		edit.OpusDTX == nil || *edit.OpusDTX || edit.OpusStereo == nil || !*edit.OpusStereo ||
		edit.Description == nil || *edit.Description != "description" || edit.SlowModeSeconds == nil || *edit.SlowModeSeconds != 3 {
		t.Fatalf("ChannelEdit payload = %+v", edit)
	}

	if got := app.SendChatReply("channel", "7", "reply", 99); got != "" {
		t.Fatalf("SendChatReply = %q", got)
	}
	var chat netproto.ChatSend
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgChatSend), &chat); err != nil || !chat.Enc || chat.ChannelID != "7" || chat.ReplyToID != 99 {
		t.Fatalf("SendChatReply payload = %+v, %v", chat, err)
	}

	if got := app.WhisperSet([]string{"u1"}, []int64{7, 8}, true); got != "" {
		t.Fatalf("WhisperSet = %q", got)
	}
	var whisper netproto.WhisperSet
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgWhisperSet), &whisper); err != nil ||
		!whisper.Active || len(whisper.UniqueIDs) != 1 || whisper.UniqueIDs[0] != "u1" || len(whisper.ChannelIDs) != 2 {
		t.Fatalf("WhisperSet payload = %+v, %v", whisper, err)
	}

	permissions, err := app.GetPermissions()
	if err != nil || len(permissions) != 1 || permissions[0].Key != "i_client_talk_power" || permissions[0].Value != 42 {
		t.Fatalf("GetPermissions = %+v, %v", permissions, err)
	}
	nextFrame(t, frames, netproto.MsgPermissionsQuery)

	tracks := []netproto.TrackSlot{{TrackID: "mic-track", Slot: "mic"}}
	answer, err := app.WebRTCOffer("offer-sdp", tracks)
	if err != nil || answer != "answer-sdp" {
		t.Fatalf("WebRTCOffer = %q, %v", answer, err)
	}
	var offer netproto.WebRTCOffer
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgWebRTCOffer), &offer); err != nil || offer.SDP != "offer-sdp" || len(offer.Tracks) != 1 {
		t.Fatalf("WebRTCOffer payload = %+v, %v", offer, err)
	}

	app.WebRTCAnswer("client-answer")
	var webRTCAnswer netproto.WebRTCAnswer
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgWebRTCAnswer), &webRTCAnswer); err != nil || webRTCAnswer.SDP != "client-answer" {
		t.Fatalf("WebRTCAnswer payload = %+v, %v", webRTCAnswer, err)
	}

	app.SendICECandidate("candidate", "audio", 4)
	var candidate netproto.ICECandidate
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgICECandidate), &candidate); err != nil ||
		candidate.Candidate != "candidate" || candidate.SDPMid != "audio" || candidate.SDPMLineIndex != 4 {
		t.Fatalf("SendICECandidate payload = %+v, %v", candidate, err)
	}

	if got := app.SetScreenShare(true); got != "" {
		t.Fatalf("SetScreenShare = %q", got)
	}
	var screen netproto.ScreenShare
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgScreenShare), &screen); err != nil || !screen.Active || screen.MaxHeight != 0 {
		t.Fatalf("SetScreenShare payload = %+v, %v", screen, err)
	}
	if got := app.SetScreenShareQuality(true, 1080); got != "" {
		t.Fatalf("SetScreenShareQuality = %q", got)
	}
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgScreenShare), &screen); err != nil || !screen.Active || screen.MaxHeight != 1080 {
		t.Fatalf("SetScreenShareQuality payload = %+v, %v", screen, err)
	}

	if got := app.SetVideoQuality("low"); got != "" {
		t.Fatalf("SetVideoQuality = %q", got)
	}
	var quality netproto.VideoQuality
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgVideoQuality), &quality); err != nil || quality.Quality != "low" {
		t.Fatalf("SetVideoQuality payload = %+v, %v", quality, err)
	}

	if got := app.SetAvatar("cG5n"); got != "" {
		t.Fatalf("SetAvatar = %q", got)
	}
	var avatarSet netproto.AvatarSet
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgAvatarSet), &avatarSet); err != nil || avatarSet.DataBase64 != "cG5n" {
		t.Fatalf("SetAvatar payload = %+v, %v", avatarSet, err)
	}

	avatar, err := app.GetAvatar("alice")
	if err != nil || avatar.UniqueID != "alice" || avatar.ContentType != "image/png" {
		t.Fatalf("GetAvatar = %+v, %v", avatar, err)
	}
	var avatarGet netproto.AvatarGet
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgAvatarGet), &avatarGet); err != nil || avatarGet.UniqueID != "alice" {
		t.Fatalf("GetAvatar payload = %+v, %v", avatarGet, err)
	}

	info, err := app.GetClientInfo("c1")
	if err != nil || info.ClientID != "c1" || info.Nickname != "Alice" || info.ChannelID != 7 {
		t.Fatalf("GetClientInfo = %+v, %v", info, err)
	}
	var infoQuery netproto.ClientInfoQuery
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgClientInfoQuery), &infoQuery); err != nil || infoQuery.ClientID != "c1" {
		t.Fatalf("GetClientInfo payload = %+v, %v", infoQuery, err)
	}

	cm.mu.Lock()
	cm.tlsUsed = false
	cm.fingerprint = ""
	cm.newServer = false
	cm.mu.Unlock()
	if got := app.ConnectionSecurity(); !strings.HasPrefix(got, "PLAINTEXT") {
		t.Fatalf("plaintext ConnectionSecurity = %q", got)
	}
}

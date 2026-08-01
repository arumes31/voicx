// voice.go implements the voice-pipeline handlers of the TCP control server:
// WebRTC signaling (SDP offer/answer, trickle ICE), whisper configuration,
// and 3D position relays. All handlers require authentication (enforced by
// dispatch). The heavy lifting lives behind the VoiceBackend interface, so
// this file contains no Pion types.
package server

import (
	"context"

	"go.uber.org/zap"

	"voicx/internal/netproto"
	"voicx/internal/permissions"
)

// Broadcast event types for the voice pipeline.
const (
	eventSpeakingChanged        = "speaking_changed"
	eventPosition               = "position"
	eventPrioritySpeakerChanged = "priority_speaker_changed"
	// eventWhisper is delivered ONLY to the targets of an active whisper. It
	// cannot ride the broadcast speaking event: that one goes to everybody, so
	// it can say THAT someone whispers but never that they whispered to YOU —
	// which is exactly what the receiving client needs to sound the whisper
	// cue, flash the taskbar (32) and remember who to whisper back to (33).
	eventWhisper = "whisper"
)

// whisperTargeter is the optional VoiceBackend capability that reports which
// clients a speaker's active whisper currently reaches. webrtc.Voice
// implements it; a backend that does not simply produces no whisper events.
type whisperTargeter interface {
	WhisperTargets(clientID string) []string
}

// whisperEvent is the payload of whisper events, emitted to every target of an
// active whisper when the whisperer's VAD state toggles. FromUniqueID is the
// handle the whisper-reply hotkey whispers back to (33); Speaking mirrors the
// speaking event so the receiver can clear the indicator again.
type whisperEvent struct {
	FromClientID string `json:"from_client_id"`
	FromUniqueID string `json:"from_unique_id"`
	FromNickname string `json:"from_nickname"`
	Speaking     bool   `json:"speaking"`
}

// prioritySpeakerEvent is the payload of priority_speaker_changed events,
// emitted when a client toggles its priority-speaker flag.
type prioritySpeakerEvent struct {
	ClientID  string `json:"client_id"`
	ChannelID int64  `json:"channel_id"`
	Active    bool   `json:"active"`
}

// speakingEvent is the payload of speaking_changed events, emitted when a
// client's VAD speaking state toggles.
type speakingEvent struct {
	ClientID string `json:"client_id"`
	Speaking bool   `json:"speaking"`
	// Whisper marks the transmission as a whisper (32). It names no targets —
	// this event is broadcast, and who is being whispered to is only disclosed
	// to those targets, via eventWhisper.
	Whisper bool `json:"whisper,omitempty"`
}

// positionEvent is the payload of position events, relaying a client's 3D
// position to the other members of its channel.
type positionEvent struct {
	ClientID string  `json:"client_id"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	Z        float64 `json:"z"`
}

// handleWebRTCOffer establishes a WebRTC session for the client: it forwards
// the SDP offer to the voice backend and replies with the SDP answer. Local
// ICE candidates are pushed to the client asynchronously as ICECandidate
// messages.
func (s *TCPServer) handleWebRTCOffer(_ context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.WebRTCOffer
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed webrtc_offer: "+err.Error())
	}
	if s.deps == nil || s.deps.Voice == nil {
		return s.sendError(client, errCodeUnavailable, "voice backend unavailable")
	}

	answer, err := s.deps.Voice.HandleOffer(client.ID, msg.SDP,
		func(candidate, sdpMid string, mlineIndex uint16) {
			// Push server-side candidates to the client as they are gathered.
			// Write errors (e.g. disconnect) are logged by writeMessage.
			_ = s.writeMessage(client, netproto.MsgICECandidate, netproto.ICECandidate{
				Candidate:     candidate,
				SDPMid:        sdpMid,
				SDPMLineIndex: mlineIndex,
			})
		})
	if err != nil {
		s.logger.Warn("webrtc offer failed",
			zap.String("client_id", client.ID),
			zap.Error(err),
		)
		return s.sendError(client, errCodeUnavailable, "webrtc offer failed")
	}
	s.metricsSink().SetWebRTCPeers(s.deps.Voice.PeerCount())
	return s.writeMessage(client, netproto.MsgWebRTCAnswer, netproto.WebRTCAnswer{SDP: answer})
}

// handleWebRTCAnswer applies an SDP answer from the client (used when the
// server initiated renegotiation).
func (s *TCPServer) handleWebRTCAnswer(_ context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.WebRTCAnswer
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed webrtc_answer: "+err.Error())
	}
	if s.deps == nil || s.deps.Voice == nil {
		return s.sendError(client, errCodeUnavailable, "voice backend unavailable")
	}
	if err := s.deps.Voice.HandleAnswer(client.ID, msg.SDP); err != nil {
		return s.sendError(client, errCodeNotFound, "webrtc answer failed: no active session")
	}
	return nil
}

// handleICECandidate applies a trickle ICE candidate from the client.
func (s *TCPServer) handleICECandidate(_ context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ICECandidate
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed ice_candidate: "+err.Error())
	}
	if s.deps == nil || s.deps.Voice == nil {
		return s.sendError(client, errCodeUnavailable, "voice backend unavailable")
	}
	if err := s.deps.Voice.AddICECandidate(client.ID, msg.Candidate, msg.SDPMid, msg.SDPMLineIndex); err != nil {
		s.logger.Debug("ice candidate rejected",
			zap.String("client_id", client.ID),
			zap.Error(err),
		)
	}
	return nil
}

// handleWhisperSet configures the client's whisper list and mode. Target
// unique IDs are resolved to online connections at set time; offline targets
// are dropped. Gated by i_client_whisper_power (unset = allowed, negated =
// denied).
func (s *TCPServer) handleWhisperSet(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.WhisperSet
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed whisper_set: "+err.Error())
	}
	if s.deps == nil || s.deps.Voice == nil {
		return s.sendError(client, errCodeUnavailable, "voice backend unavailable")
	}

	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.whisperAllowed() {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyClientWhisperPower))
	}

	clientIDs := make([]string, 0, len(msg.UniqueIDs))
	for _, uid := range msg.UniqueIDs {
		if tc, ok := s.clientByUniqueID(uid); ok {
			clientIDs = append(clientIDs, tc.ID)
		}
	}
	s.deps.Voice.SetWhisper(client.ID, clientIDs, msg.ChannelIDs, msg.Active)

	s.logger.Debug("whisper configured",
		zap.String("client_id", client.ID),
		zap.Bool("active", msg.Active),
		zap.Int("targets", len(clientIDs)),
		zap.Int("channels", len(msg.ChannelIDs)),
	)
	return nil
}

// handlePositionUpdate relays the client's 3D position to the other members
// of its channel. Positions are low-rate metadata, so they travel over the
// control channel (broadcaster events) rather than a WebRTC DataChannel.
func (s *TCPServer) handlePositionUpdate(_ context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.PositionUpdate
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed position_update: "+err.Error())
	}
	if s.deps == nil || s.deps.State == nil || s.deps.Broadcast == nil {
		return s.sendError(client, errCodeUnavailable, "state backend unavailable")
	}

	sc, ok := s.deps.State.GetClient(client.ID)
	if !ok || sc.ChannelID == 0 {
		return s.sendError(client, errCodeNotFound, "not in a channel")
	}

	payload, err := eventEnvelope(eventPosition, positionEvent{
		ClientID: client.ID,
		X:        msg.X,
		Y:        msg.Y,
		Z:        msg.Z,
	})
	if err != nil {
		return err
	}
	s.deps.Broadcast.BroadcastToChannel(sc.ChannelID, payload)
	return nil
}

// handlePrioritySpeaker toggles the calling client's priority-speaker flag
// (TS3-style channel commander). Gated by b_client_priority_speaker (deny on
// unset; server admins bypass). The flag is broadcast so subscribers can duck
// other publishers while a priority speaker talks.
func (s *TCPServer) handlePrioritySpeaker(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.PrioritySpeaker
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed priority_speaker: "+err.Error())
	}
	if s.deps == nil || s.deps.State == nil {
		return s.sendError(client, errCodeUnavailable, "state backend unavailable")
	}

	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.granted(permissions.PermissionKeyClientPrioritySpeaker) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyClientPrioritySpeaker))
	}

	s.deps.State.SetPrioritySpeaker(client.ID, msg.Active)
	var channelID int64
	if sc, ok := s.deps.State.GetClient(client.ID); ok {
		channelID = sc.ChannelID
	}
	s.broadcastEvent(eventPrioritySpeakerChanged, prioritySpeakerEvent{
		ClientID:  client.ID,
		ChannelID: channelID,
		Active:    msg.Active,
	})
	s.logger.Debug("priority speaker toggled",
		zap.String("client_id", client.ID),
		zap.Bool("active", msg.Active),
	)
	return nil
}

// handleVideoQuality sets the client's preferred simulcast layer for the
// video it receives.
func (s *TCPServer) handleVideoQuality(_ context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.VideoQuality
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed video_quality: "+err.Error())
	}
	if s.deps == nil || s.deps.Voice == nil {
		return s.sendError(client, errCodeUnavailable, "voice backend unavailable")
	}
	if err := s.deps.Voice.SetVideoQuality(client.ID, msg.Quality); err != nil {
		return s.sendError(client, errCodeMalformed, err.Error())
	}
	return nil
}

// handleRecordingControl starts or stops a server-side recording of a
// channel. Gated by b_virtualserver_recording (or server admin).
func (s *TCPServer) handleRecordingControl(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.RecordingControl
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed recording_control: "+err.Error())
	}
	if s.deps == nil || s.deps.Recorder == nil || s.deps.Voice == nil {
		return s.sendError(client, errCodeUnavailable, "recording backend unavailable")
	}

	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.admin && !pc.granted(permissions.PermissionKeyVirtualserverRecording) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyVirtualserverRecording))
	}

	switch msg.Action {
	case "start":
		session, err := s.deps.Recorder.Start(ctx, msg.ChannelID, s.deps.Voice)
		if err != nil {
			return s.sendError(client, errCodeUnavailable, "recording start failed: "+err.Error())
		}
		s.logger.Info("recording started",
			zap.String("client_id", client.ID),
			zap.Int64("channel_id", msg.ChannelID),
			zap.String("output", session.FilePath),
		)
	case "stop":
		if err := s.deps.Recorder.Stop(msg.ChannelID); err != nil {
			return s.sendError(client, errCodeNotFound, "recording stop failed: "+err.Error())
		}
		s.logger.Info("recording stopped",
			zap.String("client_id", client.ID),
			zap.Int64("channel_id", msg.ChannelID),
		)
	default:
		return s.sendError(client, errCodeMalformed, "unknown recording action: "+msg.Action)
	}
	return nil
}

// --- voice callbacks (installed on the voice backend at server construction) -

// canTalk is the router's talk-permission gate: it resolves the client's
// permissions in their current channel context and evaluates
// i_client_talk_power.
func (s *TCPServer) canTalk(clientID string) bool {
	client, ok := s.clientByID(clientID)
	if !ok {
		return false
	}
	pc, err := s.permCheckerFor(context.Background(), client)
	if err != nil {
		return false
	}
	return pc.talkAllowed()
}

// canPublishVideo is the router's video-publish gate: it evaluates
// b_client_video_publish in the client's current channel context.
func (s *TCPServer) canPublishVideo(clientID string) bool {
	client, ok := s.clientByID(clientID)
	if !ok {
		return false
	}
	pc, err := s.permCheckerFor(context.Background(), client)
	if err != nil {
		return false
	}
	return pc.videoPublishAllowed()
}

// onSpeakingChanged is the router's speaking-state callback: it updates the
// state manager and announces the transition to all clients. The event
// carries no channel ID; clients already track membership via snapshots and
// user_moved events.
//
// When the speaker is whispering, each target additionally gets a directed
// whisper event naming the whisperer (32/33).
func (s *TCPServer) onSpeakingChanged(clientID string, speaking bool) {
	if s.deps == nil || s.deps.State == nil {
		return
	}
	s.deps.State.SetSpeaking(clientID, speaking)

	var targets []string
	if wt, ok := s.deps.Voice.(whisperTargeter); ok {
		targets = wt.WhisperTargets(clientID)
	}
	s.broadcastEvent(eventSpeakingChanged, speakingEvent{
		ClientID: clientID,
		Speaking: speaking,
		Whisper:  len(targets) > 0,
	})
	if len(targets) == 0 || s.deps.Broadcast == nil {
		return
	}

	ev := whisperEvent{FromClientID: clientID, Speaking: speaking}
	if c, ok := s.clientByID(clientID); ok {
		ev.FromUniqueID = c.uniqueID()
		ev.FromNickname = c.Username
	}
	payload, err := eventEnvelope(eventWhisper, ev)
	if err != nil {
		s.logger.Error("encoding whisper event failed", zap.Error(err))
		return
	}
	for _, target := range targets {
		if target == clientID {
			continue
		}
		if err := s.deps.Broadcast.BroadcastToClient(target, payload); err != nil {
			s.logger.Debug("whisper event undeliverable",
				zap.String("client_id", target),
				zap.Error(err),
			)
		}
	}
}

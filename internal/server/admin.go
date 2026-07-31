// admin.go holds the trusted (permission-check-free) administrative
// operations shared by the TCP control handlers (after their permission
// gates) and the ServerQuery interface. Callers of the exported methods are
// trusted: they must have performed their own authorization.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"voicx/internal/netproto"
)

// MoveClient moves a client into a channel without a permission check. It
// reuses the standard move path (state, temp-channel bookkeeping, voice
// router sync, user_moved broadcast).
func (s *TCPServer) MoveClient(clientID string, channelID int64) error {
	if s.deps == nil || s.deps.State == nil {
		return errors.New("state backend unavailable")
	}
	if _, ok := s.deps.State.GetChannel(channelID); !ok {
		return errors.New("channel not found")
	}
	if _, ok := s.clientByID(clientID); !ok {
		return errors.New("target client not found")
	}
	return s.moveClient(clientID, channelID)
}

// KickClient kicks a client from its channel or the server without a
// permission check. byClientID identifies the initiator in the kicked event
// (e.g. "serverquery").
func (s *TCPServer) KickClient(byClientID, targetID string, fromServer bool, reason string) error {
	return s.performKick(byClientID, targetID, fromServer, false, reason)
}

// performKick is the shared kick implementation used by the control handler
// (after its permission gate) and KickClient.
func (s *TCPServer) performKick(byClientID, targetID string, fromServer, ban bool, reason string) error {
	if s.deps == nil || s.deps.State == nil {
		return errors.New("state backend unavailable")
	}
	target, ok := s.clientByID(targetID)
	if !ok || !target.isAuthed() {
		return errors.New("target client not found")
	}

	evt := kickEvent{
		ClientID:   target.ID,
		ByClientID: byClientID,
		Reason:     reason,
		FromServer: fromServer,
		Ban:        ban,
	}

	if fromServer {
		// Announce the kick and close the connection; the target's own
		// handleConn then performs the disconnect cleanup (state removal,
		// unregister, user_left broadcast, temp-channel check).
		s.broadcastEvent(eventKicked, evt)
		_ = target.Conn.Close()
		return nil
	}

	// Channel kick: move the target out of its channel.
	sc, ok := s.deps.State.GetClient(target.ID)
	if !ok || sc.ChannelID == 0 {
		return errors.New("target is not in a channel")
	}
	channelID := sc.ChannelID
	if err := s.deps.State.LeaveChannel(target.ID); err != nil {
		return err
	}
	if s.deps.Channels != nil {
		s.deps.Channels.OnClientLeftChannel(channelID)
	}
	if s.deps.Voice != nil {
		s.deps.Voice.LeaveChannel(target.ID, channelID)
	}
	s.broadcastEvent(eventKicked, evt)
	return nil
}

// BanClient records a unique-ID ban for the target (permanent when seconds <=
// 0) and kicks it from the server. byClientID identifies the initiator in the
// ban record and the kicked event.
func (s *TCPServer) BanClient(ctx context.Context, byClientID, targetID string, seconds int64, reason string) error {
	target, ok := s.clientByID(targetID)
	if !ok || !target.isAuthed() {
		return errors.New("target client not found")
	}

	var expiresAt any
	if seconds > 0 {
		expiresAt = time.Now().Add(time.Duration(seconds) * time.Second)
	}
	var bannedBy any
	if caller, ok := s.clientByID(byClientID); ok && caller.UserID != 0 {
		bannedBy = caller.UserID
	}
	if err := s.insertBan(ctx, target.UniqueID, reason, bannedBy, expiresAt); err != nil {
		return fmt.Errorf("recording ban: %w", err)
	}

	return s.performKick(byClientID, targetID, true, true, reason)
}

// SendServerText injects a chat message as the server (e.g. from
// ServerQuery). targetMode: 1 = direct to client (target is a client ID),
// 2 = channel (target is a channel ID), 3 = global.
func (s *TCPServer) SendServerText(targetMode int, target, msg string) error {
	if s.deps == nil || s.deps.Broadcast == nil {
		return errors.New("broadcast backend unavailable")
	}

	chat := netproto.ChatBroadcast{From: "ServerQuery", Text: msg}
	switch targetMode {
	case 1:
		payload, err := eventEnvelope(eventChat, chat)
		if err != nil {
			return err
		}
		return s.deps.Broadcast.BroadcastToClient(target, payload)
	case 2:
		channelID, err := strconv.ParseInt(target, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid channel id %q", target)
		}
		chat.ChannelID = target
		payload, err := eventEnvelope(eventChat, chat)
		if err != nil {
			return err
		}
		s.deps.Broadcast.BroadcastToChannel(channelID, payload)
		return nil
	case 3:
		raw, err := json.Marshal(chat)
		if err != nil {
			return err
		}
		s.deps.Broadcast.BroadcastEvent(eventChat, raw)
		return nil
	default:
		return fmt.Errorf("invalid targetmode %d", targetMode)
	}
}

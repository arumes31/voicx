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
	"voicx/internal/permissions"
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
	target, ok := s.clientByID(targetID)
	if !ok || !target.isAuthed() {
		return errors.New("target client not found")
	}
	uniqueID := target.UniqueID
	if err := s.performKick(byClientID, targetID, fromServer, false, reason); err != nil {
		return err
	}
	s.audit(context.Background(), byClientID, "kick", uniqueID,
		fmt.Sprintf("from_server=%t reason=%s", fromServer, reason))
	return nil
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

	if err := s.performKick(byClientID, targetID, true, true, reason); err != nil {
		return err
	}
	s.audit(ctx, byClientID, "ban", target.UniqueID,
		fmt.Sprintf("seconds=%d reason=%s", seconds, reason))
	return nil
}

// SendServerText injects a chat message as the server (e.g. from
// ServerQuery). targetMode: 1 = direct to client (target is a client ID),
// 2 = channel (target is a channel ID), 3 = global.
//
// The text is SEALED like any other chat body (91): the channel generation
// for mode 2, the global generation for modes 1 and 3. Mode 1 is therefore a
// server notice delivered to one client, NOT an E2EE direct message — the
// server has no DM key and never did, so any holder of the global generation
// who intercepts the frame can read it. The UI labels it as a notice.
func (s *TCPServer) SendServerText(targetMode int, target, msg string) error {
	if s.deps == nil || s.deps.Broadcast == nil {
		return errors.New("broadcast backend unavailable")
	}
	if s.chatKeys == nil || !s.chatKeys.configured() {
		return errChatKeysUnconfigured
	}

	if targetMode < 1 || targetMode > 3 {
		return fmt.Errorf("invalid targetmode %d", targetMode)
	}
	// ServerQuery has no request context; the seal is a local key lookup plus
	// at most one row read.
	ctx := context.Background()
	scope := globalChatScope
	var channelID int64
	if targetMode == 2 {
		id, err := strconv.ParseInt(target, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid channel id %q", target)
		}
		if s.deps.State != nil {
			if _, ok := s.deps.State.GetChannel(id); !ok {
				return fmt.Errorf("channel %d does not exist", id)
			}
		}
		if _, _, err := s.chatKeys.EnsureScope(ctx, id); err != nil {
			return fmt.Errorf("ensuring scope %d: %w", id, err)
		}
		channelID, scope = id, id
	}
	keyID, sealed, err := s.chatKeys.seal(ctx, scope, msg)
	if err != nil {
		return fmt.Errorf("sealing server text for scope %d: %w", scope, err)
	}

	chat := netproto.ChatBroadcast{From: "ServerQuery", Text: sealed, Enc: true, KeyID: keyID}
	switch targetMode {
	case 1:
		payload, err := eventEnvelope(eventChat, chat)
		if err != nil {
			return err
		}
		return s.deps.Broadcast.BroadcastToClient(target, payload)
	case 2:
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

// --- ban administration (wave 6b) --------------------------------------------

// banAdminAllowed gates ban list/removal: admins, holders of b_client_ban,
// or anyone with ban power >= 1.
func (s *TCPServer) banAdminAllowed(ctx context.Context, client *Client) (*permChecker, bool) {
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return nil, false
	}
	if pc.granted(permissions.PermissionKeyClientBan) ||
		pc.powerAtLeast(permissions.PermissionKeyClientBanPower, 1) {
		return pc, true
	}
	return pc, false
}

// handleBanList returns the ban list, newest first.
func (s *TCPServer) handleBanList(ctx context.Context, client *Client, f *netproto.Frame) error {
	if err := netproto.Decode(f, &netproto.BanList{}); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed ban_list: "+err.Error())
	}
	if s.deps == nil || s.deps.BanAdmin == nil {
		return s.sendError(client, errCodeUnavailable, "ban store unavailable")
	}
	if _, ok := s.banAdminAllowed(ctx, client); !ok {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyClientBan))
	}
	bans, err := s.deps.BanAdmin.ListBans(ctx)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "ban list failed")
	}
	resp := netproto.BanListResponse{Bans: []netproto.BanEntry{}}
	for _, b := range bans {
		e := netproto.BanEntry{
			ID: b.ID, Type: b.Type, Value: b.Value, Reason: b.Reason,
			BannedBy: b.BannedBy, CreatedAt: b.CreatedAt.Unix(),
		}
		if b.ExpiresAt != nil {
			e.ExpiresAt = b.ExpiresAt.Unix()
		}
		resp.Bans = append(resp.Bans, e)
	}
	return s.writeMessage(client, netproto.MsgBanListResponse, resp)
}

// handleBanRemove lifts one ban by ID.
func (s *TCPServer) handleBanRemove(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.BanRemove
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed ban_remove: "+err.Error())
	}
	if s.deps == nil || s.deps.BanAdmin == nil {
		return s.sendError(client, errCodeUnavailable, "ban store unavailable")
	}
	if _, ok := s.banAdminAllowed(ctx, client); !ok {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyClientBan))
	}
	if err := s.deps.BanAdmin.DeleteBan(ctx, msg.BanID); err != nil {
		return s.sendError(client, errCodeUnavailable, "ban remove failed")
	}
	s.audit(ctx, client.UniqueID, "ban_remove", strconv.FormatInt(msg.BanID, 10), "")
	return nil
}

// e2e.go implements the E2EE key directory handlers (publish/request) and
// the chat-key distribution hooks (wave 4b). See chatkeys.go for the trust
// model and the key manager itself.
package server

import (
	"context"
	"encoding/base64"
	"errors"
	"time"

	"go.uber.org/zap"

	"voicx/internal/auth"
	"voicx/internal/netproto"
)

// handleKeyPublish records the client's X25519 public key: in the state
// manager for everyone (guests included), and persisted for registered
// users. Afterwards the client receives the current global chat key plus the
// key of its current channel, sealed to the fresh public key.
func (s *TCPServer) handleKeyPublish(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.KeyPublish
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed key_publish: "+err.Error())
	}
	raw, err := base64.StdEncoding.DecodeString(msg.PublicKey)
	if err != nil || len(raw) != 32 {
		return s.sendError(client, errCodeMalformed, "invalid public key (want 32-byte base64 X25519)")
	}
	if s.deps == nil || s.deps.State == nil {
		return s.sendError(client, errCodeUnavailable, "state backend unavailable")
	}

	s.deps.State.SetE2EPublicKey(client.ID, msg.PublicKey)
	if client.UserID != 0 && s.deps.Auth != nil {
		if err := s.deps.Auth.SetE2EPublicKey(ctx, client.UserID, msg.PublicKey); err != nil {
			s.logger.Warn("persisting e2e public key failed",
				zap.String("client_id", client.ID),
				zap.Error(err),
			)
		}
	}

	// Key delivery: global scope + the client's current channel (if any).
	s.deliverScopeKey(ctx, client, globalChatScope)
	if sc, ok := s.deps.State.GetClient(client.ID); ok && sc.ChannelID != 0 {
		s.deliverScopeKey(ctx, client, sc.ChannelID)
	}
	return nil
}

// handleKeyRequest answers a public-key lookup: online clients resolve from
// state (covers guests), registered users from the database. An empty key
// means the user never published one (old client).
func (s *TCPServer) handleKeyRequest(_ context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.KeyRequest
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed key_request: "+err.Error())
	}
	if msg.UniqueID == "" {
		return s.sendError(client, errCodeMalformed, "unique_id is required")
	}

	var pub string
	if s.deps != nil && s.deps.State != nil {
		if sc, ok := s.deps.State.GetClientByUniqueID(msg.UniqueID); ok {
			pub = sc.E2EPublicKey
		}
	}
	if pub == "" && s.deps != nil && s.deps.Auth != nil {
		if key, err := s.deps.Auth.GetE2EPublicKey(context.Background(), msg.UniqueID); err == nil {
			pub = key
		} else if !errors.Is(err, auth.ErrUserNotFound) {
			s.logger.Debug("e2e key lookup failed",
				zap.String("unique_id", msg.UniqueID),
				zap.Error(err),
			)
		}
	}
	return s.writeMessage(client, netproto.MsgKeyResponse, netproto.KeyResponse{
		UniqueID:  msg.UniqueID,
		PublicKey: pub,
	})
}

// deliverScopeKey seals the scope's current chat key for the client and
// sends it. Clients without a published key (old clients) are skipped.
//
// This is one of the three authorised minting sites: the caller has already
// established that the client belongs to the scope, so a first generation may
// be created here.
func (s *TCPServer) deliverScopeKey(ctx context.Context, client *Client, scope int64) {
	if s.deps == nil || s.deps.State == nil || s.chatKeys == nil {
		return
	}
	sc, ok := s.deps.State.GetClient(client.ID)
	if !ok || sc.E2EPublicKey == "" {
		return
	}
	gen, _, err := s.chatKeys.EnsureScope(ctx, scope)
	if err != nil {
		s.logger.Warn("ensuring chat key failed",
			zap.String("client_id", client.ID),
			zap.Int64("scope", scope),
			zap.Error(err),
		)
		return
	}
	ck, err := s.chatKeys.sealFor(ctx, scope, gen, sc.E2EPublicKey)
	if err != nil {
		s.logger.Warn("sealing chat key failed",
			zap.String("client_id", client.ID),
			zap.Int64("scope", scope),
			zap.Error(err),
		)
		return
	}
	if err := s.writeMessage(client, netproto.MsgChannelKey, ck); err != nil {
		s.logger.Debug("delivering chat key failed",
			zap.String("client_id", client.ID),
			zap.Error(err),
		)
	}
}

// scopeReadable reports whether the client may read a scope's messages: the
// global scope is open to everyone, a channel only to its current members.
// A current member may fetch EVERY generation of that scope — refusing older
// generations while still serving their rows would be theatre.
func (s *TCPServer) scopeReadable(client *Client, scope int64) bool {
	if scope == globalChatScope {
		return true
	}
	if s.deps == nil || s.deps.State == nil {
		return false
	}
	sc, ok := s.deps.State.GetClient(client.ID)
	return ok && sc.ChannelID == scope
}

// rotateScopeKey rotates a channel's chat key (a member left) and
// redistributes the new generation to the remaining members. The global
// scope is NOT rotated (it would spam every client on every disconnect; the
// global history trade-off is documented).
//
// Leaves inside chat_key_rotate_min_seconds are coalesced into one rotation:
// a client flapping on a bad link would otherwise mint one persisted
// generation per reconnect.
func (s *TCPServer) rotateScopeKey(ctx context.Context, channelID int64) {
	if channelID == 0 || s.deps == nil || s.deps.State == nil || s.chatKeys == nil {
		return
	}
	window := time.Duration(0)
	if s.cfg != nil && s.cfg.ChatKeyRotateMinSecs > 0 {
		window = time.Duration(s.cfg.ChatKeyRotateMinSecs) * time.Second
	}
	if window == 0 {
		s.doRotateScopeKey(ctx, channelID)
		return
	}
	s.rotMu.Lock()
	if s.rotPending[channelID] {
		// A timer is already armed for this scope; this leave rides along.
		s.rotMu.Unlock()
		return
	}
	if s.rotPending == nil {
		s.rotPending = map[int64]bool{}
	}
	s.rotPending[channelID] = true
	s.rotMu.Unlock()
	time.AfterFunc(window, func() {
		s.rotMu.Lock()
		delete(s.rotPending, channelID)
		s.rotMu.Unlock()
		s.doRotateScopeKey(context.Background(), channelID)
	})
}

// doRotateScopeKey performs one rotation and redistributes the result.
// Redistribution happens strictly AFTER rotate returns: deliverScopeKey ->
// sealFor -> at re-enters the scope entry's mutex.
func (s *TCPServer) doRotateScopeKey(ctx context.Context, channelID int64) {
	if _, _, err := s.chatKeys.rotate(ctx, channelID); err != nil {
		// A stale key is far better than an unreadable channel.
		s.logger.Warn("chat key rotation failed", zap.Int64("channel_id", channelID), zap.Error(err))
		return
	}
	for _, member := range s.deps.State.ChannelMembers(channelID) {
		if client, ok := s.clientByID(member.ClientID); ok {
			s.deliverScopeKey(ctx, client, channelID)
		}
	}
	s.logger.Debug("chat key rotated", zap.Int64("channel_id", channelID))
}

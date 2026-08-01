// e2e.go implements the E2EE key directory handlers (publish/request) and
// the chat-key distribution hooks (wave 4b). See chatkeys.go for the trust
// model and the key manager itself.
package server

import (
	"context"
	"encoding/base64"
	"errors"

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
	s.deliverScopeKey(client, globalChatScope)
	if sc, ok := s.deps.State.GetClient(client.ID); ok && sc.ChannelID != 0 {
		s.deliverScopeKey(client, sc.ChannelID)
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
func (s *TCPServer) deliverScopeKey(client *Client, scope int64) {
	if s.deps == nil || s.deps.State == nil || s.chatKeys == nil {
		return
	}
	sc, ok := s.deps.State.GetClient(client.ID)
	if !ok || sc.E2EPublicKey == "" {
		return
	}
	ck, err := s.chatKeys.sealFor(scope, sc.E2EPublicKey)
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

// rotateScopeKey rotates a channel's chat key (a member left) and
// redistributes the new generation to the remaining members. The global
// scope is NOT rotated (it would spam every client on every disconnect; the
// global history trade-off is documented).
func (s *TCPServer) rotateScopeKey(channelID int64) {
	if channelID == 0 || s.deps == nil || s.deps.State == nil || s.chatKeys == nil {
		return
	}
	s.chatKeys.rotate(channelID)
	for _, member := range s.deps.State.ChannelMembers(channelID) {
		if client, ok := s.clientByID(member.ClientID); ok {
			s.deliverScopeKey(client, channelID)
		}
	}
	s.logger.Debug("chat key rotated", zap.Int64("channel_id", channelID))
}

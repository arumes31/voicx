// handlers.go implements the per-message-type handlers of the TCP control
// server. All handlers run on the client's connection goroutine and reply via
// s.writeMessage; asynchronous traffic (snapshots excluded) is delivered by
// the broadcaster through the client's broadcast writer goroutine as MsgEvent
// frames.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"go.uber.org/zap"

	"voicx/internal/auth"
	"voicx/internal/broadcast"
	"voicx/internal/channels"
	"voicx/internal/netproto"
	"voicx/internal/permissions"
	"voicx/internal/state"
)

// Broadcast event types sent in MsgEvent envelopes.
const (
	eventUserJoined     = "user_joined"
	eventUserLeft       = "user_left"
	eventUserMoved      = "user_moved"
	eventChannelCreated = "channel_created"
	eventChannelDeleted = "channel_deleted"
	eventKicked         = "kicked"
	eventChat           = "chat"
	eventChannelUpdated = "channel_updated"
)

// userEvent is the payload of user_joined / user_left / user_moved events.
type userEvent struct {
	ClientID  string `json:"client_id"`
	UniqueID  string `json:"unique_id,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	ChannelID int64  `json:"channel_id,omitempty"`
}

// channelEvent is the payload of channel_created / channel_deleted events.
type channelEvent struct {
	ChannelID int64  `json:"channel_id"`
	Name      string `json:"name,omitempty"`
	ParentID  int64  `json:"parent_id,omitempty"`
}

// channelUpdatedEvent is the payload of channel_updated events, carrying the
// editable channel fields after a ChannelEdit.
type channelUpdatedEvent struct {
	ChannelID       int64  `json:"channel_id"`
	Topic           string `json:"topic"`
	MaxClients      int    `json:"max_clients"`
	OpusBitrate     int    `json:"opus_bitrate"`
	OpusFEC         bool   `json:"opus_fec"`
	OpusDTX         bool   `json:"opus_dtx"`
	OpusStereo      bool   `json:"opus_stereo"`
	SlowModeSeconds int    `json:"slow_mode_seconds"`
	Description     string `json:"description"`
}

// kickEvent is the payload of kicked events.
type kickEvent struct {
	ClientID   string `json:"client_id"`
	ByClientID string `json:"by_client_id"`
	Reason     string `json:"reason,omitempty"`
	FromServer bool   `json:"from_server"`
	Ban        bool   `json:"ban"`
}

// authChallengeTTL is how long a pending challenge-response challenge remains
// valid before the client must request a new one.
const authChallengeTTL = 30 * time.Second

// handleAuthenticate starts authentication. Paths:
//   - Password: verified against the users table (registered users).
//   - No password: challenge-response handshake (registered users, and
//     guests proving an Ed25519 identity via AuthSignature.PublicKey).
//   - Anonymous without Username or Password: immediate guest login with an
//     ephemeral guest: unique ID and the requested nickname.
//
// The global server password and ban checks apply to all paths, guests
// included.
func (s *TCPServer) handleAuthenticate(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.Authenticate
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed authenticate: "+err.Error())
	}
	if client.isAuthed() {
		return s.sendError(client, errCodeMalformed, "already authenticated")
	}
	if s.deps == nil || s.deps.Auth == nil {
		return s.sendError(client, errCodeUnavailable, "authentication backend unavailable")
	}

	// Global server password: when set, the client must supply it.
	if s.deps.ServerPasswordHash != "" {
		if err := auth.VerifyPassword(msg.ServerPassword, s.deps.ServerPasswordHash); err != nil {
			return s.writeMessage(client, netproto.MsgAuthResponse, netproto.AuthResponse{Reason: "invalid server password"})
		}
	}

	if msg.Anonymous && msg.Password != "" {
		return s.writeMessage(client, netproto.MsgAuthResponse, netproto.AuthResponse{Reason: "anonymous login takes no password"})
	}

	// Reject banned clients before anything else (unique-ID bans apply to
	// the presented ID; IP bans apply to everyone, including guests).
	ip := remoteIP(client.Conn)
	if reason, err := s.banRejectReason(ctx, client, msg.Username, ip); err != nil {
		return s.writeMessage(client, netproto.MsgAuthResponse, netproto.AuthResponse{Reason: "internal error"})
	} else if reason != "" {
		return s.writeMessage(client, netproto.MsgAuthResponse, netproto.AuthResponse{Reason: reason})
	}

	// Immediate ephemeral guest login.
	if msg.Anonymous && msg.Username == "" && msg.Password == "" {
		return s.completeGuestAuth(ctx, client, newGuestUniqueID(), msg.Nickname)
	}

	// No password: challenge-response handshake.
	if msg.Password == "" {
		challenge, err := auth.GenerateChallenge()
		if err != nil {
			s.logger.Warn("challenge generation failed",
				zap.String("client_id", client.ID),
				zap.Error(err),
			)
			return s.writeMessage(client, netproto.MsgAuthResponse, netproto.AuthResponse{Reason: "internal error"})
		}
		client.setChallenge(msg.Username, msg.Nickname, challenge, time.Now().Add(authChallengeTTL))
		return s.writeMessage(client, netproto.MsgAuthChallenge, netproto.AuthChallenge{Challenge: challenge})
	}

	ok, err := s.deps.Auth.AuthenticatePassword(ctx, msg.Username, msg.Password)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			// Not a unique ID: try nickname login (TS3 model — the account is
			// found by nickname, the client's identity key gets bound).
			user, err := s.deps.Auth.AuthenticateNickname(ctx, msg.Username, msg.Password)
			if err != nil {
				if errors.Is(err, auth.ErrUserNotFound) {
					return s.writeMessage(client, netproto.MsgAuthResponse, netproto.AuthResponse{Reason: "invalid credentials"})
				}
				s.logger.Warn("nickname auth failed",
					zap.String("client_id", client.ID),
					zap.Error(err),
				)
				return s.writeMessage(client, netproto.MsgAuthResponse, netproto.AuthResponse{Reason: "internal error"})
			}
			if user == nil {
				return s.writeMessage(client, netproto.MsgAuthResponse, netproto.AuthResponse{Reason: "invalid credentials"})
			}
			// Bind the client's identity key so future challenge logins work.
			if msg.PublicKey != "" {
				if err := s.deps.Auth.BindPublicKey(ctx, user.ID, msg.PublicKey); err != nil {
					s.logger.Warn("public key binding failed",
						zap.String("client_id", client.ID),
						zap.Error(err),
					)
				}
			}
			return s.completeAuthForUser(ctx, client, user)
		}
		s.logger.Warn("authenticate failed",
			zap.String("client_id", client.ID),
			zap.Error(err),
		)
		return s.writeMessage(client, netproto.MsgAuthResponse, netproto.AuthResponse{Reason: "internal error"})
	}
	if !ok {
		return s.writeMessage(client, netproto.MsgAuthResponse, netproto.AuthResponse{Reason: "invalid credentials"})
	}

	return s.completeAuth(ctx, client, msg.Username)
}

// handleAuthSignature completes the challenge-response handshake: it verifies
// the client's Ed25519 signature over the pending challenge and, on success,
// finishes authentication.
func (s *TCPServer) handleAuthSignature(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.AuthSignature
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed auth_signature: "+err.Error())
	}
	if client.isAuthed() {
		return s.sendError(client, errCodeMalformed, "already authenticated")
	}
	if s.deps == nil || s.deps.Auth == nil {
		return s.sendError(client, errCodeUnavailable, "authentication backend unavailable")
	}

	if msg.PublicKey != "" {
		return s.handleGuestSignature(ctx, client, msg)
	}

	challenge, _, ok := client.takeChallenge(msg.UniqueID)
	if !ok {
		return s.writeMessage(client, netproto.MsgAuthResponse, netproto.AuthResponse{Reason: "no pending challenge"})
	}

	verified, err := s.deps.Auth.AuthenticateChallenge(ctx, msg.UniqueID, challenge, msg.Signature)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return s.writeMessage(client, netproto.MsgAuthResponse, netproto.AuthResponse{Reason: "invalid credentials"})
		}
		s.logger.Warn("challenge verification failed",
			zap.String("client_id", client.ID),
			zap.Error(err),
		)
		return s.writeMessage(client, netproto.MsgAuthResponse, netproto.AuthResponse{Reason: "internal error"})
	}
	if !verified {
		return s.writeMessage(client, netproto.MsgAuthResponse, netproto.AuthResponse{Reason: "invalid credentials"})
	}

	return s.completeAuth(ctx, client, msg.UniqueID)
}

// handleGuestSignature verifies a signature against the presented public
// key and authenticates the client as a registered user (if the derived
// unique ID has a users row) or as a guest with the key-derived unique ID.
func (s *TCPServer) handleGuestSignature(ctx context.Context, client *Client, msg netproto.AuthSignature) error {
	uniqueID, err := auth.UniqueIDFromPublicKey(msg.PublicKey)
	if err != nil {
		return s.writeMessage(client, netproto.MsgAuthResponse, netproto.AuthResponse{Reason: "invalid public key"})
	}

	// A guest could bypass the Authenticate-time unique-ID ban check by
	// withholding the ID until now; re-check with the derived ID.
	ip := remoteIP(client.Conn)
	if reason, err := s.banRejectReason(ctx, client, uniqueID, ip); err != nil {
		return s.writeMessage(client, netproto.MsgAuthResponse, netproto.AuthResponse{Reason: "internal error"})
	} else if reason != "" {
		return s.writeMessage(client, netproto.MsgAuthResponse, netproto.AuthResponse{Reason: reason})
	}

	challenge, nickname, ok := client.takeChallenge("")
	if !ok {
		return s.writeMessage(client, netproto.MsgAuthResponse, netproto.AuthResponse{Reason: "no pending challenge"})
	}
	if err := auth.VerifyChallenge(msg.PublicKey, challenge, msg.Signature); err != nil {
		return s.writeMessage(client, netproto.MsgAuthResponse, netproto.AuthResponse{Reason: "invalid credentials"})
	}

	// Registered user with this identity? Then it is a normal login.
	if _, err := s.deps.Auth.LookupUser(ctx, uniqueID); err == nil {
		return s.completeAuth(ctx, client, uniqueID)
	} else if !errors.Is(err, auth.ErrUserNotFound) {
		s.logger.Warn("user lookup failed",
			zap.String("client_id", client.ID),
			zap.Error(err),
		)
		return s.writeMessage(client, netproto.MsgAuthResponse, netproto.AuthResponse{Reason: "internal error"})
	}

	// Account with this key bound via a nickname login? The account's unique
	// ID stays canonical even though it differs from the key-derived one.
	if user, err := s.deps.Auth.LookupUserByPublicKey(ctx, msg.PublicKey); err == nil {
		return s.completeAuthForUser(ctx, client, user)
	} else if !errors.Is(err, auth.ErrUserNotFound) {
		s.logger.Warn("user lookup by public key failed",
			zap.String("client_id", client.ID),
			zap.Error(err),
		)
		return s.writeMessage(client, netproto.MsgAuthResponse, netproto.AuthResponse{Reason: "internal error"})
	}

	return s.completeGuestAuth(ctx, client, uniqueID, nickname)
}

// banRejectReason returns the rejection reason when the unique ID or IP is
// actively banned, "" when clear, or an error on lookup failure.
func (s *TCPServer) banRejectReason(ctx context.Context, client *Client, uniqueID, ip string) (string, error) {
	ban, err := s.deps.Auth.LookupActiveBan(ctx, uniqueID, ip)
	if err != nil {
		s.logger.Warn("ban lookup failed",
			zap.String("client_id", client.ID),
			zap.Error(err),
		)
		return "", err
	}
	if ban == nil {
		return "", nil
	}
	reason := "banned"
	if ban.Reason != "" {
		reason = "banned: " + ban.Reason
	}
	s.logger.Info("banned client rejected",
		zap.String("client_id", client.ID),
		zap.String("unique_id", uniqueID),
		zap.String("ip", ip),
		zap.Int64("ban_id", ban.ID),
	)
	return reason, nil
}

// completeAuth finishes a successful registered-user authentication (any
// method): it resolves the user record and calls completeAuthForUser.
func (s *TCPServer) completeAuth(ctx context.Context, client *Client, uniqueID string) error {
	user, err := s.deps.Auth.LookupUser(ctx, uniqueID)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return s.writeMessage(client, netproto.MsgAuthResponse, netproto.AuthResponse{Reason: "invalid credentials"})
		}
		s.logger.Warn("user lookup failed",
			zap.String("client_id", client.ID),
			zap.Error(err),
		)
		return s.writeMessage(client, netproto.MsgAuthResponse, netproto.AuthResponse{Reason: "internal error"})
	}
	return s.completeAuthForUser(ctx, client, user)
}

// completeAuthForUser finishes authentication for an already-resolved user
// record: the account's unique ID is the canonical identity, its nickname
// the display name.
func (s *TCPServer) completeAuthForUser(ctx context.Context, client *Client, user *auth.User) error {
	nickname := user.Nickname
	if nickname == "" {
		nickname = user.UniqueID
	}
	return s.finishAuth(ctx, client, authIdentity{
		uniqueID: user.UniqueID,
		nickname: nickname,
		userID:   user.ID,
		admin:    user.IsAdmin,
	})
}

// completeGuestAuth finishes an anonymous login (ephemeral or key-derived).
// Guests are never admin and have no users row: their permission resolution
// yields an empty tiered set (all defaults), and they get no offline spool.
func (s *TCPServer) completeGuestAuth(ctx context.Context, client *Client, uniqueID, nickname string) error {
	if nickname == "" {
		nickname = "guest"
	}
	nickname = s.dedupeNickname(nickname)
	return s.finishAuth(ctx, client, authIdentity{
		uniqueID: uniqueID,
		nickname: nickname,
		guest:    true,
	})
}

// authIdentity is the resolved identity of an authenticated client.
type authIdentity struct {
	uniqueID string
	nickname string
	userID   int64
	admin    bool
	guest    bool
}

// finishAuth is the shared tail of all auth paths: record the identity,
// register with state and the broadcaster, reply with an AuthResponse
// followed by a full tree snapshot, deliver any spooled offline messages
// (registered users only), and announce the join.
func (s *TCPServer) finishAuth(ctx context.Context, client *Client, id authIdentity) error {
	client.setIdentity(id.uniqueID, id.nickname, id.userID, id.admin)

	// Default groups (143/144): a registered user with no server-group
	// memberships joins the Member group on first login. Guests virtually
	// hold the Guest group (see permCheckerFor).
	if !id.guest {
		s.assignDefaultGroup(ctx, client)
	}

	s.logger.Info("client authenticated",
		zap.String("client_id", client.ID),
		zap.String("unique_id", id.uniqueID),
		zap.String("nickname", id.nickname),
		zap.Bool("guest", id.guest),
	)

	if s.deps.State != nil {
		s.deps.State.AddClient(&state.Client{
			ClientID:    client.ID,
			UniqueID:    id.uniqueID,
			Nickname:    id.nickname,
			ConnectedAt: time.Now(),
			Conn:        client.Conn,
		})
	}

	if s.deps.Broadcast != nil {
		out, err := s.deps.Broadcast.Register(client.ID)
		if err != nil {
			s.logger.Warn("broadcast register failed",
				zap.String("client_id", client.ID),
				zap.Error(err),
			)
		} else {
			go s.broadcastWriter(client, out)
		}
	}

	// Reply first, then send the snapshot, then announce the join.
	resp := netproto.AuthResponse{
		OK:             true,
		ClientID:       client.ID,
		UniqueID:       id.uniqueID,
		Nickname:       id.nickname,
		TLSFingerprint: s.tlsFingerprint,
		MOTD:           s.serverSetting(ctx, "motd"),
		IsAdmin:        id.admin,
	}
	if s.deps.ICEServers != nil {
		resp.ICEServers = s.deps.ICEServers(id.uniqueID)
	}
	if err := s.writeMessage(client, netproto.MsgAuthResponse, resp); err != nil {
		return err
	}

	if err := s.sendSnapshot(client); err != nil {
		return err
	}

	// (133) active announcement, if any — after the snapshot so clients
	// process it as the first live event.
	if ann := s.serverSetting(ctx, "announcement"); ann != "" {
		if payload, err := eventEnvelope(eventAnnouncement, map[string]any{"text": ann}); err == nil {
			_ = s.writeFrame(client, &netproto.Frame{Type: uint16(netproto.MsgEvent), Payload: payload})
		}
	}

	// Guests have no users row, so no offline spool (spool inserts require a
	// users.id).
	if !id.guest {
		s.deliverSpooled(ctx, client, id.userID)
	}

	if s.deps.State != nil {
		s.metricsSink().SetClientsConnected(s.deps.State.ClientCount())
	}

	s.broadcastEvent(eventUserJoined, userEvent{
		ClientID: client.ID,
		UniqueID: id.uniqueID,
		Nickname: id.nickname,
	})
	return nil
}

// dedupeNickname appends #2, #3, ... when the nickname is already taken by
// an online client.
func (s *TCPServer) dedupeNickname(nickname string) string {
	taken := make(map[string]bool)
	s.mu.RLock()
	for _, c := range s.clients {
		c.mu.RLock()
		if c.authed {
			taken[c.Username] = true
		}
		c.mu.RUnlock()
	}
	s.mu.RUnlock()

	if !taken[nickname] {
		return nickname
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s#%d", nickname, i)
		if !taken[candidate] {
			return candidate
		}
	}
}

// newGuestUniqueID returns an ephemeral unique ID for a guest session,
// clearly distinguishable from key-derived (base64) unique IDs.
func newGuestUniqueID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("guest:%x", time.Now().UnixNano())
	}
	return "guest:" + hex.EncodeToString(b)
}

// handleCreateChannel creates a channel via the channel manager after a
// permission check keyed on the requested channel type, announces the new
// channel to all clients, and replies with the current channel list.
func (s *TCPServer) handleCreateChannel(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.CreateChannel
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed create_channel: "+err.Error())
	}
	if s.deps == nil || s.deps.Channels == nil {
		return s.sendError(client, errCodeUnavailable, "channel backend unavailable")
	}

	ct := channels.ChannelType(msg.Type)
	if !ct.Valid() {
		return s.sendError(client, errCodeMalformed, "invalid channel type")
	}

	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	var permKey permissions.PermissionKey
	switch ct {
	case channels.ChannelTypePermanent:
		permKey = permissions.PermissionKeyChannelCreatePermanent
	case channels.ChannelTypeSemiPermanent:
		permKey = permissions.PermissionKeyChannelCreateSemiPermanent
	default:
		permKey = permissions.PermissionKeyChannelCreateTemporary
	}
	if !pc.granted(permKey) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permKey))
	}
	// A caller may not set a needed join power above their own join power
	// (admins bypass), mirroring the TS3 power-cap rule.
	if msg.NeededJoinPower > 0 && !pc.admin &&
		pc.power(permissions.PermissionKeyChannelJoinPower) < msg.NeededJoinPower {
		return s.sendError(client, errCodePermissionDenied, "cannot set needed join power above your own join power")
	}

	channelID, err := s.deps.Channels.CreateChannel(ctx, channels.ChannelSpec{
		Name:            msg.Name,
		Topic:           msg.Topic,
		ParentID:        msg.ParentID,
		Type:            ct,
		MaxClients:      msg.MaxClients,
		Password:        msg.Password,
		NeededJoinPower: msg.NeededJoinPower,
		CreatedBy:       client.UserID,
		OpusBitrate:     msg.OpusBitrate,
		OpusFEC:         msg.OpusFEC,
		OpusDTX:         msg.OpusDTX,
		OpusStereo:      msg.OpusStereo,
	})
	if err != nil {
		s.logger.Warn("create channel failed",
			zap.String("client_id", client.ID),
			zap.Error(err),
		)
		return s.sendError(client, errCodeMalformed, "create channel failed: "+err.Error())
	}

	s.broadcastEvent(eventChannelCreated, channelEvent{
		ChannelID: channelID,
		Name:      msg.Name,
		ParentID:  msg.ParentID,
	})
	s.audit(ctx, client.UniqueID, "channel_create", fmt.Sprintf("%d", channelID), msg.Name)
	if s.deps.State != nil {
		s.metricsSink().SetChannelsActive(s.deps.State.ChannelCount())
	}
	return s.writeMessage(client, netproto.MsgChannelList, s.channelListResponse())
}

// handleDeleteChannel deletes a channel via the channel manager after a
// b_channel_delete check and announces the deletion to all clients.
func (s *TCPServer) handleDeleteChannel(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.DeleteChannel
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed delete_channel: "+err.Error())
	}
	if s.deps == nil || s.deps.Channels == nil {
		return s.sendError(client, errCodeUnavailable, "channel backend unavailable")
	}

	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.granted(permissions.PermissionKeyChannelDelete) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyChannelDelete))
	}

	if err := s.deps.Channels.DeleteChannel(ctx, msg.ChannelID); err != nil {
		if errors.Is(err, channels.ErrChannelNotFound) {
			return s.sendError(client, errCodeNotFound, "channel not found")
		}
		s.logger.Warn("delete channel failed",
			zap.String("client_id", client.ID),
			zap.Int64("channel_id", msg.ChannelID),
			zap.Error(err),
		)
		return s.sendError(client, errCodeUnavailable, "delete channel failed")
	}

	s.broadcastEvent(eventChannelDeleted, channelEvent{ChannelID: msg.ChannelID})
	s.audit(ctx, client.UniqueID, "channel_delete", fmt.Sprintf("%d", msg.ChannelID), "")
	if s.deps.State != nil {
		s.metricsSink().SetChannelsActive(s.deps.State.ChannelCount())
	}
	return nil
}

// handleChannelEdit edits a channel's settings (topic, max clients, Opus
// audio quality) after a b_channel_modify check, persists them, and
// announces the change to all clients.
func (s *TCPServer) handleChannelEdit(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ChannelEdit
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed channel_edit: "+err.Error())
	}
	if s.deps == nil || s.deps.Channels == nil || s.deps.State == nil {
		return s.sendError(client, errCodeUnavailable, "channel backend unavailable")
	}
	if _, ok := s.deps.State.GetChannel(msg.ChannelID); !ok {
		return s.sendError(client, errCodeNotFound, "channel not found")
	}

	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.granted(permissions.PermissionKeyChannelModify) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyChannelModify))
	}

	if err := s.deps.Channels.UpdateChannel(ctx, msg.ChannelID, channels.ChannelUpdate{
		Topic:           msg.Topic,
		MaxClients:      msg.MaxClients,
		OpusBitrate:     msg.OpusBitrate,
		OpusFEC:         msg.OpusFEC,
		OpusDTX:         msg.OpusDTX,
		OpusStereo:      msg.OpusStereo,
		SlowModeSeconds: msg.SlowModeSeconds,
		Description:     msg.Description,
	}); err != nil {
		if errors.Is(err, channels.ErrChannelNotFound) {
			return s.sendError(client, errCodeNotFound, "channel not found")
		}
		s.logger.Warn("channel edit failed",
			zap.String("client_id", client.ID),
			zap.Int64("channel_id", msg.ChannelID),
			zap.Error(err),
		)
		return s.sendError(client, errCodeMalformed, "channel edit failed: "+err.Error())
	}

	if ch, ok := s.deps.State.GetChannel(msg.ChannelID); ok {
		s.broadcastEvent(eventChannelUpdated, channelUpdatedEvent{
			ChannelID:       ch.ChannelID,
			Topic:           ch.Topic,
			MaxClients:      ch.MaxClients,
			OpusBitrate:     ch.OpusBitrate,
			OpusFEC:         ch.OpusFEC,
			OpusDTX:         ch.OpusDTX,
			OpusStereo:      ch.OpusStereo,
			SlowModeSeconds: ch.SlowModeSeconds,
			Description:     ch.Description,
		})
	}
	topic := ""
	if msg.Topic != nil {
		topic = *msg.Topic
	}
	s.audit(ctx, client.UniqueID, "channel_edit", fmt.Sprintf("%d", msg.ChannelID), topic)
	return nil
}

// BroadcastChannelUpdated announces a channel's current editable fields to
// all clients (used by out-of-band edits, e.g. ServerQuery channeledit).
func (s *TCPServer) BroadcastChannelUpdated(channelID int64) {
	if s.deps == nil || s.deps.State == nil {
		return
	}
	ch, ok := s.deps.State.GetChannel(channelID)
	if !ok {
		return
	}
	s.broadcastEvent(eventChannelUpdated, channelUpdatedEvent{
		ChannelID:       ch.ChannelID,
		Topic:           ch.Topic,
		MaxClients:      ch.MaxClients,
		OpusBitrate:     ch.OpusBitrate,
		OpusFEC:         ch.OpusFEC,
		OpusDTX:         ch.OpusDTX,
		OpusStereo:      ch.OpusStereo,
		SlowModeSeconds: ch.SlowModeSeconds,
		Description:     ch.Description,
	})
}

// handleJoinChannel moves the calling client into the target channel. Joining
// requires the caller's i_channel_join_power to meet the channel's
// needed_join_power; when the channel has a password, the caller must supply
// it unless they hold b_channel_join_ignore_password (TS3 semantics: the
// power check and the password check are independent).
func (s *TCPServer) handleJoinChannel(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.JoinChannel
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed join_channel: "+err.Error())
	}
	if s.deps == nil || s.deps.State == nil {
		return s.sendError(client, errCodeUnavailable, "state backend unavailable")
	}
	ch, ok := s.deps.State.GetChannel(msg.ChannelID)
	if !ok {
		return s.sendError(client, errCodeNotFound, "channel not found")
	}

	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.joinAllowed(ch.NeededJoinPower) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyChannelJoinPower))
	}
	if ch.PasswordHash != "" && !pc.granted(permissions.PermissionKeyChannelJoinIgnorePassword) {
		if err := auth.VerifyPassword(msg.Password, ch.PasswordHash); err != nil {
			return s.sendError(client, errCodePermissionDenied, "invalid channel password")
		}
	}

	if err := s.moveClient(client.ID, msg.ChannelID); err != nil {
		return s.sendError(client, errCodeNotFound, err.Error())
	}
	return nil
}

// handleMoveClient moves another client into a channel after an
// i_client_move_power vs i_client_needed_move_power check.
func (s *TCPServer) handleMoveClient(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.MoveClient
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed move_client: "+err.Error())
	}
	if s.deps == nil || s.deps.State == nil {
		return s.sendError(client, errCodeUnavailable, "state backend unavailable")
	}
	if _, ok := s.deps.State.GetChannel(msg.ChannelID); !ok {
		return s.sendError(client, errCodeNotFound, "channel not found")
	}
	target, ok := s.clientByID(msg.ClientID)
	if !ok || !target.isAuthed() {
		return s.sendError(client, errCodeNotFound, "target client not found")
	}

	if err := s.checkPowerOver(ctx, client, target,
		permissions.PermissionKeyClientMovePower,
		permissions.PermissionKeyClientNeededMovePower); err != nil {
		return s.sendError(client, errCodePermissionDenied, err.Error())
	}

	if err := s.moveClient(target.ID, msg.ChannelID); err != nil {
		return s.sendError(client, errCodeNotFound, err.Error())
	}
	return nil
}

// handleKickClient kicks a client from its channel or from the server (and
// optionally records a ban) after a kick/ban power vs needed power check.
func (s *TCPServer) handleKickClient(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.KickClient
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed kick_client: "+err.Error())
	}
	if s.deps == nil || s.deps.State == nil {
		return s.sendError(client, errCodeUnavailable, "state backend unavailable")
	}
	target, ok := s.clientByID(msg.ClientID)
	if !ok || !target.isAuthed() {
		return s.sendError(client, errCodeNotFound, "target client not found")
	}

	powerKey := permissions.PermissionKeyClientKickFromChannelPower
	neededKey := permissions.PermissionKeyClientNeededKickFromChannelPower
	if msg.FromServer || msg.Ban {
		powerKey = permissions.PermissionKeyClientKickFromServerPower
		neededKey = permissions.PermissionKeyClientNeededKickFromServerPower
	}
	if msg.Ban {
		powerKey = permissions.PermissionKeyClientBanPower
		neededKey = permissions.PermissionKeyClientNeededBanPower
	}
	if err := s.checkPowerOver(ctx, client, target, powerKey, neededKey); err != nil {
		return s.sendError(client, errCodePermissionDenied, err.Error())
	}

	if msg.Ban {
		if err := s.recordBan(ctx, client, target, msg.Reason, msg.DurationSeconds); err != nil {
			s.logger.Warn("recording ban failed",
				zap.String("client_id", client.ID),
				zap.String("target_id", target.ID),
				zap.Error(err),
			)
			return s.sendError(client, errCodeUnavailable, "recording ban failed")
		}
	}

	if err := s.performKick(client.ID, target.ID, msg.FromServer || msg.Ban, msg.Ban, msg.Reason); err != nil {
		return s.sendError(client, errCodeNotFound, err.Error())
	}
	action := "kick"
	if msg.Ban {
		action = "ban"
	}
	s.audit(ctx, client.UniqueID, action, target.UniqueID,
		fmt.Sprintf("from_server=%t duration_seconds=%d reason=%s", msg.FromServer || msg.Ban, msg.DurationSeconds, msg.Reason))
	return nil
}

// maxChatBytes caps the chat body size (for encrypted messages this is the
// base64 ciphertext length).
const maxChatBytes = 16 * 1024

// handleChatSend routes a chat message: channel chat to the channel's
// members, a direct message to a user by unique ID (spooled when offline), a
// direct message to an online connection by client ID (echoed back to the
// sender), or a global message to all clients.
//
// Encryption (4b): plaintext is rejected unless chat_allow_plaintext is set.
// For encrypted messages the server validates the scope key id (channel and
// global scopes only; direct messages are true E2EE and unverifiable by
// design) and the ciphertext size — it cannot read the body.
func (s *TCPServer) handleChatSend(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ChatSend
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed chat_send: "+err.Error())
	}
	if s.deps == nil || s.deps.Broadcast == nil {
		return s.sendError(client, errCodeUnavailable, "broadcast backend unavailable")
	}

	if len(msg.Text) > maxChatBytes {
		return s.sendError(client, errCodeMalformed, "chat message too large")
	}
	if !msg.Enc && !s.cfg.ChatAllowPlaintext {
		return s.sendError(client, errCodePermissionDenied, "plaintext chat is disabled on this server — update your client (chat encryption is mandatory)")
	}
	if msg.Enc && msg.KeyID == 0 && msg.ChannelID != "" {
		return s.sendError(client, errCodeMalformed, "channel chat requires a scope key id")
	}
	if msg.Enc && msg.ChannelID != "" {
		channelID, err := strconv.ParseInt(msg.ChannelID, 10, 64)
		if err != nil {
			return s.sendError(client, errCodeMalformed, "invalid channel_id: "+msg.ChannelID)
		}
		if s.chatKeys != nil {
			currentID, _, _ := s.chatKeys.current(ctx, channelID)
			if msg.KeyID != currentID {
				return s.sendError(client, errCodeMalformed, "stale chat key for channel (key rotated; wait for re-key)")
			}
		}
	}
	if msg.Enc && msg.ChannelID == "" && msg.ToUniqueID == "" && msg.ToClientID == "" && s.chatKeys != nil {
		// Global scope: validate against the global key generation.
		currentID, _, _ := s.chatKeys.current(ctx, globalChatScope)
		if msg.KeyID != currentID {
			return s.sendError(client, errCodeMalformed, "stale chat key for global scope (key rotated; wait for re-key)")
		}
	}

	isDM := msg.ToUniqueID != "" || msg.ToClientID != ""

	// Channel/global scopes run the moderation pipeline (wave 5a): rate
	// limit, slow mode, decrypt, filters, spam, mentions, store, relay.
	if !isDM {
		return s.routeScopedChat(ctx, client, msg, parseChannelID(msg.ChannelID))
	}

	// Direct messages are true E2EE: relay/spool only, no moderation.
	chat := netproto.ChatBroadcast{
		ChannelID:    msg.ChannelID,
		FromClientID: client.ID,
		FromUniqueID: client.UniqueID,
		From:         client.Username,
		Text:         msg.Text,
		Enc:          msg.Enc,
		KeyID:        msg.KeyID,
		E2E:          msg.Enc,
		ClientMsgID:  msg.ClientMsgID,
	}
	payload, err := eventEnvelope(eventChat, chat)
	if err != nil {
		return err
	}

	switch {
	case msg.ToUniqueID != "":
		return s.sendDirectByUniqueID(ctx, client, msg.ToUniqueID, payload, msg.Text)
	default: // msg.ToClientID != ""
		if err := s.deps.Broadcast.BroadcastToClient(msg.ToClientID, payload); err != nil {
			return s.sendError(client, errCodeNotFound, "target client not reachable")
		}
		// Echo the direct message back to the sender.
		_ = s.deps.Broadcast.BroadcastToClient(client.ID, payload)
		s.metricsSink().IncChatMessage("direct")
	}
	return nil
}

// sendDirectByUniqueID delivers a direct message to the user with the given
// unique ID. If the user is online the message is delivered immediately (and
// echoed to the sender); otherwise it is spooled into offline_messages for
// delivery at their next login.
func (s *TCPServer) sendDirectByUniqueID(ctx context.Context, client *Client, toUniqueID string, payload []byte, text string) error {
	if s.deps.Auth == nil {
		return s.sendError(client, errCodeUnavailable, "authentication backend unavailable")
	}
	target, err := s.deps.Auth.LookupUser(ctx, toUniqueID)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return s.sendError(client, errCodeNotFound, "target user not found")
		}
		s.logger.Warn("user lookup failed",
			zap.String("client_id", client.ID),
			zap.Error(err),
		)
		return s.sendError(client, errCodeUnavailable, "user lookup failed")
	}

	if tc, ok := s.clientByUniqueID(toUniqueID); ok {
		if err := s.deps.Broadcast.BroadcastToClient(tc.ID, payload); err != nil {
			return s.sendError(client, errCodeNotFound, "target client not reachable")
		}
		_ = s.deps.Broadcast.BroadcastToClient(client.ID, payload)
		s.metricsSink().IncChatMessage("direct")
		return nil
	}

	if s.deps.Spool == nil {
		return s.sendError(client, errCodeNotFound, "target user is offline")
	}
	// E2EE DMs are spooled as ciphertext the server cannot read; the sender's
	// unique ID travels along so the recipient can fetch the public key.
	if err := s.deps.Spool.SpoolMessage(ctx, client.UserID, target.ID, client.UniqueID, text); err != nil {
		s.logger.Warn("spooling message failed",
			zap.String("client_id", client.ID),
			zap.Error(err),
		)
		return s.sendError(client, errCodeUnavailable, "spooling message failed")
	}
	s.logger.Info("message spooled for offline user",
		zap.String("client_id", client.ID),
		zap.String("to_unique_id", toUniqueID),
	)
	return nil
}

// deliverSpooled sends any spooled offline messages for the user to the
// client as offline chat events and marks them delivered.
func (s *TCPServer) deliverSpooled(ctx context.Context, client *Client, userID int64) {
	if s.deps.Spool == nil || s.deps.Broadcast == nil {
		return
	}
	msgs, err := s.deps.Spool.PendingMessages(ctx, userID)
	if err != nil {
		s.logger.Warn("loading spooled messages failed",
			zap.String("client_id", client.ID),
			zap.Error(err),
		)
		return
	}
	if len(msgs) == 0 {
		return
	}

	ids := make([]int64, 0, len(msgs))
	for _, m := range msgs {
		// Messages spooled with a from_unique_id are E2EE DMs (ciphertext);
		// older rows are plaintext and delivered as-is.
		e2e := m.FromUniqueID != ""
		payload, err := eventEnvelope(eventChat, netproto.ChatBroadcast{
			FromClientID: strconv.FormatInt(m.FromUserID, 10),
			FromUniqueID: m.FromUniqueID,
			From:         m.FromName,
			Text:         m.Message,
			Offline:      true,
			Enc:          e2e,
			E2E:          e2e,
		})
		if err != nil {
			continue
		}
		if err := s.deps.Broadcast.BroadcastToClient(client.ID, payload); err != nil {
			s.logger.Warn("delivering spooled message failed",
				zap.String("client_id", client.ID),
				zap.Int64("message_id", m.ID),
				zap.Error(err),
			)
			continue
		}
		ids = append(ids, m.ID)
	}

	if err := s.deps.Spool.MarkMessagesDelivered(ctx, ids); err != nil {
		s.logger.Warn("marking spooled messages delivered failed",
			zap.String("client_id", client.ID),
			zap.Error(err),
		)
	}
}

// handlePing replies with a Pong.
func (s *TCPServer) handlePing(_ context.Context, client *Client, _ *netproto.Frame) error {
	s.logger.Debug("ping received", zap.String("client_id", client.ID))
	return s.writeMessage(client, netproto.MsgPong, netproto.Pong{})
}

// --- handler helpers -------------------------------------------------------

// remoteIP extracts the host part of the connection's remote address, used
// for IP ban lookups.
func remoteIP(conn net.Conn) string {
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return conn.RemoteAddr().String()
	}
	return host
}

// moveClient moves a client into a channel in the state manager, performs the
// temp-channel cleanup bookkeeping for the source and target channels, keeps
// the voice router's membership in sync, and announces the move.
func (s *TCPServer) moveClient(clientID string, channelID int64) error {
	var oldChannelID int64
	if sc, ok := s.deps.State.GetClient(clientID); ok {
		oldChannelID = sc.ChannelID
	}
	if err := s.deps.State.MoveClient(clientID, channelID); err != nil {
		return err
	}
	if s.deps.Channels != nil {
		if oldChannelID != 0 && oldChannelID != channelID {
			s.deps.Channels.OnClientLeftChannel(oldChannelID)
		}
		s.deps.Channels.OnClientJoinedChannel(channelID)
	}
	if s.deps.Voice != nil {
		if oldChannelID != 0 && oldChannelID != channelID {
			s.deps.Voice.LeaveChannel(clientID, oldChannelID)
		}
		s.deps.Voice.JoinChannel(clientID, channelID)
	}
	// Chat keys (4b): the client gets the new channel's key; the channel it
	// left rotates so ex-members cannot read new messages.
	if oldChannelID != 0 && oldChannelID != channelID {
		s.rotateScopeKey(context.Background(), oldChannelID)
	}
	if client, ok := s.clientByID(clientID); ok {
		s.deliverScopeKey(context.Background(), client, channelID)
	}
	s.broadcastEvent(eventUserMoved, userEvent{ClientID: clientID, ChannelID: channelID})
	return nil
}

// checkPowerOver resolves both the caller's power permission and the target's
// needed power permission and reports whether the caller may act on the
// target. Non-admin callers may not act on admin targets.
func (s *TCPServer) checkPowerOver(ctx context.Context, caller, target *Client, powerKey, neededKey permissions.PermissionKey) error {
	callerPC, err := s.permCheckerFor(ctx, caller)
	if err != nil {
		return err
	}
	targetPC, err := s.permCheckerFor(ctx, target)
	if err != nil {
		return err
	}
	if !callerPC.admin && targetPC.admin {
		return errors.New("cannot act on a server admin")
	}
	if !callerPC.powerAtLeast(powerKey, targetPC.neededPower(neededKey)) {
		return errors.New("insufficient permission: " + string(powerKey))
	}
	return nil
}

// recordBan inserts a unique-ID ban for the target. durationSeconds > 0 makes
// the ban temporary (171); 0 is permanent.
func (s *TCPServer) recordBan(ctx context.Context, caller, target *Client, reason string, durationSeconds int64) error {
	var bannedBy any
	if caller.UserID != 0 {
		bannedBy = caller.UserID
	}
	var expiresAt any
	if durationSeconds > 0 {
		expiresAt = time.Now().Add(time.Duration(durationSeconds) * time.Second)
	}
	return s.insertBan(ctx, target.UniqueID, reason, bannedBy, expiresAt)
}

// insertBan inserts a unique-ID ban into the bans table. expiresAt nil (or
// the nil interface) means a permanent ban. It is a no-op when ban
// persistence is not wired; kicks still proceed.
func (s *TCPServer) insertBan(ctx context.Context, uniqueID, reason string, bannedBy, expiresAt any) error {
	if s.deps.Bans == nil {
		return nil // ban persistence not wired; kick still proceeds
	}
	const q = `INSERT INTO bans (ban_type, value, reason, banned_by, expires_at) VALUES (1, $1, $2, $3, $4)`
	if _, err := s.deps.Bans.DB().ExecContext(ctx, q, uniqueID, reason, bannedBy, expiresAt); err != nil {
		return err
	}
	return nil
}

// sendSnapshot builds the current channel-tree snapshot and sends it to the
// client as a MsgSnapshot frame.
func (s *TCPServer) sendSnapshot(client *Client) error {
	if s.deps == nil || s.deps.State == nil {
		return nil
	}
	snap := broadcast.BuildSnapshot(s.deps.State, client.isAdmin(), client.UniqueID)
	payload, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	return s.writeFrame(client, &netproto.Frame{Type: uint16(netproto.MsgSnapshot), Payload: payload})
}

// broadcastEvent marshals payload and broadcasts it to all registered clients
// as an event envelope. It is a no-op when no broadcaster is wired.
func (s *TCPServer) broadcastEvent(eventType string, payload any) {
	if s.deps == nil || s.deps.Broadcast == nil {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		s.logger.Error("failed to marshal event payload",
			zap.String("event_type", eventType),
			zap.Error(err),
		)
		return
	}
	s.deps.Broadcast.BroadcastEvent(eventType, raw)
}

// broadcastToAdmins sends an event to every online server admin (used by
// the invisible-status semantics, 381).
func (s *TCPServer) broadcastToAdmins(eventType string, payload any) {
	if s.deps == nil || s.deps.Broadcast == nil {
		return
	}
	raw, err := eventEnvelope(eventType, payload)
	if err != nil {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.clients {
		if c.isAdmin() {
			_ = s.deps.Broadcast.BroadcastToClient(c.ID, raw)
		}
	}
}

// eventEnvelope wraps payload in the {"type": ..., "data": ...} envelope used
// for targeted broadcasts (BroadcastToChannel / BroadcastToClient), matching
// the shape Broadcaster.BroadcastEvent produces for server-wide events.
func eventEnvelope(eventType string, payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	envelope := struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}{Type: eventType, Data: raw}
	return json.Marshal(envelope)
}

// channelListResponse builds a ChannelList reply from the current state.
func (s *TCPServer) channelListResponse() netproto.ChannelList {
	var list netproto.ChannelList
	if s.deps == nil || s.deps.State == nil {
		return list
	}
	for _, ch := range s.deps.State.ChannelTree() {
		list.Channels = append(list.Channels, netproto.Channel{
			ID:      strconv.FormatInt(ch.ChannelID, 10),
			Name:    ch.Name,
			Clients: ch.ClientCount,
		})
	}
	return list
}

// broadcastWriter pumps outbound broadcast payloads to the client connection
// as MsgEvent frames until the broadcaster closes the channel (on Unregister)
// or a write fails.
func (s *TCPServer) broadcastWriter(client *Client, out <-chan []byte) {
	for payload := range out {
		frame := &netproto.Frame{Type: uint16(netproto.MsgEvent), Payload: payload}
		if err := s.writeFrame(client, frame); err != nil {
			return
		}
	}
}

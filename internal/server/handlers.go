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
	// Join power (160), sort index (163), parent (168) and the inheritance
	// toggle (157) ride the same event so a client that edited any of them
	// sees the result without refetching the tree.
	NeededJoinPower    int   `json:"needed_join_power"`
	OrderIndex         int   `json:"order_index"`
	ParentID           int64 `json:"parent_id"`
	InheritPermissions bool  `json:"inherit_permissions"`
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
	// (133) the encryption key is captured before any auth path branches, so
	// finishAuth can seal the global generation and the MOTD into the reply
	// whichever path completes.
	if msg.X25519PublicKey != "" {
		client.setX25519(msg.X25519PublicKey)
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
	// The guest/challenge path leaves Authenticate.PublicKey empty and carries
	// its keys here instead, so the encryption key is captured here too (133).
	if msg.X25519PublicKey != "" {
		client.setX25519(msg.X25519PublicKey)
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
			// (180) resolved once at auth so the badge rides every snapshot.
			IsBot: s.ClientIsBot(ctx, client),
		})
		// (133) the key must be live before the snapshot, so anything that
		// reads it during this handshake sees it.
		if x := client.x25519(); x != "" {
			s.deps.State.SetE2EPublicKey(client.ID, x)
		}
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
		IsAdmin:        id.admin,
	}
	s.attachChatKeysAndMOTD(ctx, client, &resp)
	if s.deps.ICEServers != nil {
		resp.ICEServers = s.deps.ICEServers(id.uniqueID)
	}
	if err := s.writeMessage(client, netproto.MsgAuthResponse, resp); err != nil {
		return err
	}

	if err := s.sendSnapshot(client); err != nil {
		return err
	}
	// (312) Seed the client's channel-tab model with the authoritative set.
	// The current channel is implicit and therefore cannot be unsubscribed.
	if err := s.sendSubscriptionState(client); err != nil {
		return err
	}

	// (133) active announcement, if any — after the snapshot so clients
	// process it as the first live event. It stays here rather than moving
	// behind key publish: deliverScopeKey skips clients that never published
	// one, which would silently drop the announcement for all of them.
	if ann, gen, err := s.serverSettingSealed(ctx, "announcement"); err == nil && ann != "" {
		data := map[string]any{"text": ann, "enc": gen > 0, "key_id": gen}
		if payload, err := eventEnvelope(eventAnnouncement, data); err == nil {
			_ = s.writeFrame(client, &netproto.Frame{Type: uint16(netproto.MsgEvent), Payload: payload})
		}
	}

	// Guests have no users row, so no offline spool (spool inserts require a
	// users.id).
	if !id.guest {
		s.deliverSpooled(ctx, client, id.userID)
	}

	// (216) the rules gate arms last in the handshake: the client already has
	// the tree and its own identity, so the modal goes up over a rendered UI
	// and a failing rules read costs it none of that.
	s.sendPendingRules(ctx, client, id.guest)

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

// attachChatKeysAndMOTD seals the global generation for the client's X25519
// key and hands over the MOTD sealed under that same generation, so the
// client can render it before Connect() returns — no extra frame, no
// ordering rule, no race (133).
//
// A client that published no encryption key gets neither: it could not open
// them. Under the plaintext escape hatch such a client still sees the MOTD,
// which is the only reason that branch exists.
func (s *TCPServer) attachChatKeysAndMOTD(ctx context.Context, client *Client, resp *netproto.AuthResponse) {
	pub := client.x25519()
	if pub == "" {
		if s.cfg != nil && s.cfg.ChatAllowPlaintext {
			resp.MOTD = s.serverSettingPlain(ctx, "motd")
		}
		return
	}
	if s.chatKeys == nil || !s.chatKeys.configured() {
		return
	}
	// Never mints: the global generation is created once at boot.
	gen, _, err := s.chatKeys.current(ctx, globalChatScope)
	if err != nil {
		s.logger.Warn("global chat key unavailable at auth",
			zap.String("client_id", client.ID),
			zap.Error(err),
		)
		return
	}
	ck, err := s.chatKeys.sealFor(ctx, globalChatScope, gen, pub)
	if err != nil {
		s.logger.Warn("sealing global chat key for auth response failed",
			zap.String("client_id", client.ID),
			zap.Error(err),
		)
		return
	}
	resp.ChatKeys = []netproto.ChannelKey{*ck}

	motd, motdGen, err := s.serverSettingSealed(ctx, "motd")
	if err != nil || motd == "" {
		return
	}
	resp.MOTD, resp.MOTDEnc, resp.MOTDKeyID = motd, true, motdGen
}

// setRulesPending arms or clears the server-rules gate on the connection
// (215).
func (c *Client) setRulesPending(pending bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rulesPending = pending
}

// rulesBlocked reports whether the session still owes the server rules an
// answer (215).
func (c *Client) rulesBlocked() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.rulesPending
}

// sendPendingRules delivers the operator's rules when this session still owes
// them an answer, and arms the gate that keeps the client out of channels and
// chat until it gives one (215).
//
// Guests are asked on EVERY connect and their answer lives only in this
// session: server_rules_acceptance.user_id references users.id, and a guest
// has no such row, so no acceptance can be recorded for them. Asking every
// time is the honest reading of an item that publishes the rules to everyone
// who joins — the alternative, not asking at all, would exempt exactly the
// users the operator knows least about.
func (s *TCPServer) sendPendingRules(ctx context.Context, client *Client, guest bool) {
	if s.deps == nil || s.deps.Rules == nil {
		return
	}
	var (
		text, hash string
		pending    bool
		err        error
	)
	if guest {
		text, hash, err = s.deps.Rules.Text(ctx)
		pending = hash != ""
	} else {
		text, hash, pending, err = s.deps.Rules.Pending(ctx, client.UserID)
	}
	if err != nil {
		s.logger.Warn("reading the server rules failed",
			zap.String("client_id", client.ID),
			zap.Error(err),
		)
		return
	}
	if !pending {
		return
	}
	client.setRulesPending(true)
	if err := s.writeMessage(client, netproto.MsgServerRules, netproto.ServerRules{Text: text, Hash: hash}); err != nil {
		s.logger.Warn("sending the server rules failed",
			zap.String("client_id", client.ID),
			zap.Error(err),
		)
	}
}

// handleServerRulesAccept records the caller's acceptance of the wording it
// was shown (215). A stale hash is refused with an error frame AND a fresh
// ServerRules frame carrying the text actually in force, so the client
// re-displays instead of silently consenting to words nobody read. An empty
// ServerRules frame is the acknowledgement of a successful accept: the gate
// state stays server-authoritative, so the dialog never has to guess.
func (s *TCPServer) handleServerRulesAccept(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ServerRulesAccept
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed server_rules_accept: "+err.Error())
	}
	if s.deps == nil || s.deps.Rules == nil {
		return s.sendError(client, errCodeUnavailable, "server rules unavailable")
	}
	text, hash, err := s.deps.Rules.Text(ctx)
	if err != nil {
		s.logger.Warn("reading the server rules failed",
			zap.String("client_id", client.ID),
			zap.Error(err),
		)
		return s.sendError(client, errCodeUnavailable, "server rules unavailable")
	}
	if hash == "" || msg.Hash != hash {
		if err := s.sendError(client, errCodeMalformed, "the server rules changed since they were shown"); err != nil {
			return err
		}
		client.setRulesPending(hash != "")
		return s.writeMessage(client, netproto.MsgServerRules, netproto.ServerRules{Text: text, Hash: hash})
	}
	// A guest has no users row to write the acceptance to, so it stays on the
	// connection (see sendPendingRules).
	if client.UserID != 0 {
		if err := s.deps.Rules.Accept(ctx, client.UserID, msg.Hash); err != nil {
			s.logger.Warn("recording the rules acceptance failed",
				zap.String("client_id", client.ID),
				zap.Error(err),
			)
			return s.sendError(client, errCodeUnavailable, "recording the acceptance failed")
		}
	}
	client.setRulesPending(false)
	return s.writeMessage(client, netproto.MsgServerRules, netproto.ServerRules{})
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

	// (312) taken before the delete: removing the channel from the state
	// manager is what drops its subscriptions, leaving nobody to notify.
	subscribers := s.subscriberIDsOf(msg.ChannelID)

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
	s.pushSubscriptionStateTo(subscribers)
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
	channel, ok := s.deps.State.GetChannel(msg.ChannelID)
	if !ok {
		return s.sendError(client, errCodeNotFound, "channel not found")
	}

	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.granted(permissions.PermissionKeyChannelModify) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyChannelModify))
	}
	// Same power cap as channel creation (160): an editor may not raise the
	// needed join power above their own join power, or they could lock
	// themselves and their peers out of a channel they still administer.
	if msg.NeededJoinPower != nil && !pc.admin {
		if pc.power(permissions.PermissionKeyChannelJoinPower) < *msg.NeededJoinPower {
			return s.sendError(client, errCodePermissionDenied, "cannot set needed join power above your own join power")
		}
		if *msg.NeededJoinPower < channel.NeededJoinPower {
			return s.sendError(client, errCodePermissionDenied, "cannot reduce the channel's needed join power")
		}
	}

	if err := s.deps.Channels.UpdateChannel(ctx, msg.ChannelID, channels.ChannelUpdate{
		Topic:              msg.Topic,
		MaxClients:         msg.MaxClients,
		OpusBitrate:        msg.OpusBitrate,
		OpusFEC:            msg.OpusFEC,
		OpusDTX:            msg.OpusDTX,
		OpusStereo:         msg.OpusStereo,
		SlowModeSeconds:    msg.SlowModeSeconds,
		Description:        msg.Description,
		NeededJoinPower:    msg.NeededJoinPower,
		OrderIndex:         msg.OrderIndex,
		ParentID:           msg.ParentID,
		InheritPermissions: msg.InheritPermissions,
	}); err != nil {
		if errors.Is(err, channels.ErrInvalidMove) {
			return s.sendError(client, errCodeMalformed, err.Error())
		}
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

	// (157) re-parenting or flipping inheritance changes the resolved channel
	// tier for every user in the affected subtree, and the loader caches per
	// (user, channel) — nothing here can enumerate those pairs, so drop all.
	if s.deps.Perms != nil && (msg.InheritPermissions != nil || msg.ParentID != nil) {
		s.deps.Perms.InvalidateAll()
	}

	if ch, ok := s.deps.State.GetChannel(msg.ChannelID); ok {
		s.broadcastEvent(eventChannelUpdated, channelUpdatedEventFor(ch))
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
	s.broadcastEvent(eventChannelUpdated, channelUpdatedEventFor(ch))
}

// channelUpdatedEventFor snapshots a channel's editable fields for the
// channel_updated event.
func channelUpdatedEventFor(ch *state.Channel) channelUpdatedEvent {
	return channelUpdatedEvent{
		ChannelID:          ch.ChannelID,
		Topic:              ch.Topic,
		MaxClients:         ch.MaxClients,
		OpusBitrate:        ch.OpusBitrate,
		OpusFEC:            ch.OpusFEC,
		OpusDTX:            ch.OpusDTX,
		OpusStereo:         ch.OpusStereo,
		SlowModeSeconds:    ch.SlowModeSeconds,
		Description:        ch.Description,
		NeededJoinPower:    ch.NeededJoinPower,
		OrderIndex:         ch.OrderIndex,
		ParentID:           ch.ParentID,
		InheritPermissions: ch.InheritPermissions,
	}
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
	// (215) acceptance is a condition of entry, not a notice: an unanswered
	// rules prompt keeps the client in the lobby, where the only thing it can
	// still do is answer.
	if client.rulesBlocked() {
		return s.sendError(client, errCodePermissionDenied, "accept the server rules before joining a channel")
	}
	ch, ok := s.deps.State.GetChannel(msg.ChannelID)
	if !ok {
		return s.sendError(client, errCodeNotFound, "channel not found")
	}

	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	// Inheriting sub-channels are gated by the strongest needed power on their
	// chain (157/168), so a child cannot be used as a back door into a gated
	// parent's subtree.
	if !pc.joinAllowed(s.deps.State.EffectiveJoinPower(ch.ChannelID)) {
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
	if target.rulesBlocked() {
		return s.sendError(client, errCodePermissionDenied, "target must accept the server rules before joining a channel")
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
	// (215) same gate as the join: rules the user has not answered would
	// otherwise be advisory, and DMs would route around a channel-only check.
	if client.rulesBlocked() {
		return s.sendError(client, errCodePermissionDenied, "accept the server rules before sending messages")
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

	isDM := msg.ToUniqueID != "" || msg.ToClientID != ""

	// Channel/global scopes run the moderation pipeline (wave 5a): rate
	// limit, slow mode, decrypt, filters, spam, mentions, store, relay.
	if !isDM {
		var channelID int64
		if msg.ChannelID != "" {
			id, err := strconv.ParseInt(msg.ChannelID, 10, 64)
			if err != nil {
				return s.sendError(client, errCodeMalformed, "invalid channel_id: "+msg.ChannelID)
			}
			channelID = id
		}
		// Read entitlement FIRST, before anything touches chatKeys: members and
		// entitled subscribers may write the channel tab they can read, while
		// routeScopedChat may ensure a scope's first generation. Reaching it with an
		// attacker-supplied channel id is a disk-exhaustion DoS (91).
		if channelID != 0 {
			if s.deps.State == nil {
				return s.sendError(client, errCodeUnavailable, "state backend unavailable")
			}
			if !s.scopeReadable(ctx, client, channelID) {
				return s.sendError(client, errCodePermissionDenied, "not a member or subscriber of this channel")
			}
		}
		if msg.Enc {
			if s.chatKeys == nil {
				return s.sendError(client, errCodeUnavailable, "chat key manager unavailable")
			}
			// Non-minting lookup: an unknown scope is "rejoin", never a mint.
			currentID, _, err := s.chatKeys.current(ctx, channelID)
			if errors.Is(err, ErrNoScopeKey) {
				return s.sendError(client, errCodeUnavailable, "no chat key for this channel yet — rejoin the channel")
			}
			if err != nil {
				return s.sendError(client, errCodeUnavailable, "chat key unavailable")
			}
			if msg.KeyID != currentID {
				scope := "channel"
				if channelID == 0 {
					scope = "global scope"
				}
				return s.sendError(client, errCodeMalformed, "stale chat key for "+scope+" (key rotated; wait for re-key)")
			}
		}
		return s.routeScopedChat(ctx, client, msg, channelID)
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
		return s.sendDirectByUniqueID(ctx, client, msg.ToUniqueID, payload, msg.Text, msg.Enc)
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
func (s *TCPServer) sendDirectByUniqueID(ctx context.Context, client *Client, toUniqueID string, payload []byte, text string, enc bool) error {
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
	// A DM has no scope key, so the server cannot seal one on the sender's
	// behalf: a plaintext DM to an offline user would land in the spool in
	// the clear. Relaying it live is the sender's choice; persisting it is
	// not, so the escape hatch stops at the spool (91).
	if !enc {
		return s.sendError(client, errCodePermissionDenied, "target user is offline and plaintext direct messages are never spooled — encrypt the message")
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
		// Every spooled row is E2EE ciphertext: 012 deleted the undelivered
		// pre-4b plaintext rows and offline_messages_sealed stops new ones,
		// so there is no plaintext replay branch left to take (91).
		payload, err := eventEnvelope(eventChat, netproto.ChatBroadcast{
			FromClientID: strconv.FormatInt(m.FromUserID, 10),
			FromUniqueID: m.FromUniqueID,
			From:         m.FromName,
			Text:         m.Message,
			Offline:      true,
			Enc:          true,
			E2E:          true,
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
		_ = s.deliverScopeKey(context.Background(), client, channelID)
		// (312) the channel a client stands in is implicitly subscribed, so a
		// move changes the authoritative set even though nothing was asked.
		_ = s.sendSubscriptionState(client)
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

// channelListResponse builds a ChannelList reply from the current state. The
// list carries no order field, so the row order IS the order (163): it must be
// the same total order the snapshot tree uses.
func (s *TCPServer) channelListResponse() netproto.ChannelList {
	var list netproto.ChannelList
	if s.deps == nil || s.deps.State == nil {
		return list
	}
	for _, ch := range s.deps.State.ChannelTreeOrdered() {
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

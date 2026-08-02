// Package server hosts the long-running voicx server components. This file
// implements the TCP control listener: it accepts connections, frames messages
// using the netproto wire format, dispatches them to per-message-type
// handlers, and tracks connected clients in a thread-safe registry.
//
// The handlers live in handlers.go and are wired to the auth, state,
// channels, broadcast, and permissions backends via Deps (see deps.go). A
// client must authenticate before any command other than Authenticate and
// Ping is accepted.
package server

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"go.uber.org/zap"

	"voicx/internal/chatcrypto"
	"voicx/internal/config"
	"voicx/internal/metrics"
	"voicx/internal/netproto"
	"voicx/internal/tlscert"
)

// Error codes sent in MsgError frames.
const (
	errCodeUnknown          = 1 // unknown message type
	errCodeMalformed        = 2 // malformed message payload
	errCodeNotAuthenticated = 3 // command requires authentication
	errCodePermissionDenied = 4 // caller lacks the required permission
	errCodeUnavailable      = 5 // backend dependency unavailable
	errCodeNotFound         = 6 // referenced entity not found
	errCodeConflict         = 7 // optimistic concurrency precondition failed
)

// Client represents a single connected control-channel client.
type Client struct {
	ID       string
	Conn     net.Conn
	Username string // nickname, once authenticated
	UniqueID string // TS3-style unique ID, once authenticated
	UserID   int64  // database users.id, once authenticated

	mu     sync.RWMutex
	authed bool
	admin  bool
	// rulesPending gates a session that still owes the operator's rules an
	// answer (215). It is per connection, not per account, because a guest
	// has no users row to record an acceptance against.
	rulesPending bool

	// Activity and connection stats (Client Info dialog).
	lastActive time.Time // last received frame
	bytesIn    int64     // payload bytes received
	bytesOut   int64     // payload bytes sent
	lastPingAt time.Time // last server-initiated Ping sent
	rttNs      int64     // smoothed RTT in nanoseconds (EWMA)
	rttKnown   bool      // whether any Pong was received

	// Pending challenge-response handshake state (set on Authenticate without
	// a password, consumed by AuthSignature).
	challenge     []byte
	challengeUID  string
	challengeNick string
	challengeExp  time.Time

	// x25519Key is the encryption key presented at authenticate time so the
	// server can seal the global generation and the MOTD into the auth
	// response (133). The identity PublicKey is Ed25519 and cannot be sealed
	// to, hence the separate field.
	x25519Key string

	wmu sync.Mutex // serializes frame writes to Conn
}

// isAuthed reports whether the client has completed authentication.
func (c *Client) isAuthed() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.authed
}

// isAdmin reports whether the client authenticated as a server admin.
func (c *Client) isAdmin() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.admin
}

// setIdentity atomically records the authenticated identity on the client.
func (c *Client) setIdentity(uniqueID, nickname string, userID int64, admin bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.UniqueID = uniqueID
	c.Username = nickname
	c.UserID = userID
	c.admin = admin
	c.authed = true
}

// promote records the durable identity created by a guest token redemption.
func (c *Client) promote(userID int64, admin bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.UserID = userID
	c.admin = c.admin || admin
}

// uniqueID returns the client's authenticated unique ID ("" before auth).
func (c *Client) uniqueID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.UniqueID
}

// setX25519 records the encryption key presented at authenticate time.
func (c *Client) setX25519(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.x25519Key = key
}

// x25519 returns the encryption key presented at authenticate time ("" for
// clients that supplied none).
func (c *Client) x25519() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.x25519Key
}

// noteReceived records inbound activity for the Client Info stats.
func (c *Client) noteReceived(payloadBytes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastActive = time.Now()
	c.bytesIn += int64(payloadBytes)
}

func (c *Client) lastActivity() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastActive
}

// noteSent records outbound payload bytes for the Client Info stats.
func (c *Client) noteSent(payloadBytes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bytesOut += int64(payloadBytes)
}

// notePingSent records that a server-initiated Ping was sent (for RTT
// matching).
func (c *Client) notePingSent() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastPingAt = time.Now()
}

// notePong measures the RTT against the last server Ping and folds it into
// the smoothed value.
func (c *Client) notePong() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastPingAt.IsZero() {
		return
	}
	sample := time.Since(c.lastPingAt).Nanoseconds()
	c.rttNs = ewmaRTT(c.rttNs, sample, c.rttKnown)
	c.rttKnown = true
}

// ewmaRTT folds a new RTT sample into the exponentially weighted moving
// average (alpha = 1/8). The first sample seeds the average.
func ewmaRTT(prev, sample int64, known bool) int64 {
	if !known {
		return sample
	}
	return prev*7/8 + sample/8
}

// clientStats returns a snapshot of the Client Info stats.
type clientStats struct {
	lastActive time.Time
	bytesIn    int64
	bytesOut   int64
	rttNs      int64
	rttKnown   bool
}

// stats returns a snapshot of the activity stats.
func (c *Client) stats() clientStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return clientStats{
		lastActive: c.lastActive,
		bytesIn:    c.bytesIn,
		bytesOut:   c.bytesOut,
		rttNs:      c.rttNs,
		rttKnown:   c.rttKnown,
	}
}

// setChallenge stores a pending auth challenge for the challenge-response
// handshake, along with the requested nickname (used for guest logins).
func (c *Client) setChallenge(uniqueID, nickname string, challenge []byte, expires time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.challengeUID = uniqueID
	c.challengeNick = nickname
	c.challenge = challenge
	c.challengeExp = expires
}

// takeChallenge returns and clears the pending challenge if it belongs to
// uniqueID and has not expired. An empty uniqueID matches any pending
// challenge (used by the guest public-key path, which does not know its
// unique ID in advance). It also returns the stored nickname.
func (c *Client) takeChallenge(uniqueID string) ([]byte, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.challenge == nil || time.Now().After(c.challengeExp) {
		return nil, "", false
	}
	if uniqueID != "" && c.challengeUID != uniqueID {
		return nil, "", false
	}
	ch := c.challenge
	nick := c.challengeNick
	c.challenge = nil
	c.challengeUID = ""
	c.challengeNick = ""
	c.challengeExp = time.Time{}
	return ch, nick, true
}

// TCPServer accepts and serves control-channel connections.
type TCPServer struct {
	cfg      *config.Config
	logger   *zap.Logger
	deps     *Deps
	configMu sync.RWMutex

	listener net.Listener

	// tlsFingerprint is the SHA-256 fingerprint of the control-channel
	// certificate (empty when TLS is disabled). Set in Start, or earlier by
	// UseTLSMaterial.
	tlsFingerprint string

	// tlsCert is the control-channel certificate when the caller supplied it
	// up front. The file-transfer port must present the SAME certificate, so
	// the binary generates it once and hands it to both rather than letting
	// two racing tlscert.Ensure calls mint two self-signed certs.
	tlsCert    *tls.Certificate
	tlsCertSet bool

	// chatKeys manages the per-scope chat encryption keys and their
	// persisted generations (91).
	chatKeys *chatKeyManager

	// rotPending coalesces channel key rotations inside
	// chat_key_rotate_min_seconds so a flapping client cannot mint one
	// persisted generation per reconnect.
	rotMu      sync.Mutex
	rotPending map[int64]bool

	// Chat infrastructure (wave 5a): rate limiter, spam tracker, slow-mode
	// tracker, and the memoised runtime moderation lists (117/118).
	chatRate    *chatRateLimiter
	chatSpam    *spamTracker
	chatSlow    *slowTracker
	typingRate  *typingTracker
	chatFilters *chatFilterCache

	permWriteMu sync.Mutex

	mu      sync.RWMutex
	clients map[string]*Client

	// startedAt feeds the public server-info uptime (313).
	startedAt time.Time

	stopOnce sync.Once
	stopCh   chan struct{}
}

// New constructs a TCPServer wired to the given backend dependencies. The
// listener is created lazily in Start so that construction never blocks and
// Shutdown is always safe to call. deps may be nil; handlers that need a
// missing dependency reply with an "unavailable" error.
func New(cfg *config.Config, logger *zap.Logger, deps *Deps) *TCPServer {
	var scopeKeys ScopeKeyStore
	var kek *chatcrypto.KEKRing
	if deps != nil {
		scopeKeys, kek = deps.ScopeKeys, deps.ChatKEK
	}
	s := &TCPServer{
		cfg:        cfg,
		logger:     logger,
		deps:       deps,
		clients:    make(map[string]*Client),
		stopCh:     make(chan struct{}),
		startedAt:  time.Now(),
		chatKeys:   newChatKeyManager(scopeKeys, kek, logger),
		rotPending: make(map[int64]bool),
		chatRate:   newChatRateLimiter(cfg.ChatRateMsgs, time.Duration(cfg.ChatRateWindowSeconds)*time.Second),
		chatSpam:   newSpamTracker(),
		chatSlow:   newSlowTracker(),
		typingRate: newTypingTracker(),

		chatFilters: &chatFilterCache{},
	}
	// Install the voice pipeline callbacks (talk/video permission gates,
	// speaking-state announcements, and renegotiation offer delivery).
	if deps != nil && deps.Voice != nil {
		deps.Voice.SetHandlers(s.canTalk, s.onSpeakingChanged)
		deps.Voice.SetVideoHandlers(s.canPublishVideo)
		deps.Voice.SetOfferSender(func(clientID, offerSDP string) error {
			client, ok := s.clientByID(clientID)
			if !ok {
				return errors.New("client not connected")
			}
			return s.writeMessage(client, netproto.MsgWebRTCOffer, netproto.WebRTCOffer{SDP: offerSDP})
		})
	}
	return s
}

// Start binds the TCP listener and serves connections until ctx is cancelled
// or Shutdown is called. It returns when the accept loop exits. When
// tls_enabled is set the listener is wrapped in TLS (self-signed cert from
// tls_dir or the configured cert/key files).
func (s *TCPServer) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.TCPAddr)
	if err != nil {
		return fmt.Errorf("tcp listen on %s: %w", s.cfg.TCPAddr, err)
	}
	if s.cfg.TLSEnabled {
		cert, fp := tls.Certificate{}, ""
		if s.tlsCertSet {
			cert, fp = *s.tlsCert, s.tlsFingerprint
		} else {
			c, f, err := tlscert.Ensure(s.cfg.TLSDir, s.cfg.TLSCertFile, s.cfg.TLSKeyFile,
				[]string{"localhost", s.cfg.ServerName})
			if err != nil {
				_ = ln.Close()
				return fmt.Errorf("preparing control-channel TLS: %w", err)
			}
			cert, fp = c, f
		}
		s.tlsFingerprint = fp
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
		s.logger.Info("TCP control listener started (TLS)",
			zap.String("addr", s.cfg.TCPAddr),
			zap.String("tls_fingerprint", fp),
		)
	} else {
		s.logger.Info("TCP control listener started (plaintext!)", zap.String("addr", s.cfg.TCPAddr))
	}
	s.listener = ln

	// Close the listener when the context is cancelled so Accept unblocks.
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Shutdown()
		case <-s.stopCh:
		}
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			// Expected during shutdown: listener closed.
			select {
			case <-s.stopCh:
				return nil
			default:
				return fmt.Errorf("tcp accept: %w", err)
			}
		}
		s.logger.Debug("TCP connection accepted", zap.String("remote", conn.RemoteAddr().String()))
		s.metricsSink().IncTCPConnections()
		go s.handleConn(ctx, conn)
	}
}

// TLSFingerprint returns the SHA-256 fingerprint of the control-channel
// certificate, or "" when TLS is disabled.
func (s *TCPServer) TLSFingerprint() string {
	return s.tlsFingerprint
}

// UseTLSMaterial installs the control-channel certificate instead of letting
// Start mint its own. The file-transfer port presents the same certificate,
// and two independent tlscert.Ensure calls on a fresh install would race to
// create two different self-signed certs. Call before Start.
func (s *TCPServer) UseTLSMaterial(cert tls.Certificate, fingerprint string) {
	s.tlsCert, s.tlsFingerprint, s.tlsCertSet = &cert, fingerprint, true
}

// EnsureGlobalScopeKey mints the global chat generation at boot. Global is a
// fixed, known scope, so minting it eagerly is safe and removes the only
// legitimate lazy mint from a hot path (91).
func (s *TCPServer) EnsureGlobalScopeKey(ctx context.Context) error {
	_, _, err := s.chatKeys.EnsureScope(ctx, globalChatScope)
	return err
}

// Shutdown stops accepting new connections and closes the listener. Existing
// per-connection goroutines will exit when their reads fail. It is safe to
// call multiple times.
func (s *TCPServer) Shutdown() error {
	var err error
	s.stopOnce.Do(func() {
		close(s.stopCh)
		if s.listener != nil {
			err = s.listener.Close()
		}
	})
	return err
}

// handleConn services a single client connection for its lifetime.
func (s *TCPServer) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	client := &Client{
		ID:   newClientID(),
		Conn: conn,
	}
	s.register(client)
	defer func() {
		s.unregister(client.ID)
		s.onDisconnect(client)
	}()

	// (217) max-clients enforcement: config max_clients, tightened by the
	// serveredit override. 0 = unlimited.
	if max := s.EffectiveMaxClients(ctx); max > 0 && s.clientCount() > max {
		_ = s.sendError(client, errCodeUnavailable, "server is full")
		return
	}

	s.logger.Info("client connected",
		zap.String("client_id", client.ID),
		zap.String("remote", conn.RemoteAddr().String()),
	)

	// Server-initiated keepalive: Ping every 15s; matching Pongs feed the
	// smoothed RTT reported in Client Info.
	pingStop := make(chan struct{})
	defer close(pingStop)
	go s.pingLoop(client, pingStop)

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		default:
		}

		frame, err := netproto.ReadFrame(conn)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				s.logger.Info("client disconnected",
					zap.String("client_id", client.ID),
					zap.String("remote", conn.RemoteAddr().String()),
				)
				return
			}
			s.logger.Warn("read frame error",
				zap.String("client_id", client.ID),
				zap.Error(err),
			)
			return
		}

		client.noteReceived(len(frame.Payload))

		if err := s.dispatch(ctx, client, frame); err != nil {
			s.logger.Warn("dispatch error",
				zap.String("client_id", client.ID),
				zap.String("msg_type", netproto.MessageType(frame.Type).String()),
				zap.Error(err),
			)
			return
		}
	}
}

// dispatch routes a frame to the appropriate handler based on its type. Any
// command other than Authenticate and Ping is rejected until the client has
// authenticated.
func (s *TCPServer) dispatch(ctx context.Context, client *Client, f *netproto.Frame) error {
	mt := netproto.MessageType(f.Type)

	if !client.isAuthed() && mt != netproto.MsgAuthenticate && mt != netproto.MsgAuthSignature && mt != netproto.MsgPing {
		return s.sendError(client, errCodeNotAuthenticated, "not authenticated")
	}
	if client.rulesBlocked() && !allowedWhileRulesPending[mt] {
		return s.sendError(client, errCodePermissionDenied, "accept the server rules before continuing")
	}

	switch mt {
	case netproto.MsgAuthenticate:
		return s.handleAuthenticate(ctx, client, f)
	case netproto.MsgAuthSignature:
		return s.handleAuthSignature(ctx, client, f)
	case netproto.MsgCreateChannel:
		return s.handleCreateChannel(ctx, client, f)
	case netproto.MsgChannelEdit:
		return s.handleChannelEdit(ctx, client, f)
	case netproto.MsgDeleteChannel:
		return s.handleDeleteChannel(ctx, client, f)
	case netproto.MsgJoinChannel:
		return s.handleJoinChannel(ctx, client, f)
	case netproto.MsgMoveClient:
		return s.handleMoveClient(ctx, client, f)
	case netproto.MsgKickClient:
		return s.handleKickClient(ctx, client, f)
	case netproto.MsgChatSend:
		return s.handleChatSend(ctx, client, f)
	case netproto.MsgWebRTCOffer:
		return s.handleWebRTCOffer(ctx, client, f)
	case netproto.MsgWebRTCAnswer:
		return s.handleWebRTCAnswer(ctx, client, f)
	case netproto.MsgICECandidate:
		return s.handleICECandidate(ctx, client, f)
	case netproto.MsgWhisperSet:
		return s.handleWhisperSet(ctx, client, f)
	case netproto.MsgPositionUpdate:
		return s.handlePositionUpdate(ctx, client, f)
	case netproto.MsgVideoQuality:
		return s.handleVideoQuality(ctx, client, f)
	case netproto.MsgPrioritySpeaker:
		return s.handlePrioritySpeaker(ctx, client, f)
	case netproto.MsgRecordingControl:
		return s.handleRecordingControl(ctx, client, f)
	case netproto.MsgFileTransferInit:
		return s.handleFileTransferInit(ctx, client, f)
	case netproto.MsgFileList:
		return s.handleFileList(ctx, client, f)
	case netproto.MsgFileDelete:
		return s.handleFileDelete(ctx, client, f)
	case netproto.MsgFileRename:
		return s.handleFileRename(ctx, client, f)
	case netproto.MsgFileVersions:
		return s.handleFileVersions(ctx, client, f)
	case netproto.MsgFileLink:
		return s.handleFileLink(ctx, client, f)
	case netproto.MsgServerIconSet:
		return s.handleServerIconSet(ctx, client, f)
	case netproto.MsgServerIconGet:
		return s.handleServerIconGet(ctx, client, f)
	case netproto.MsgServerBannerSet:
		return s.handleServerBannerSet(ctx, client, f)
	case netproto.MsgServerBannerGet:
		return s.handleServerBannerGet(ctx, client, f)
	case netproto.MsgSetStatus:
		return s.handleSetStatus(ctx, client, f)
	case netproto.MsgPoke:
		return s.handlePoke(ctx, client, f)
	case netproto.MsgServerInfoQuery:
		return s.handleServerInfoQuery(ctx, client, f)
	case netproto.MsgAvatarSet:
		return s.handleAvatarSet(ctx, client, f)
	case netproto.MsgAvatarGet:
		return s.handleAvatarGet(ctx, client, f)
	case netproto.MsgChannelIconSet:
		return s.handleChannelIconSet(ctx, client, f)
	case netproto.MsgChannelIconGet:
		return s.handleChannelIconGet(ctx, client, f)
	case netproto.MsgTokenUse:
		return s.handleTokenUse(ctx, client, f)
	case netproto.MsgComplaint:
		return s.handleComplaint(ctx, client, f)
	case netproto.MsgScreenShare:
		return s.handleScreenShare(ctx, client, f)
	case netproto.MsgPermissionsQuery:
		return s.handlePermissionsQuery(ctx, client, f)
	case netproto.MsgKeyPublish:
		return s.handleKeyPublish(ctx, client, f)
	case netproto.MsgKeyRequest:
		return s.handleKeyRequest(ctx, client, f)
	case netproto.MsgChatHistory:
		return s.handleChatHistory(ctx, client, f)
	case netproto.MsgChatEdit:
		return s.handleChatEdit(ctx, client, f)
	case netproto.MsgChatDelete:
		return s.handleChatDelete(ctx, client, f)
	case netproto.MsgChatPin:
		return s.handleChatPin(ctx, client, f)
	case netproto.MsgChatPins:
		return s.handleChatPins(ctx, client, f)
	case netproto.MsgChatKeyRequest:
		return s.handleChatKeyRequest(ctx, client, f)
	case netproto.MsgChatReact:
		return s.handleChatReact(ctx, client, f)
	case netproto.MsgChatFilterGet:
		return s.handleChatFilterGet(ctx, client, f)
	case netproto.MsgChatFilterSet:
		return s.handleChatFilterSet(ctx, client, f)
	case netproto.MsgTyping:
		return s.handleTyping(ctx, client, f)
	case netproto.MsgChatDelivered:
		return s.handleChatDelivered(ctx, client, f)
	case netproto.MsgChatRead:
		return s.handleChatRead(ctx, client, f)
	case netproto.MsgEmojiUpload:
		return s.handleEmojiUpload(ctx, client, f)
	case netproto.MsgEmojiList:
		return s.handleEmojiList(ctx, client, f)
	case netproto.MsgEmojiGet:
		return s.handleEmojiGet(ctx, client, f)
	case netproto.MsgEmojiDelete:
		return s.handleEmojiDelete(ctx, client, f)
	case netproto.MsgEmojiRename:
		return s.handleEmojiRename(ctx, client, f)
	case netproto.MsgServerConfigQuery:
		return s.handleServerConfigQuery(ctx, client, f)
	case netproto.MsgServerConfigSet:
		return s.handleServerConfigSet(ctx, client, f)
	case netproto.MsgPreKeyPublish:
		return s.handlePreKeyPublish(ctx, client, f)
	case netproto.MsgPreKeyQuery:
		return s.handlePreKeyQuery(ctx, client, f)
	case netproto.MsgClientInfoQuery:
		return s.handleClientInfoQuery(ctx, client, f)
	case netproto.MsgGroupList:
		return s.handleGroupList(ctx, client, f)
	case netproto.MsgGroupCreate:
		return s.handleGroupCreate(ctx, client, f)
	case netproto.MsgGroupRename:
		return s.handleGroupRename(ctx, client, f)
	case netproto.MsgGroupDelete:
		return s.handleGroupDelete(ctx, client, f)
	case netproto.MsgGroupAssign:
		return s.handleGroupAssign(ctx, client, f)
	case netproto.MsgGroupUnassign:
		return s.handleGroupUnassign(ctx, client, f)
	case netproto.MsgGroupIconSet:
		return s.handleGroupIconSet(ctx, client, f)
	case netproto.MsgPermSet:
		return s.handlePermSet(ctx, client, f)
	case netproto.MsgPermList:
		return s.handlePermList(ctx, client, f)
	case netproto.MsgPermUnset:
		return s.handlePermUnset(ctx, client, f)
	case netproto.MsgPermTemplateApply:
		return s.handlePermTemplateApply(ctx, client, f)
	case netproto.MsgPermTrace:
		return s.handlePermTrace(ctx, client, f)
	case netproto.MsgAuditLog:
		return s.handleAuditLog(ctx, client, f)
	case netproto.MsgGroupIconGet:
		return s.handleGroupIconGet(ctx, client, f)
	case netproto.MsgGroupMembers:
		return s.handleGroupMembers(ctx, client, f)
	case netproto.MsgBanList:
		return s.handleBanList(ctx, client, f)
	case netproto.MsgBanRemove:
		return s.handleBanRemove(ctx, client, f)
	case netproto.MsgGroupEdit:
		return s.handleGroupEdit(ctx, client, f)
	case netproto.MsgPermCopy:
		return s.handlePermCopy(ctx, client, f)
	case netproto.MsgComplaintList:
		return s.handleComplaintList(ctx, client, f)
	case netproto.MsgComplaintClear:
		return s.handleComplaintClear(ctx, client, f)
	case netproto.MsgTokenList:
		return s.handleTokenList(ctx, client, f)
	case netproto.MsgTokenAdd:
		return s.handleTokenAdd(ctx, client, f)
	case netproto.MsgTokenDelete:
		return s.handleTokenDelete(ctx, client, f)
	case netproto.MsgServerRulesAccept:
		return s.handleServerRulesAccept(ctx, client, f)
	case netproto.MsgChannelSubscribe:
		return s.handleChannelSubscribe(ctx, client, f)
	case netproto.MsgPing:
		return s.handlePing(ctx, client, f)
	case netproto.MsgPong:
		client.notePong()
		return nil
	default:
		s.logger.Warn("unknown message type",
			zap.String("client_id", client.ID),
			zap.Uint16("msg_type", f.Type),
		)
		return s.sendError(client, errCodeUnknown, fmt.Sprintf("unknown message type %d", f.Type))
	}
}

// allowedWhileRulesPending is deliberately small: central dispatch denies new
// message types by default until the client accepts the current rules.
var allowedWhileRulesPending = map[netproto.MessageType]bool{
	netproto.MsgServerRulesAccept: true,
	netproto.MsgPing:              true,
	netproto.MsgPong:              true,
}

// pingLoop sends a server-initiated Ping every 15s until stopped; matching
// Pongs feed the client's smoothed RTT.
func (s *TCPServer) pingLoop(client *Client, stop chan struct{}) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.configMu.RLock()
			timeout := s.cfg.ClientTimeoutSeconds
			s.configMu.RUnlock()
			last := client.lastActivity()
			if timeout > 0 && !last.IsZero() && time.Since(last) > time.Duration(timeout)*time.Second {
				_ = client.Conn.Close()
				return
			}
			client.notePingSent()
			if err := s.writeMessage(client, netproto.MsgPing, netproto.Ping{}); err != nil {
				return
			}
		}
	}
}

// onDisconnect performs the post-disconnect cleanup for an authenticated
// client: remove it from the state manager and the broadcaster, announce the
// departure to the remaining clients, and trigger the temp-channel cleanup
// check for the channel it left.
func (s *TCPServer) onDisconnect(client *Client) {
	if !client.isAuthed() || s.deps == nil {
		return
	}

	var channelID int64
	var wasInvisible bool
	if s.deps.State != nil {
		if sc, ok := s.deps.State.GetClient(client.ID); ok {
			channelID = sc.ChannelID
			wasInvisible = sc.Status == "invisible"
		}
		s.deps.State.RemoveClient(client.ID)
	}

	if s.deps.Broadcast != nil {
		s.deps.Broadcast.Unregister(client.ID)
		if wasInvisible {
			// Invisible users were never visible to non-admins; their leave
			// is admin-only too (381).
			s.broadcastToAdmins(eventUserLeft, userEvent{
				ClientID: client.ID,
				UniqueID: client.UniqueID,
				Nickname: client.Username,
			})
		} else {
			s.broadcastEvent(eventUserLeft, userEvent{
				ClientID: client.ID,
				UniqueID: client.UniqueID,
				Nickname: client.Username,
			})
		}
	}

	if channelID != 0 && s.deps.Channels != nil {
		s.deps.Channels.OnClientLeftChannel(channelID)
	}

	// Rotate the chat key of the channel the client left (4b).
	if channelID != 0 {
		s.rotateScopeKey(context.Background(), channelID)
	}

	// Tear down the client's voice session (peer connection, router state).
	if s.deps.Voice != nil {
		if err := s.deps.Voice.ClosePeer(client.ID); err != nil {
			s.logger.Warn("voice session teardown failed",
				zap.String("client_id", client.ID),
				zap.Error(err),
			)
		}
		s.metricsSink().SetWebRTCPeers(s.deps.Voice.PeerCount())
	}

	if s.deps.State != nil {
		s.metricsSink().SetClientsConnected(s.deps.State.ClientCount())
	}
}

// --- helpers ---------------------------------------------------------------

// metricsSink returns the configured metrics sink, or a no-op when none is
// wired.
func (s *TCPServer) metricsSink() metrics.Sink {
	if s.deps == nil || s.deps.Metrics == nil {
		return metrics.Noop{}
	}
	return s.deps.Metrics
}

// writeMessage encodes and writes a single message to the client connection.
// Writes are serialized per client so handler replies and broadcast events
// never interleave on the wire.
func (s *TCPServer) writeMessage(client *Client, mt netproto.MessageType, msg any) error {
	frame, err := netproto.Encode(mt, msg)
	if err != nil {
		return err
	}
	return s.writeFrame(client, frame)
}

// writeFrame writes a raw frame to the client under the per-client write lock.
func (s *TCPServer) writeFrame(client *Client, frame *netproto.Frame) error {
	client.wmu.Lock()
	defer client.wmu.Unlock()
	if err := netproto.WriteFrame(client.Conn, frame); err != nil {
		s.logger.Warn("write error",
			zap.String("client_id", client.ID),
			zap.String("msg_type", netproto.MessageType(frame.Type).String()),
			zap.Error(err),
		)
		return err
	}
	client.noteSent(len(frame.Payload))
	return nil
}

// sendError writes an Error frame to the client.
func (s *TCPServer) sendError(client *Client, code uint16, message string) error {
	return s.writeMessage(client, netproto.MsgError, netproto.Error{Code: code, Message: message})
}

// register adds a client to the registry.
func (s *TCPServer) register(c *Client) {
	s.mu.Lock()
	s.clients[c.ID] = c
	s.mu.Unlock()
}

// unregister removes a client from the registry.
func (s *TCPServer) unregister(id string) {
	s.mu.Lock()
	delete(s.clients, id)
	s.mu.Unlock()
}

// clientCount returns the number of registered connections (217).
func (s *TCPServer) clientCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.clients)
}

// clientByID returns the registered client with the given ID, if any.
func (s *TCPServer) clientByID(id string) (*Client, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.clients[id]
	return c, ok
}

// clientByUniqueID returns the registered, authenticated client with the
// given unique ID, if any.
func (s *TCPServer) clientByUniqueID(uniqueID string) (*Client, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.clients {
		if c.uniqueID() == uniqueID {
			return c, true
		}
	}
	return nil, false
}

// newClientID returns a short random hex identifier suitable for logging and
// registry keys. It is not a security-sensitive value.
func newClientID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fallback to a timestamp-based ID if crypto/rand fails.
		return fmt.Sprintf("c-%x", time.Now().UnixNano())
	}
	return "c-" + hex.EncodeToString(b[:])
}

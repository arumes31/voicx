// conn.go implements the voicx control-protocol connection manager for the
// client: dial, authenticate (password path), frame read loop, and event
// fan-out to the Wails frontend. All server state lives here; the frontend
// is a dumb UI fed by Wails runtime events.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"voicx/internal/auth"
	"voicx/internal/netproto"
	"voicx/internal/tlscert"
)

// eventSink receives backend events: the Wails runtime in production, a
// recorder in tests.
type eventSink interface {
	Emit(name string, payload any)
}

// wailsSink forwards events to the Wails runtime.
type wailsSink struct {
	ctx context.Context
}

// Emit implements eventSink.
func (s wailsSink) Emit(name string, payload any) {
	if s.ctx != nil {
		wailsRuntime.EventsEmit(s.ctx, name, payload)
	}
}

// connManager owns the control-channel connection and its read loop.
type connManager struct {
	sink eventSink

	// tabID identifies the owning server tab (281). Empty means the legacy
	// single-connection manager (tests, headless tools): events go out under
	// their plain names. Tabbed managers route through tabSink, which journals
	// state events for replay and also forwards active-tab events under their
	// plain names.
	tabID string

	mu       sync.Mutex
	conn     net.Conn
	addr     string // control address (tab info)
	clientID string
	uniqueID string
	nickname string
	isAdmin  bool
	isGuest  bool
	closed   bool
	// lastSnapshot/lastChannelList cache the latest state frames so a tab
	// switch can replay them (281).
	lastSnapshot    string
	lastChannelList string
	// lastSubscriptions is the newest authoritative subscription set (312).
	lastSubscriptions string
	// iceServers are the ICE servers delivered by the server in the
	// AuthResponse (nil = use client defaults).
	iceServers []netproto.ICEServer
	// motd is the server's message of the day from the AuthResponse
	// ("" when unset); surfaced in chat once per connect (133).
	motd string

	// id is the client's Ed25519 identity (nil = load lazily on connect;
	// tests inject a temp one).
	id *identity

	// E2EE chat (wave 4b): peer key cache + per-scope channel keys.
	pubKeys   *pubKeyCache
	scopeKeys *scopeKeyStore

	// debugFrames tees frame summaries to the "debug_frame" event (327 debug
	// console) when enabled.
	debugFrames bool

	// Transport security (wave 4a): TLS with TOFU fingerprint pinning.
	// allowPlaintext permits falling back to plaintext for dev servers.
	// knownServers nil disables verification (tests, tools).
	allowPlaintext bool
	knownServers   *knownServers
	// tlsUsed/fingerprint/newServer describe the current connection for the
	// UI ("connected via TLS, fingerprint …, first seen").
	tlsUsed     bool
	fingerprint string
	newServer   bool

	// pending maps message types to one-shot response waiters
	// (PermissionsQuery, WebRTCOffer).
	pending map[netproto.MessageType]chan *netproto.Frame
	// reqMu serializes request/response exchanges so concurrent same-type
	// requests (e.g. avatar fetches for several clients) cannot clobber each
	// other's waiter.
	reqMu sync.Mutex
}

func newConnManager(wailsCtx context.Context) *connManager {
	return &connManager{
		sink:      wailsSink{ctx: wailsCtx},
		pending:   make(map[netproto.MessageType]chan *netproto.Frame),
		pubKeys:   newPubKeyCache(),
		scopeKeys: newScopeKeyStore(),
	}
}

// appOf returns the App this manager belongs to, or nil for a test or
// headless manager. The tab sink is the only back-channel a connManager has,
// and read-state pushes (121) arrive on the read loop but have to land in the
// App's settings.
func (m *connManager) appOf() *App {
	if s, ok := m.sink.(tabSink); ok {
		return s.app
	}
	return nil
}

// identity returns the client's key pair, loading or generating it lazily.
func (m *connManager) identity() (*identity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.id != nil {
		return m.id, nil
	}
	id, err := loadOrCreateIdentity()
	if err != nil {
		return nil, err
	}
	m.id = id
	return id, nil
}

// dialTransport dials the control channel with TLS and verifies the
// certificate fingerprint against the TOFU store: first-seen servers are
// accepted and pinned; a changed fingerprint fails hard
// (errFingerprintMismatch). When the server does not speak TLS, it falls
// back to plaintext only if allowPlaintext is set.
func (m *connManager) dialTransport(addr string) (net.Conn, error) {
	tlsConf := &tls.Config{
		InsecureSkipVerify: true, // TOFU: verified via fingerprint pinning below
		MinVersion:         tls.VersionTLS12,
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, tlsConf)
	if err == nil {
		var fp string
		if pc := conn.ConnectionState().PeerCertificates; len(pc) > 0 {
			fp = tlscert.FingerprintDER(pc[0].Raw)
		}
		m.mu.Lock()
		m.tlsUsed = true
		m.fingerprint = fp
		m.newServer = false
		ks := m.knownServers
		m.mu.Unlock()

		if ks != nil && fp != "" {
			switch ks.verify(addr, fp) {
			case trustUnknown:
				if err := ks.trust(addr, fp); err != nil {
					_ = conn.Close()
					return nil, fmt.Errorf("pinning server fingerprint: %w", err)
				}
				m.mu.Lock()
				m.newServer = true
				m.mu.Unlock()
			case trustMismatch:
				_ = conn.Close()
				return nil, errFingerprintMismatch
			}
		}
		return conn, nil
	}
	tlsErr := err

	m.mu.Lock()
	allowPlain := m.allowPlaintext
	m.mu.Unlock()
	if !allowPlain {
		return nil, fmt.Errorf("server does not accept TLS: %v (plaintext dev server? enable allow_plaintext in settings)", tlsErr)
	}

	m.mu.Lock()
	m.tlsUsed = false
	m.fingerprint = ""
	m.newServer = false
	m.mu.Unlock()
	return net.DialTimeout("tcp", addr, 5*time.Second)
}

// securitySnapshot reports how the current connection is secured, for
// display in the UI (About / login flow).
func (m *connManager) securitySnapshot() (tlsUsed bool, fingerprint string, newServer bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tlsUsed, m.fingerprint, m.newServer
}

// connect dials and authenticates. With a password it is an account login
// (nickname or unique ID + password); without one it is a guest login using
// the client's own Ed25519 identity (key-derived unique ID). It returns ""
// on success or the failure reason.
func (m *connManager) connect(addr, nickname, password, serverPassword string) string {
	id, err := m.identity()
	if err != nil {
		return err.Error()
	}

	// The X25519 key rides along with auth so the server can seal the global
	// chat generation and the MOTD into the AuthResponse itself (133).
	encPub := m.x25519PublicB64()

	if password != "" {
		return m.connectWith(addr, netproto.Authenticate{
			Username:        nickname, // unique ID or nickname; the server resolves both
			Password:        password,
			ServerPassword:  serverPassword,
			PublicKey:       id.PublicKey,
			X25519PublicKey: encPub,
		}, nil)
	}

	// Guest login with the client's own identity (key-derived unique ID). The
	// Ed25519 key is supplied by the signer callback, not here.
	uid, err := id.uniqueID()
	if err != nil {
		return err.Error()
	}
	return m.connectWith(addr, netproto.Authenticate{
		Username:        uid,
		Anonymous:       true,
		Nickname:        nickname,
		ServerPassword:  serverPassword,
		X25519PublicKey: encPub,
	}, func(challenge []byte) ([]byte, string, error) {
		sig, err := auth.SignChallenge(id.PrivateKey, challenge)
		return sig, id.PublicKey, err
	})
}

// challengeSigner signs a server challenge and returns the signature and
// the public key to present in AuthSignature.
type challengeSigner func(challenge []byte) (signature []byte, publicKey string, err error)

// connectWith dials and authenticates using the given Authenticate message.
// When the server replies with a challenge and signer is non-nil, the
// challenge handshake is completed. It returns "" on success or the failure
// reason.
func (m *connManager) connectWith(addr string, authMsg netproto.Authenticate, signer challengeSigner) string {
	conn, err := m.dialTransport(addr)
	if err != nil {
		return err.Error()
	}

	if err := m.writeConn(conn, netproto.MsgAuthenticate, authMsg); err != nil {
		_ = conn.Close()
		return err.Error()
	}

	// Read until AuthResponse, completing a challenge round if the server
	// asks for one.
	var resp netproto.AuthResponse
	for {
		f, err := readOfType(conn, 5*time.Second, netproto.MsgAuthResponse, netproto.MsgAuthChallenge)
		if err != nil {
			_ = conn.Close()
			return err.Error()
		}
		if netproto.MessageType(f.Type) == netproto.MsgAuthChallenge {
			if signer == nil {
				_ = conn.Close()
				return "server requested a challenge but no identity is available"
			}
			var ch netproto.AuthChallenge
			if err := netproto.Decode(f, &ch); err != nil {
				_ = conn.Close()
				return err.Error()
			}
			sig, pub, err := signer(ch.Challenge)
			if err != nil {
				_ = conn.Close()
				return err.Error()
			}
			if err := m.writeConn(conn, netproto.MsgAuthSignature, netproto.AuthSignature{
				UniqueID:        authMsg.Username,
				PublicKey:       pub,
				Signature:       sig,
				X25519PublicKey: authMsg.X25519PublicKey,
			}); err != nil {
				_ = conn.Close()
				return err.Error()
			}
			continue
		}
		if err := netproto.Decode(f, &resp); err != nil {
			_ = conn.Close()
			return err.Error()
		}
		break
	}

	if !resp.OK {
		_ = conn.Close()
		if resp.Reason != "" {
			return resp.Reason
		}
		return "authentication failed"
	}

	// The global generation and the MOTD sealed under it are resolved BEFORE
	// this returns, so App.MOTD() stays a correct one-shot read with no event
	// and no re-render path (133). installCurrentKeys takes m.mu via
	// identity(), so it must run outside the state lock below.
	m.installCurrentKeys(0, resp.ChatKeys)
	motd := m.openMOTD(resp)

	m.mu.Lock()
	m.conn = conn
	m.addr = addr
	m.clientID = resp.ClientID
	m.uniqueID = resp.UniqueID
	m.nickname = resp.Nickname
	m.isAdmin = resp.IsAdmin
	m.isGuest = authMsg.Anonymous
	m.iceServers = resp.ICEServers
	m.motd = motd
	m.closed = false
	m.mu.Unlock()

	// Publish the E2EE public key; the server answers with sealed chat keys.
	// Failure is non-fatal (old server) but chat encryption will not work.
	if err := m.publishE2EKey(); err != nil {
		m.emit("servererror", "e2e key publish failed: "+err.Error())
	}

	// Publishing the E2EE key above triggers the server to answer with sealed
	// scope keys on the read loop below. Account reconnect and state
	// synchronization are driven by server broadcasts upon authentication.

	// recover is per-goroutine: the read loop needs its own guard (331).
	go guardCrash("readLoop", func() {
		defer func() {
			if r := recover(); r != nil {
				m.mu.Lock()
				owned := (m.conn == conn)
				intentional := m.closed
				if owned {
					m.disconnectLocked()
				}
				m.mu.Unlock()
				if owned {
					if !intentional {
						m.emit("disconnected", "")
					}
				} else {
					_ = conn.Close()
				}
				panic(r)
			}
		}()
		m.readLoop(conn)
	})
	return ""
}

// openMOTD unseals the AuthResponse MOTD with the global generation that came
// with it. A sealed MOTD the client cannot open resolves to "" — the raw
// ciphertext must never reach the banner.
func (m *connManager) openMOTD(resp netproto.AuthResponse) string {
	if !resp.MOTDEnc {
		return resp.MOTD
	}
	key, ok := m.scopeKeys.get(0, resp.MOTDKeyID)
	if !ok {
		return ""
	}
	plain, err := openScope(resp.MOTD, key)
	if err != nil {
		return ""
	}
	return plain
}

// disconnectLocked closes the connection while m.mu is held.
func (m *connManager) disconnectLocked() {
	m.iceServers = nil
	m.motd = ""
	// (312) subscriptions are per connection and the server forgets them on
	// disconnect, so keeping the cached set would show tabs that no longer
	// receive anything.
	m.lastSubscriptions = ""
	if m.conn != nil {
		m.closed = true
		_ = m.conn.Close()
		m.conn = nil
	}
}

// disconnect closes the connection, if any.
func (m *connManager) disconnect() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.disconnectLocked()
}

// iceServersSnapshot returns the ICE servers the server provided at connect
// (nil = use client defaults).
func (m *connManager) iceServersSnapshot() []netproto.ICEServer {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.iceServers
}

// motdSnapshot returns the server's message of the day delivered in the
// AuthResponse ("" when unset or offline).
func (m *connManager) motdSnapshot() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.motd
}

// clientIDSnapshot returns the server-assigned client ID ("" when not
// connected).
func (m *connManager) clientIDSnapshot() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.clientID
}

// isAdminSnapshot reports whether the authenticated user is a server admin.
func (m *connManager) isAdminSnapshot() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.isAdmin
}

// isGuestSnapshot reports whether the current session authenticated through
// the anonymous guest flow. Guest identities can be stable, so the unique ID
// alone is not a reliable way for the frontend to distinguish an account.
func (m *connManager) isGuestSnapshot() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.isGuest
}

// connected reports whether a live connection exists.
func (m *connManager) connected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conn != nil
}

// write encodes and writes a control message.
func (m *connManager) write(mt netproto.MessageType, msg any) error {
	m.mu.Lock()
	conn := m.conn
	m.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("not connected")
	}
	return m.writeConn(conn, mt, msg)
}

func (m *connManager) writeConn(conn net.Conn, mt netproto.MessageType, msg any) error {
	f, err := netproto.Encode(mt, msg)
	if err != nil {
		return err
	}
	m.teeFrame("out", f)
	return netproto.WriteFrame(conn, f)
}

// frameSummary is the debug console's per-frame record (327).
type frameSummary struct {
	Dir     string `json:"dir"` // "in" | "out"
	Type    string `json:"type"`
	Payload string `json:"payload"`
	At      int64  `json:"at"` // unix millis
}

// teeFrame emits a frame summary when the debug console is listening.
func (m *connManager) teeFrame(dir string, f *netproto.Frame) {
	m.mu.Lock()
	on := m.debugFrames
	m.mu.Unlock()
	if !on {
		return
	}
	payload := string(f.Payload)
	if len(payload) > 4000 {
		payload = payload[:4000] + "…"
	}
	m.emit("debug_frame", frameSummary{
		Dir:     dir,
		Type:    netproto.MessageType(f.Type).String(),
		Payload: payload,
		At:      time.Now().UnixMilli(),
	})
}

// request sends a message and waits for a typed response.
func (m *connManager) request(send, reply netproto.MessageType, msg any, timeout time.Duration) (*netproto.Frame, error) {
	m.reqMu.Lock()
	defer m.reqMu.Unlock()

	ch := make(chan *netproto.Frame, 1)
	m.mu.Lock()
	m.pending[reply] = ch
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.pending, reply)
		m.mu.Unlock()
	}()

	if err := m.write(send, msg); err != nil {
		return nil, err
	}
	select {
	case f := <-ch:
		if f.Type == uint16(netproto.MsgError) {
			var e netproto.Error
			if err := netproto.Decode(f, &e); err == nil {
				return nil, fmt.Errorf("%s", e.Message)
			}
			return nil, fmt.Errorf("server error")
		}
		return f, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout waiting for %s", reply)
	}
}

// readLoop dispatches incoming frames until the connection fails.
func (m *connManager) readLoop(conn net.Conn) {
	for {
		f, err := netproto.ReadFrame(conn)
		if err != nil {
			// Intentional closes (Disconnect/CloseTab) end the loop quietly;
			// only unexpected drops are reported.
			m.mu.Lock()
			intentional := m.closed
			m.mu.Unlock()
			if !intentional {
				m.emit("disconnected", "")
			}
			m.disconnect()
			return
		}
		m.dispatch(f)
	}
}

// dispatch routes one frame to pending waiters or frontend events.
func (m *connManager) dispatch(f *netproto.Frame) {
	mt := netproto.MessageType(f.Type)
	m.teeFrame("in", f)

	m.mu.Lock()
	waiter, ok := m.pending[mt]
	m.mu.Unlock()
	if ok {
		select {
		case waiter <- f:
		default:
		}
		return
	}

	switch mt {
	case netproto.MsgSnapshot:
		m.mu.Lock()
		m.lastSnapshot = string(f.Payload)
		m.mu.Unlock()
		m.emit("snapshot", string(f.Payload))
	case netproto.MsgEvent:
		// Sealed payloads (chat bodies, edits, announcements) are opened in
		// the backend; DMs decrypt asynchronously and re-emit, which is what
		// the empty return means.
		if out := m.maybeDecryptEvent(string(f.Payload)); out != "" {
			m.applySessionEvent(out)
			m.emit("event", out)
		}
	case netproto.MsgChannelKey:
		m.handleChannelKey(f)
	case netproto.MsgChatKeyBundle:
		// Answer to a pull (99/100). Archival generations only: they never
		// advance the send key, and a waiter in awaitScopeText wakes on the
		// install.
		m.handleChatKeyBundle(f)
	case netproto.MsgSubscriptionState:
		// (312) always the authoritative full set, so it is cached and
		// replayed verbatim on a tab switch like the other state frames.
		m.mu.Lock()
		m.lastSubscriptions = string(f.Payload)
		m.mu.Unlock()
		m.emit("subscriptions", string(f.Payload))
	case netproto.MsgServerRules:
		// (216) The webview owns the blocking prompt, but the frame stays a
		// typed backend event so operator text is never interpreted as markup.
		m.emit("server_rules", string(f.Payload))
	case netproto.MsgChannelList:
		m.mu.Lock()
		m.lastChannelList = string(f.Payload)
		m.mu.Unlock()
		m.emit("channellist", string(f.Payload))
	case netproto.MsgICECandidate:
		m.emit("ice", string(f.Payload))
	case netproto.MsgWebRTCOffer:
		m.emit("offer", string(f.Payload))
	case netproto.MsgAvatarData:
		m.emit("avatar", string(f.Payload))
	case netproto.MsgPermsInvalid:
		// (151) the server pushes this instead of the client re-resolving on a
		// timer; the reason distinguishes a cosmetics change from a grant change.
		var pi netproto.PermsInvalid
		if err := netproto.Decode(f, &pi); err == nil {
			m.emit("perms_invalid", pi.Reason)
		}
	case netproto.MsgError:
		m.mu.Lock()
		for _, waiter := range m.pending {
			select {
			case waiter <- f:
			default:
			}
		}
		m.mu.Unlock()

		var e netproto.Error
		if err := netproto.Decode(f, &e); err == nil {
			// Capability probes (121 read state) are sent speculatively, so an
			// older server answering "unknown message type" is an expected
			// negative, not something to show the user.
			if strings.Contains(e.Message, "unknown message type") {
				return
			}
			m.emit("servererror", fmt.Sprintf("%d: %s", e.Code, e.Message))
		}
	case netproto.MsgPong, netproto.MsgChatBroadcast, netproto.MsgAuthResponse:
		// Nothing to do.
	case netproto.MsgPing:
		// Answer server-initiated keepalive pings (feeds server-side RTT for
		// the Client Info dialog).
		_ = m.write(netproto.MsgPong, netproto.Pong{})
	default:
		// Unknown frame: ignore.
	}
}

// applySessionEvent keeps the bound session flags aligned with grants that
// take effect after authentication. In particular, a guest token redemption
// promotes the identity and an admin token changes IsAdmin immediately.
func (m *connManager) applySessionEvent(raw string) {
	var env struct {
		Type string `json:"type"`
		Data struct {
			GroupID  int64 `json:"group_id"`
			Promoted bool  `json:"promoted"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil || env.Type != "token_used" {
		return
	}
	m.mu.Lock()
	if env.Data.Promoted {
		m.isGuest = false
	}
	if env.Data.GroupID == 0 {
		m.isAdmin = true
	}
	m.mu.Unlock()
}

// emit sends a backend event to the sink.
func (m *connManager) emit(name string, payload any) {
	if m.sink != nil {
		m.sink.Emit(name, payload)
	}
}

// readOfType reads frames until one of the wanted types arrives or the
// deadline passes (used during the synchronous auth handshake).
func readOfType(conn net.Conn, timeout time.Duration, wanted ...netproto.MessageType) (*netproto.Frame, error) {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	for {
		f, err := netproto.ReadFrame(conn)
		if err != nil {
			return nil, err
		}
		for _, mt := range wanted {
			if netproto.MessageType(f.Type) == mt {
				return f, nil
			}
		}
	}
}

// decodeJSON is a small helper for the bound API.
func decodeJSON(f *netproto.Frame, v any) error {
	return json.Unmarshal(f.Payload, v)
}

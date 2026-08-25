// conn.go implements the voicx control-protocol connection manager for the
// client: dial, authenticate (password path), frame read loop, and event
// fan-out to the Wails frontend. All server state lives here; the frontend
// is a dumb UI fed by Wails runtime events.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
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

	mu        sync.Mutex
	writeMu   sync.Mutex
	conn      net.Conn
	connEpoch uint64
	addr      string // control address (tab info)
	clientID  string
	uniqueID  string
	nickname  string
	isAdmin   bool
	isGuest   bool
	closed    bool
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
	pubKeys    *pubKeyCache
	scopeKeys  *scopeKeyStore
	decryptSem chan struct{}

	// debugFrames tees frame summaries to the "debug_frame" event (327 debug
	// console) when enabled.
	debugFrames bool

	// Transport security (wave 4a): TLS with TOFU fingerprint pinning.
	// allowPlaintext permits falling back to plaintext for dev servers.
	// knownServers nil disables verification (tests, tools).
	allowPlaintext bool
	knownServers   *knownServers
	// tlsUsed/fingerprint/newServer describe the current connection for the
	// UI ("connected via TLS, fingerprint …, first seen"). The certificate
	// validity window is retained separately so the UI can warn about a local
	// clock that is too far outside it without weakening TOFU verification.
	tlsUsed             bool
	fingerprint         string
	peerCertificateDER  []byte
	newServer           bool
	certNotBefore       time.Time
	certNotAfter        time.Time
	certValidityTrusted bool

	// The wire protocol has no request ID, so same-reply-type requests remain
	// serialized. Newer servers attach Error.OriginType, letting an error for
	// a fire-and-forget command stay global instead of failing this request.
	// Typed replies are still protected by closing the exact transport on a
	// timeout, so a late reply can never satisfy the next request.
	requestGatesMu sync.Mutex
	requestGates   map[netproto.MessageType]*sync.Mutex
	pending        map[netproto.MessageType]pendingRequest
	// beforeDispatchLock is a test-only barrier used to prove that an already
	// parsed frame cannot race a replacement connection's waiter.
	beforeDispatchLock func()
	transfers          transferRegistry
	progress           transferProgressReporter
	// transferEpoch and acceptingTransfers bind data-port workers to one
	// authenticated control connection. A disconnect flips the gate before
	// transfer entries are detached, so a worker that finishes dialing cannot
	// register an orphan into a later reconnect.
	transferEpoch      uint64
	acceptingTransfers bool

	// The defaults keep an idle connected control channel alive while making a
	// peer that stops responding fail in bounded time. Tests may shorten them.
	heartbeatInterval time.Duration
	readTimeout       time.Duration
	writeTimeout      time.Duration
}

type requestResult struct {
	frame *netproto.Frame
	err   error
}

type pendingRequest struct {
	request netproto.MessageType
	reply   netproto.MessageType
	result  chan requestResult
}

const (
	defaultHeartbeatInterval = 15 * time.Second
	defaultReadTimeout       = 45 * time.Second
	defaultWriteTimeout      = 10 * time.Second
)

func newConnManager(wailsCtx context.Context) *connManager {
	return &connManager{
		sink:              wailsSink{ctx: wailsCtx},
		pending:           make(map[netproto.MessageType]pendingRequest),
		requestGates:      make(map[netproto.MessageType]*sync.Mutex),
		heartbeatInterval: defaultHeartbeatInterval,
		readTimeout:       defaultReadTimeout,
		writeTimeout:      defaultWriteTimeout,
		pubKeys:           newPubKeyCache(),
		scopeKeys:         newScopeKeyStore(),
		decryptSem:        make(chan struct{}, maxAsyncDecrypts),
	}
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
	m.mu.Lock()
	ks := m.knownServers
	// A failed redial must not leave certificate metadata from an earlier
	// transport available to diagnostics.
	m.tlsUsed = false
	m.fingerprint = ""
	m.peerCertificateDER = nil
	m.newServer = false
	m.certNotBefore = time.Time{}
	m.certNotAfter = time.Time{}
	m.certValidityTrusted = false
	m.mu.Unlock()
	canonicalAddr := ""
	if ks != nil {
		var err error
		canonicalAddr, err = normalizeServerAddr(addr)
		if err != nil {
			return nil, trustStoreUnavailable("normalize server address %q: %v", addr, err)
		}
	}
	var fingerprint string
	var peerCertificateDER []byte
	var firstSeen bool
	var certNotBefore time.Time
	var certNotAfter time.Time
	tlsConf := &tls.Config{
		// #nosec G402 -- VerifyConnection enforces the TOFU pin on every full or resumed handshake.
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return fmt.Errorf("server presented no TLS certificate")
			}
			leaf := state.PeerCertificates[0]
			fingerprint = tlscert.FingerprintDER(leaf.Raw)
			peerCertificateDER = append(peerCertificateDER[:0], leaf.Raw...)
			certNotBefore = leaf.NotBefore
			certNotAfter = leaf.NotAfter
			if ks == nil {
				return nil
			}
			status, err := ks.verify(canonicalAddr, fingerprint)
			if err != nil {
				return fmt.Errorf("TLS trust store unavailable: %w", err)
			}
			switch status {
			case trustUnknown:
				firstSeen = true
				return nil
			case trustMismatch:
				return errFingerprintMismatch
			default:
				return nil
			}
		},
	}
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 5 * time.Second},
		Config:    tlsConf,
	}
	conn, err := dialer.DialContext(context.Background(), "tcp", addr)
	if err == nil {
		if ks != nil && firstSeen {
			if err := ks.trust(canonicalAddr, fingerprint); err != nil {
				_ = conn.Close()
				return nil, fmt.Errorf("pinning server fingerprint: %w", err)
			}
		}
		m.mu.Lock()
		m.tlsUsed = true
		m.fingerprint = fingerprint
		m.peerCertificateDER = append([]byte(nil), peerCertificateDER...)
		m.newServer = firstSeen
		m.certNotBefore = certNotBefore
		m.certNotAfter = certNotAfter
		// Retain dates only when the certificate fingerprint was accepted by
		// the TOFU store. Pinning authenticates continuity, not the dates as a
		// time source; the dates are used only for a conditional advisory.
		m.certValidityTrusted = ks != nil
		m.mu.Unlock()
		return conn, nil
	}
	tlsErr := err
	if errors.Is(tlsErr, errFingerprintMismatch) {
		m.mu.Lock()
		m.tlsUsed = true
		m.fingerprint = fingerprint
		m.newServer = false
		m.certNotBefore = certNotBefore
		m.certNotAfter = certNotAfter
		m.certValidityTrusted = false
		m.mu.Unlock()
		return nil, errFingerprintMismatch
	}
	if errors.Is(tlsErr, errTrustStoreUnavailable) {
		return nil, tlsErr
	}

	m.mu.Lock()
	allowPlain := m.allowPlaintext
	m.mu.Unlock()
	if !allowPlain {
		return nil, fmt.Errorf("server does not accept TLS: %v (plaintext dev server? enable allow_plaintext in settings)", tlsErr)
	}

	m.mu.Lock()
	m.tlsUsed = false
	m.fingerprint = ""
	m.peerCertificateDER = nil
	m.newServer = false
	m.certNotBefore = time.Time{}
	m.certNotAfter = time.Time{}
	m.certValidityTrusted = false
	m.mu.Unlock()
	return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(context.Background(), "tcp", addr)
}

// securitySnapshot reports how the current connection is secured, for
// display in the UI (About / login flow).
func (m *connManager) securitySnapshot() (tlsUsed bool, fingerprint string, newServer bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tlsUsed, m.fingerprint, m.newServer
}

// certificateValiditySnapshot reports the peer certificate's validity window
// and whether the fingerprint-pinned transport authenticated that metadata.
func (m *connManager) certificateValiditySnapshot() (notBefore, notAfter time.Time, trusted bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.certNotBefore, m.certNotAfter, m.certValidityTrusted
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
	if m.pubKeys != nil {
		m.pubKeys.clear()
	}
	// Establishment writes happen before the transport becomes externally
	// installed. If the peer closes after auth but before KeyPublish, Connect
	// fails cleanly with no disconnected event and no reader/heartbeat loops.
	if err := m.publishE2EKeyConn(conn); err != nil {
		_ = conn.Close()
		return "e2e key publish failed: " + err.Error()
	}

	m.mu.Lock()
	m.conn = conn
	m.connEpoch++
	m.addr = addr
	m.clientID = resp.ClientID
	m.uniqueID = resp.UniqueID
	m.nickname = resp.Nickname
	m.isAdmin = resp.IsAdmin
	m.isGuest = authMsg.Anonymous
	m.iceServers = resp.ICEServers
	m.motd = motd
	m.closed = false
	m.transferEpoch++
	m.acceptingTransfers = true
	m.mu.Unlock()

	// The KeyPublish above triggers the server to answer with sealed
	// scope keys on the read loop below. Account reconnect and state
	// synchronization are driven by server broadcasts upon authentication.

	// recover is per-goroutine: the read loop needs its own guard (331).
	go guardCrash("readLoop", func() {
		defer func() {
			if r := recover(); r != nil {
				m.terminateConn(conn, true)
				panic(r)
			}
		}()
		m.readLoop(conn)
	})
	go guardCrash("heartbeat", func() { m.heartbeatLoop(conn) })
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

// detachLocked clears manager-owned connection state while m.mu is held. The
// caller owns closing the returned connection and notifying the returned
// waiters after unlocking.
func (m *connManager) detachLocked() (net.Conn, []chan requestResult, []net.Conn) {
	conn := m.conn
	waiters := make([]chan requestResult, 0, len(m.pending))
	for _, pending := range m.pending {
		waiters = append(waiters, pending.result)
	}
	clear(m.pending)
	m.iceServers = nil
	m.motd = ""
	m.tlsUsed = false
	m.fingerprint = ""
	m.peerCertificateDER = nil
	m.newServer = false
	m.certNotBefore = time.Time{}
	m.certNotAfter = time.Time{}
	m.certValidityTrusted = false
	// (312) subscriptions are per connection and the server forgets them on
	// disconnect, so keeping the cached set would show tabs that no longer
	// receive anything.
	m.lastSubscriptions = ""
	m.closed = true
	m.conn = nil
	m.connEpoch++
	m.acceptingTransfers = false
	m.transferEpoch++
	transferConns := m.detachTransfersLocked()
	return conn, waiters, transferConns
}

// disconnect closes the connection, if any.
func (m *connManager) disconnect() {
	m.mu.Lock()
	conn, waiters, transferConns := m.detachLocked()
	m.mu.Unlock()
	if m.pubKeys != nil {
		m.pubKeys.clear()
	}
	if conn != nil {
		_ = conn.Close()
	}
	m.notifyWaiters(waiters, requestResult{err: net.ErrClosed})
	closeTransfers(transferConns)
}

// terminateConn is the single unexpected-termination owner. It detaches only
// the active transport, wakes waiters and emits disconnected exactly once.
// A stale read/write loop can never clear or signal a replacement connection.
func (m *connManager) terminateConn(conn net.Conn, emitDisconnected bool) bool {
	m.mu.Lock()
	if m.conn != conn {
		m.mu.Unlock()
		return false
	}
	toClose, waiters, transferConns := m.detachLocked()
	m.mu.Unlock()
	if m.pubKeys != nil {
		m.pubKeys.clear()
	}
	if toClose != nil {
		_ = toClose.Close()
	}
	m.notifyWaiters(waiters, requestResult{err: net.ErrClosed})
	closeTransfers(transferConns)
	if emitDisconnected {
		m.emit("disconnected", "")
	}
	return true
}

func (m *connManager) notifyWaiters(waiters []chan requestResult, result requestResult) {
	for _, waiter := range waiters {
		select {
		case waiter <- result:
		default:
		}
	}
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
	m.mu.Lock()
	timeout := m.writeTimeout
	debugFrames := m.debugFrames
	m.mu.Unlock()

	m.writeMu.Lock()
	f, err := netproto.Encode(mt, msg)
	if err != nil {
		m.writeMu.Unlock()
		return err
	}
	if timeout > 0 {
		_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	}
	err = netproto.WriteFrame(conn, f)
	if timeout > 0 {
		_ = conn.SetWriteDeadline(time.Time{})
	}
	m.writeMu.Unlock()
	if err != nil {
		m.terminateConn(conn, true)
		return err
	}
	// Event sinks may re-enter write. Emit only after releasing writeMu and
	// only for a completed wire write.
	m.teeFrameIfEnabled("out", f, debugFrames)
	return err
}

func (m *connManager) readDeadline() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readTimeout
}

func (m *connManager) heartbeatPeriod() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.heartbeatInterval
}

func (m *connManager) heartbeatLoop(conn net.Conn) {
	period := m.heartbeatPeriod()
	if period <= 0 {
		return
	}
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for range ticker.C {
		m.mu.Lock()
		owned := m.conn == conn
		m.mu.Unlock()
		if !owned {
			return
		}
		if err := m.writeConn(conn, netproto.MsgPing, netproto.Ping{}); err != nil {
			return
		}
	}
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
	m.teeFrameIfEnabled(dir, f, on)
}

func (m *connManager) teeFrameIfEnabled(dir string, f *netproto.Frame, on bool) {
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
	gate := m.requestGate(reply)
	gate.Lock()
	defer gate.Unlock()

	ch := make(chan requestResult, 1)
	m.mu.Lock()
	conn := m.conn
	if conn == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("not connected")
	}
	if m.pending == nil {
		m.pending = make(map[netproto.MessageType]pendingRequest)
	}
	m.pending[reply] = pendingRequest{request: send, reply: reply, result: ch}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if pending, ok := m.pending[reply]; ok && pending.result == ch {
			delete(m.pending, reply)
		}
		m.mu.Unlock()
	}()

	if err := m.writeConn(conn, send, msg); err != nil {
		return nil, err
	}
	select {
	case result := <-ch:
		if result.err != nil {
			return nil, result.err
		}
		f := result.frame
		if f.Type == uint16(netproto.MsgError) {
			var e netproto.Error
			if err := netproto.Decode(f, &e); err == nil {
				return nil, fmt.Errorf("%s", e.Message)
			}
			return nil, fmt.Errorf("server error")
		}
		return f, nil
	case <-time.After(timeout):
		// There is no typed response correlation on the legacy frame format.
		// Closing this exact transport prevents its late response from being
		// delivered to a later request that expects the same reply type.
		m.terminateConn(conn, true)
		return nil, fmt.Errorf("timeout waiting for %s", reply)
	}
}

// requestGate serializes only requests that expect the same reply type. The
// wire protocol has no request ID, so those requests cannot be distinguished;
// unrelated replies may proceed concurrently.
func (m *connManager) requestGate(reply netproto.MessageType) *sync.Mutex {
	m.requestGatesMu.Lock()
	defer m.requestGatesMu.Unlock()
	if m.requestGates == nil {
		m.requestGates = make(map[netproto.MessageType]*sync.Mutex)
	}
	gate := m.requestGates[reply]
	if gate == nil {
		gate = &sync.Mutex{}
		m.requestGates[reply] = gate
	}
	return gate
}

// readLoop dispatches incoming frames until the connection fails.
func (m *connManager) readLoop(conn net.Conn) {
	m.mu.Lock()
	epoch := m.connEpoch
	m.mu.Unlock()
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	for {
		if timeout := m.readDeadline(); timeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(timeout))
		}
		f, err := netproto.ReadFrame(conn)
		if err != nil {
			// Intentional closes (Disconnect/CloseTab) end the loop quietly;
			// only unexpected drops are reported. An old loop may only detach
			// the exact transport it was started for.
			m.terminateConn(conn, true)
			return
		}
		m.dispatchFrom(conn, epoch, f)
	}
}

// dispatch is the test/direct-call wrapper. A live read loop uses
// dispatchFrom with its installed connection epoch.
func (m *connManager) dispatch(f *netproto.Frame) {
	// Unit tests and synthetic state replay intentionally inject a frame
	// without a transport. Live readers always use dispatchFrom below.
	m.dispatchAccepted(nil, 0, false, f)
}

// dispatchFrom drops an already-read frame unless its exact source connection
// and generation are still installed. This check happens before pending/UI
// effects, so an old read loop cannot complete a replacement's request.
func (m *connManager) dispatchFrom(conn net.Conn, epoch uint64, f *netproto.Frame) {
	m.dispatchAccepted(conn, epoch, true, f)
}

// dispatchAccepted validates a live frame's source and claims any matching
// waiter in one mutex critical section. In particular, an old read loop can
// never observe a valid connection, unlock, and then complete a waiter that a
// replacement connection installed in the gap.
func (m *connManager) dispatchAccepted(conn net.Conn, epoch uint64, checkSource bool, f *netproto.Frame) {
	mt := netproto.MessageType(f.Type)
	var protocolErr netproto.Error
	if mt == netproto.MsgError {
		if err := netproto.Decode(f, &protocolErr); err != nil {
			return
		}
	}
	if hook := m.beforeDispatchLock; hook != nil {
		hook()
	}

	m.mu.Lock()
	if checkSource && (conn == nil || m.conn != conn || m.connEpoch != epoch) {
		m.mu.Unlock()
		return
	}
	var waiter chan requestResult
	if mt != netproto.MsgError {
		if pending, ok := m.pending[mt]; ok {
			delete(m.pending, mt)
			waiter = pending.result
		}
	} else if protocolErr.OriginType != 0 {
		origin := netproto.MessageType(protocolErr.OriginType)
		for reply, pending := range m.pending {
			if pending.request == origin {
				delete(m.pending, reply)
				waiter = pending.result
				break
			}
		}
	}
	m.mu.Unlock()
	m.teeFrame("in", f)
	if waiter != nil {
		select {
		case waiter <- requestResult{frame: f}:
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
		// OriginType was absent from legacy Error frames. It is unsafe to
		// continue because an eventual uncorrelated reply could satisfy a
		// later request, so terminate this exact transport and wake all
		// current waiters immediately. The global error remains observable.
		if protocolErr.OriginType == 0 && checkSource {
			m.terminateConn(conn, true)
		}
		// Capability probes (121 read state) are sent speculatively, so an
		// older server answering "unknown message type" is an expected
		// negative, not something to show the user.
		if strings.Contains(protocolErr.Message, "unknown message type") {
			return
		}
		m.emit("servererror", fmt.Sprintf("%d: %s", protocolErr.Code, protocolErr.Message))
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

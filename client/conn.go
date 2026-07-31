// conn.go implements the voicx control-protocol connection manager for the
// client: dial, authenticate (password path), frame read loop, and event
// fan-out to the Wails frontend. All server state lives here; the frontend
// is a dumb UI fed by Wails runtime events.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"voicx/internal/auth"
	"voicx/internal/netproto"
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

	mu       sync.Mutex
	conn     net.Conn
	clientID string
	uniqueID string
	nickname string
	closed   bool

	// id is the client's Ed25519 identity (nil = load lazily on connect;
	// tests inject a temp one).
	id *identity

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
		sink:    wailsSink{ctx: wailsCtx},
		pending: make(map[netproto.MessageType]chan *netproto.Frame),
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

// connect dials and authenticates. With a password it is an account login
// (nickname or unique ID + password); without one it is a guest login using
// the client's own Ed25519 identity (key-derived unique ID). It returns ""
// on success or the failure reason.
func (m *connManager) connect(addr, nickname, password, serverPassword string) string {
	id, err := m.identity()
	if err != nil {
		return err.Error()
	}

	if password != "" {
		return m.connectWith(addr, netproto.Authenticate{
			Username:       nickname, // unique ID or nickname; the server resolves both
			Password:       password,
			ServerPassword: serverPassword,
			PublicKey:      id.PublicKey,
		}, nil)
	}

	// Guest login with the client's own identity (key-derived unique ID).
	uid, err := id.uniqueID()
	if err != nil {
		return err.Error()
	}
	return m.connectWith(addr, netproto.Authenticate{
		Username:       uid,
		Anonymous:      true,
		Nickname:       nickname,
		ServerPassword: serverPassword,
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
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
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
				UniqueID:  authMsg.Username,
				PublicKey: pub,
				Signature: sig,
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

	m.mu.Lock()
	m.conn = conn
	m.clientID = resp.ClientID
	m.uniqueID = resp.UniqueID
	m.nickname = resp.Nickname
	m.closed = false
	m.mu.Unlock()

	go m.readLoop(conn)
	return ""
}

// disconnect closes the connection, if any.
func (m *connManager) disconnect() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn != nil {
		m.closed = true
		_ = m.conn.Close()
		m.conn = nil
	}
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
	return netproto.WriteFrame(conn, f)
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
			m.emit("disconnected", "")
			m.disconnect()
			return
		}
		m.dispatch(f)
	}
}

// dispatch routes one frame to pending waiters or frontend events.
func (m *connManager) dispatch(f *netproto.Frame) {
	mt := netproto.MessageType(f.Type)

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
		m.emit("snapshot", string(f.Payload))
	case netproto.MsgEvent:
		m.emit("event", string(f.Payload))
	case netproto.MsgChannelList:
		m.emit("channellist", string(f.Payload))
	case netproto.MsgICECandidate:
		m.emit("ice", string(f.Payload))
	case netproto.MsgWebRTCOffer:
		m.emit("offer", string(f.Payload))
	case netproto.MsgAvatarData:
		m.emit("avatar", string(f.Payload))
	case netproto.MsgError:
		var e netproto.Error
		if err := netproto.Decode(f, &e); err == nil {
			m.emit("servererror", fmt.Sprintf("%d: %s", e.Code, e.Message))
		}
	case netproto.MsgPong, netproto.MsgChatBroadcast, netproto.MsgAuthResponse:
		// Nothing to do.
	default:
		// Unknown frame: ignore.
	}
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

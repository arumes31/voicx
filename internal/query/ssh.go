// ssh.go serves the same ServerQuery command loop over SSH (224). Only the
// transport differs: authentication uses the SSH user name as the unique ID
// and the SSH password as the query password, so the credentials and the
// admin-only rule are identical to the raw TCP port.
package query

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"

	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"
)

// SSHServer is the SSH front end for a ServerQuery Server. It shares the
// backend, the limits and the brute-force lockout state of the base server.
type SSHServer struct {
	base   *Server
	logger *zap.Logger

	// Addr is the listen address (e.g. ":10022").
	Addr string
	// HostKeyPath is where the persistent ed25519 host key lives; it is
	// generated on first start. A regenerated key makes every client warn
	// about a changed host identity, so it must survive restarts.
	HostKeyPath string

	listener net.Listener
	stopOnce sync.Once
	stopCh   chan struct{}
	wg       sync.WaitGroup

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

// NewSSH constructs an SSH transport in front of base.
func NewSSH(addr, hostKeyPath string, base *Server) *SSHServer {
	return &SSHServer{
		base:        base,
		logger:      base.logger,
		Addr:        addr,
		HostKeyPath: hostKeyPath,
		stopCh:      make(chan struct{}),
		conns:       make(map[net.Conn]struct{}),
	}
}

// Start binds the listener and serves SSH connections until ctx is cancelled
// or Close is called.
func (s *SSHServer) Start(ctx context.Context) error {
	signer, err := loadOrCreateHostKey(s.HostKeyPath)
	if err != nil {
		return fmt.Errorf("query ssh host key: %w", err)
	}
	cfg := &ssh.ServerConfig{
		ServerVersion:    "SSH-2.0-voicx_" + Version,
		MaxAuthTries:     s.base.MaxLoginFailures,
		PasswordCallback: s.passwordCallback(ctx),
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("query ssh listen on %s: %w", s.Addr, err)
	}
	s.listener = ln
	s.logger.Info("ServerQuery SSH listener started",
		zap.String("addr", s.Addr),
		zap.String("host_key_fingerprint", ssh.FingerprintSHA256(signer.PublicKey())))

	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-s.base.stopCh:
			_ = s.Close()
		case <-s.stopCh:
		}
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.stopCh:
				return nil
			default:
				return fmt.Errorf("query ssh accept: %w", err)
			}
		}
		if !s.base.registerConn(conn) {
			s.logger.Warn("query ssh connection refused: too many connections",
				zap.String("remote", conn.RemoteAddr().String()))
			_ = conn.Close()
			continue
		}
		s.track(conn, true)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.base.unregisterConn(conn)
			defer s.track(conn, false)
			s.serve(ctx, conn, cfg)
		}()
	}
}

// track adds or removes an accepted connection.
func (s *SSHServer) track(conn net.Conn, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if add {
		s.conns[conn] = struct{}{}
		return
	}
	delete(s.conns, conn)
}

// Close stops the listener and waits for the active sessions. The live
// connections are closed too: a session blocked reading a command would
// otherwise hold shutdown until its idle timeout.
func (s *SSHServer) Close() error {
	var err error
	s.stopOnce.Do(func() {
		close(s.stopCh)
		if s.listener != nil {
			err = s.listener.Close()
		}
		s.mu.Lock()
		for conn := range s.conns {
			_ = conn.Close()
		}
		s.mu.Unlock()
	})
	s.wg.Wait()
	return err
}

// passwordCallback authenticates an SSH login with the query credentials and
// feeds the same per-IP lockout counter as the TCP port.
func (s *SSHServer) passwordCallback(ctx context.Context) func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
	authFailed := errors.New("invalid loginname or password")
	return func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		ip := hostOnly(meta.RemoteAddr().String())
		if s.base.lockedOut(ip) {
			return nil, errors.New("too many failed logins, try again later")
		}
		ok, admin, err := s.base.backend.Authenticate(ctx, meta.User(), string(password))
		if err != nil {
			s.logger.Warn("query ssh login error", zap.Error(err))
			return nil, errors.New("internal error")
		}
		if !ok || !admin {
			// Non-admins are refused like bad credentials: ServerQuery is
			// admin-only and the distinction would confirm an account.
			s.base.recordLoginFailure(ip)
			return nil, authFailed
		}
		s.base.clearLoginFailures(ip)
		return nil, nil
	}
}

// serve runs one SSH connection: handshake, then one command loop per
// accepted session channel.
func (s *SSHServer) serve(ctx context.Context, nConn net.Conn, cfg *ssh.ServerConfig) {
	defer nConn.Close()

	sshConn, chans, globalReqs, err := ssh.NewServerConn(nConn, cfg)
	if err != nil {
		s.logger.Debug("query ssh handshake failed",
			zap.String("remote", nConn.RemoteAddr().String()), zap.Error(err))
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(globalReqs)

	s.logger.Info("query ssh client logged in",
		zap.String("unique_id", sshConn.User()),
		zap.String("remote", hostOnly(nConn.RemoteAddr().String())))

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		ch, reqs, err := newChan.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serveChannel(ctx, sshConn, nConn, ch, reqs)
		}()
	}
}

// serveChannel runs the command loop for one SSH session channel. It supports
// an interactive shell and a one-shot exec ("ssh host clientlist").
func (s *SSHServer) serveChannel(ctx context.Context, sshConn *ssh.ServerConn, nConn net.Conn, ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()

	sess := &session{
		r: bufio.NewReader(ch),
		w: ch,
		// The idle timeout lives on the transport connection: an SSH channel
		// has no deadline of its own.
		setReadDeadline: nConn.SetReadDeadline,
		remoteIP:        hostOnly(nConn.RemoteAddr().String()),
		authed:          true,
		username:        sshConn.User(),
	}

	for req := range reqs {
		switch req.Type {
		case "shell":
			_ = req.Reply(true, nil)
			_, _ = io.WriteString(ch, banner+"\n"+bannerHint+"\n")
			s.base.runSession(ctx, sess)
			s.exit(ch)
			return
		case "exec":
			_ = req.Reply(true, nil)
			s.base.execute(ctx, sess, execCommand(req.Payload))
			s.exit(ch)
			return
		case "pty-req", "env", "window-change":
			// Accepted and ignored: the protocol is line-based text.
			_ = req.Reply(true, nil)
		default:
			_ = req.Reply(false, nil)
		}
	}
}

// exit reports a clean exit status so the client does not report a broken
// session.
func (s *SSHServer) exit(ch ssh.Channel) {
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
}

// execCommand extracts the command string from an SSH "exec" request payload
// (a 4-byte length prefix followed by the command).
func execCommand(payload []byte) string {
	if len(payload) < 4 {
		return ""
	}
	n := binary.BigEndian.Uint32(payload)
	if int(n) > len(payload)-4 {
		return ""
	}
	return string(payload[4 : 4+n])
}

// hostOnly strips the port from a host:port address.
func hostOnly(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// loadOrCreateHostKey reads the ed25519 host key at path, generating and
// persisting one (0600) when it is missing.
func loadOrCreateHostKey(path string) (ssh.Signer, error) {
	if path == "" {
		return nil, errors.New("no host key path configured")
	}
	raw, err := os.ReadFile(path)
	if err == nil {
		return ssh.ParsePrivateKey(raw)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(encoded)
}

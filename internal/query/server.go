// server.go implements the ServerQuery TCP listener: accept loop, per-
// connection sessions, connection cap, login brute-force lockout, and
// graceful shutdown.
package query

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Version reported by the version command and the greeting banner.
const Version = "0.1.0"

const (
	// banner is sent on connect, one line.
	banner = "VOICX ServerQuery " + Version
	// bannerHint is the second greeting line.
	bannerHint = "type 'help' for a list of commands"
)

// Server is a ServerQuery listener.
type Server struct {
	backend Backend
	logger  *zap.Logger

	// Addr is the listen address (e.g. ":10012").
	Addr string
	// MaxConns caps concurrent query connections. Defaults to 10.
	MaxConns int
	// IdleTimeout closes connections that send no command for this long.
	// Defaults to 5 minutes.
	IdleTimeout time.Duration
	// MaxLineLength is the longest accepted command line. Defaults to 4096.
	MaxLineLength int
	// MaxLoginFailures is the number of failed logins from one IP before it
	// is locked out. Defaults to 5.
	MaxLoginFailures int
	// LockoutDuration is how long an IP stays locked out. Defaults to 5
	// minutes.
	LockoutDuration time.Duration

	listener net.Listener

	stopOnce sync.Once
	stopCh   chan struct{}
	wg       sync.WaitGroup

	mu         sync.Mutex
	conns      map[net.Conn]struct{}
	loginFails map[string]int
	lockouts   map[string]time.Time
}

// New constructs a Server listening on addr. limits left at zero get
// defaults; tests can set the exported fields before Start.
func New(addr string, logger *zap.Logger, backend Backend) *Server {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Server{
		Addr:             addr,
		MaxConns:         10,
		IdleTimeout:      5 * time.Minute,
		MaxLineLength:    4096,
		MaxLoginFailures: 5,
		LockoutDuration:  5 * time.Minute,
		backend:          backend,
		logger:           logger,
		stopCh:           make(chan struct{}),
		conns:            make(map[net.Conn]struct{}),
		loginFails:       make(map[string]int),
		lockouts:         make(map[string]time.Time),
	}
}

// Start binds the listener and serves connections until ctx is cancelled or
// Close is called. It returns when the accept loop exits.
func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("query listen on %s: %w", s.Addr, err)
	}
	s.listener = ln
	s.logger.Info("ServerQuery listener started", zap.String("addr", s.Addr))

	go func() {
		select {
		case <-ctx.Done():
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
				return fmt.Errorf("query accept: %w", err)
			}
		}

		if !s.registerConn(conn) {
			s.logger.Warn("query connection refused: too many connections",
				zap.String("remote", conn.RemoteAddr().String()))
			_, _ = io.WriteString(conn, errorLine(errTooManyConnections, "too many connections"))
			_ = conn.Close()
			continue
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serve(ctx, conn)
		}()
	}
}

// Close stops the listener and all active sessions. It is safe to call
// multiple times.
func (s *Server) Close() error {
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

// registerConn adds conn to the active set if below the cap.
func (s *Server) registerConn(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.conns) >= s.MaxConns {
		return false
	}
	s.conns[conn] = struct{}{}
	return true
}

// unregisterConn removes conn from the active set.
func (s *Server) unregisterConn(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, conn)
}

// serve handles one query connection for its lifetime.
func (s *Server) serve(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	defer s.unregisterConn(conn)

	s.logger.Debug("query connection accepted", zap.String("remote", conn.RemoteAddr().String()))

	if _, err := io.WriteString(conn, banner+"\n"+bannerHint+"\n"); err != nil {
		return
	}

	sess := &session{conn: conn, remoteIP: remoteIP(conn)}
	reader := bufio.NewReader(conn)

	for {
		_ = conn.SetReadDeadline(time.Now().Add(s.IdleTimeout))

		line, err := reader.ReadString('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				s.logger.Debug("query read error", zap.Error(err))
			}
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		if len(line) > s.MaxLineLength {
			if _, err := io.WriteString(conn, errorLine(errInvalidParameter, "line too long")); err != nil {
				return
			}
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		default:
		}

		if !s.execute(ctx, sess, line) {
			return
		}
	}
}

// remoteIP extracts the host part of a connection's remote address.
func remoteIP(conn net.Conn) string {
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return conn.RemoteAddr().String()
	}
	return host
}

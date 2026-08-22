package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"voicx/internal/config"
	"voicx/internal/netproto"
)

// testLogger returns a discard logger suitable for tests.
func testLogger() *zap.Logger {
	logger, _ := zap.NewDevelopment()
	return logger
}

// freePort returns a string ":0"-style address that the OS will resolve to an
// ephemeral free port. We use ":0" directly because net.Listen supports it.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// TestNewTCPServer verifies that New returns a non-nil TCPServer without
// starting it.
func TestNewTCPServer(t *testing.T) {
	cfg := &config.Config{TCPAddr: ":0"}
	s := New(cfg, testLogger(), nil)
	if s == nil {
		t.Fatal("New returned nil TCPServer")
	}
}

// TestTCPServerStartShutdownPingPong exercises the full lifecycle: start the
// server, connect a TCP client, send a Ping frame, expect a Pong frame back,
// then cancel the context and assert Shutdown returns cleanly.
func TestTCPServerStartShutdownPingPong(t *testing.T) {
	addr := freePort(t)
	cfg := &config.Config{TCPAddr: addr}
	s := New(cfg, testLogger(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startErr := make(chan error, 1)
	go func() {
		startErr <- s.Start(ctx)
	}()

	// Wait until the server is accepting connections by retrying dial.
	var conn net.Conn
	var dialErr error
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr = (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
		if dialErr == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if dialErr != nil {
		t.Fatalf("dial tcp server: %v", dialErr)
	}
	defer func() { _ = conn.Close() }()

	// Send a Ping frame.
	pingFrame, err := netproto.Encode(netproto.MsgPing, netproto.Ping{})
	if err != nil {
		t.Fatalf("encode ping: %v", err)
	}
	if err := netproto.WriteFrame(conn, pingFrame); err != nil {
		t.Fatalf("write ping: %v", err)
	}

	// Expect a Pong frame back.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := netproto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read pong: %v", err)
	}
	if netproto.MessageType(resp.Type) != netproto.MsgPong {
		t.Fatalf("response type = %s, want Pong", netproto.MessageType(resp.Type))
	}

	// Cancel context and assert Shutdown returns cleanly.
	cancel()
	if err := <-startErr; err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}
}

func TestTCPServerShutdownClosesIdleConnectionAndDrainsRegistry(t *testing.T) {
	srv, addr, startErr := startPlainTCPServer(t)
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	frame, err := netproto.Encode(netproto.MsgPing, netproto.Ping{})
	if err != nil {
		t.Fatalf("encode ping: %v", err)
	}
	if err := netproto.WriteFrame(conn, frame); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	if _, err := netproto.ReadFrame(conn); err != nil {
		t.Fatalf("read pong: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-startErr; err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("idle connection remained open after Shutdown")
	}
	srv.lifecycleMu.Lock()
	remaining := len(srv.acceptedConns)
	srv.lifecycleMu.Unlock()
	if remaining != 0 {
		t.Fatalf("accepted connection registry has %d entries after Shutdown", remaining)
	}
}

func TestTCPServerShutdownBeforeStartNeverPublishesListener(t *testing.T) {
	srv := New(&config.Config{TCPAddr: "127.0.0.1:0"}, testLogger(), nil)
	if err := srv.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown before Start: %v", err)
	}
	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("Start after Shutdown: %v", err)
	}
	select {
	case <-srv.started:
		t.Fatal("Start after Shutdown published a listener")
	default:
	}
}

func TestTCPServerShutdownDeadlineIsSticky(t *testing.T) {
	srv := New(&config.Config{TCPAddr: "127.0.0.1:0"}, zap.NewNop(), nil)
	conn := newBlockingTCPConn()
	srv.lifecycleMu.Lock()
	srv.lifecycleState = tcpLifecycleRunning
	srv.acceptedConns[conn] = struct{}{}
	srv.connWG.Add(1)
	srv.lifecycleMu.Unlock()
	go srv.serveConn(context.Background(), conn)
	select {
	case <-conn.readEntered:
	case <-time.After(time.Second):
		t.Fatal("connection handler did not begin its read")
	}

	shutdownCtx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	firstErr := srv.Shutdown(shutdownCtx)
	if !errors.Is(firstErr, context.DeadlineExceeded) {
		t.Fatalf("Shutdown deadline = %v, want DeadlineExceeded", firstErr)
	}
	close(conn.release)
	select {
	case <-srv.shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("terminal shutdown did not finish after connection release")
	}
	if laterErr := srv.Shutdown(context.Background()); laterErr != firstErr {
		t.Fatalf("later Shutdown = %v, want persisted %v", laterErr, firstErr)
	}
}

func TestTCPServerShutdownDuringTLSPreparationDoesNotPublish(t *testing.T) {
	srv := New(&config.Config{TCPAddr: "127.0.0.1:0", TLSEnabled: true}, zap.NewNop(), nil)
	prepareStarted := make(chan struct{})
	releasePrepare := make(chan struct{})
	listenCalled := make(chan struct{}, 1)
	srv.prepareTLS = func() (tls.Certificate, string, error) {
		close(prepareStarted)
		<-releasePrepare
		return tls.Certificate{}, "unpublished", nil
	}
	srv.listen = func(context.Context, string, string) (net.Listener, error) {
		listenCalled <- struct{}{}
		return nil, errors.New("listener should not be reached after shutdown")
	}
	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(context.Background()) }()
	select {
	case <-prepareStarted:
	case <-time.After(time.Second):
		t.Fatal("TLS preparation did not begin")
	}

	shutdownCtx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown during TLS preparation = %v, want DeadlineExceeded", err)
	}
	select {
	case <-srv.started:
		t.Fatal("shutdown during TLS preparation published a listener")
	default:
	}
	close(releasePrepare)
	if err := <-startErr; err != nil {
		t.Fatalf("Start after cancelled TLS preparation: %v", err)
	}
	select {
	case <-srv.shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("terminal shutdown did not wait for TLS preparation")
	}
	select {
	case <-listenCalled:
		t.Fatal("listener bound after shutdown cancelled TLS preparation")
	default:
	}
}

func TestTCPServerShutdownWaitsForAcceptedConnectionRejection(t *testing.T) {
	srv := New(&config.Config{TCPAddr: "127.0.0.1:0"}, zap.NewNop(), nil)
	listener := newPausedAcceptListener()
	srv.listen = func(context.Context, string, string) (net.Listener, error) { return listener, nil }
	afterAccept := make(chan struct{})
	releaseAccept := make(chan struct{})
	srv.afterAccept = func() {
		close(afterAccept)
		<-releaseAccept
	}
	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(context.Background()) }()
	select {
	case <-afterAccept:
	case <-time.After(time.Second):
		t.Fatal("accept loop did not pause after accepting a connection")
	}

	shutdownCtx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown while accept registration paused = %v, want DeadlineExceeded", err)
	}
	select {
	case <-srv.shutdownDone:
		t.Fatal("shutdown completed before the accepted connection was rejected")
	default:
	}
	close(releaseAccept)
	if err := <-startErr; err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-srv.shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after accept loop retired")
	}
	_ = listener.client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := listener.client.Read(make([]byte, 1)); err == nil {
		t.Fatal("accepted connection was not closed when shutdown won registration")
	}
}

func TestTCPServerRecoversHandlerPanicAndAcceptsNextConnection(t *testing.T) {
	core, observed := observer.New(zapcore.DebugLevel)
	srv := New(&config.Config{TCPAddr: "127.0.0.1:0"}, zap.New(core), nil)
	panicEntered := make(chan struct{})
	var calls atomic.Int32
	srv.beforeHandle = func() {
		if calls.Add(1) == 1 {
			close(panicEntered)
			panic("tcp-panic-secret")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(ctx) }()
	select {
	case <-srv.started:
	case <-time.After(time.Second):
		t.Fatal("TCP listener did not start")
	}
	srv.lifecycleMu.Lock()
	addr := srv.listener.Addr().String()
	srv.lifecycleMu.Unlock()

	first, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	defer func() { _ = first.Close() }()
	select {
	case <-panicEntered:
	case <-time.After(time.Second):
		t.Fatal("injected handler panic did not run")
	}
	_ = first.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := first.Read(make([]byte, 1)); err == nil {
		t.Fatal("panicking connection was not closed")
	}
	waitForTCPConnectionCleanup(t, srv)

	second, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("second dial: %v", err)
	}
	defer func() { _ = second.Close() }()
	ping, err := netproto.Encode(netproto.MsgPing, netproto.Ping{})
	if err != nil {
		t.Fatalf("encode ping: %v", err)
	}
	if err := netproto.WriteFrame(second, ping); err != nil {
		t.Fatalf("second write ping: %v", err)
	}
	if _, err := netproto.ReadFrame(second); err != nil {
		t.Fatalf("second connection did not receive pong: %v", err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(t.Context(), time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-startErr; err != nil {
		t.Fatalf("Start: %v", err)
	}
	entries := observed.FilterMessage("TCP connection handler panic").All()
	if len(entries) != 1 {
		t.Fatalf("panic log count = %d, want 1", len(entries))
	}
	fields := entries[0].ContextMap()
	if got := fields["panic_type"]; got != "string" {
		t.Fatalf("panic type = %#v, want string", got)
	}
	if stack, ok := fields["stack"].(string); !ok || stack == "" {
		t.Fatalf("panic log is missing stack: %#v", fields)
	}
	if strings.Contains(entries[0].Message+fmt.Sprint(fields), "tcp-panic-secret") {
		t.Fatalf("panic log leaked recovered value: %#v", fields)
	}
}

func waitForTCPConnectionCleanup(t *testing.T, srv *TCPServer) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		srv.lifecycleMu.Lock()
		accepted := len(srv.acceptedConns)
		srv.lifecycleMu.Unlock()
		if srv.clientCount() == 0 && accepted == 0 {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("connection cleanup incomplete: clients=%d accepted=%d", srv.clientCount(), accepted)
		case <-ticker.C:
		}
	}
}

func startPlainTCPServer(t *testing.T) (*TCPServer, string, <-chan error) {
	t.Helper()
	srv := New(&config.Config{TCPAddr: "127.0.0.1:0"}, testLogger(), nil)
	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(context.Background()) }()
	select {
	case <-srv.started:
	case <-time.After(time.Second):
		t.Fatal("TCP listener did not start")
	}
	srv.lifecycleMu.Lock()
	addr := srv.listener.Addr().String()
	srv.lifecycleMu.Unlock()
	return srv, addr, startErr
}

type blockingTCPConn struct {
	readEntered chan struct{}
	release     chan struct{}
	closed      chan struct{}
	readOnce    sync.Once
	closeOnce   sync.Once
}

func newBlockingTCPConn() *blockingTCPConn {
	return &blockingTCPConn{
		readEntered: make(chan struct{}),
		release:     make(chan struct{}),
		closed:      make(chan struct{}),
	}
}

func (c *blockingTCPConn) Read([]byte) (int, error) {
	c.readOnce.Do(func() { close(c.readEntered) })
	<-c.release
	return 0, io.EOF
}

func (c *blockingTCPConn) Write(p []byte) (int, error) { return len(p), nil }

func (c *blockingTCPConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *blockingTCPConn) LocalAddr() net.Addr              { return tcpTestAddr("local") }
func (c *blockingTCPConn) RemoteAddr() net.Addr             { return tcpTestAddr("remote") }
func (c *blockingTCPConn) SetDeadline(time.Time) error      { return nil }
func (c *blockingTCPConn) SetReadDeadline(time.Time) error  { return nil }
func (c *blockingTCPConn) SetWriteDeadline(time.Time) error { return nil }

type tcpTestAddr string

func (a tcpTestAddr) Network() string { return "tcp" }
func (a tcpTestAddr) String() string  { return string(a) }

type pausedAcceptListener struct {
	server net.Conn
	client net.Conn
	closed chan struct{}
	mu     sync.Mutex
	used   bool
}

func newPausedAcceptListener() *pausedAcceptListener {
	server, client := net.Pipe()
	return &pausedAcceptListener{server: server, client: client, closed: make(chan struct{})}
}

func (l *pausedAcceptListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.used {
		l.used = true
		l.mu.Unlock()
		return l.server, nil
	}
	l.mu.Unlock()
	<-l.closed
	return nil, net.ErrClosed
}

func (l *pausedAcceptListener) Close() error {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	return nil
}

func (l *pausedAcceptListener) Addr() net.Addr { return tcpTestAddr("paused-listener") }

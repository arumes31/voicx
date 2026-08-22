package grpcserver

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"voicx/internal/eventbus"
	"voicx/internal/query"
	voicxv1 "voicx/v1"
)

func TestServerExternalShutdownRetiresCancellationWatcher(t *testing.T) {
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	srv, _, cancel, errCh := startGRPCServer(t, &stubBackend{}, bus, nil)
	defer cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Start: %v", err)
	}

	srv.watchMu.Lock()
	watchDone := srv.watchDone
	srv.watchMu.Unlock()
	select {
	case <-watchDone:
	case <-time.After(time.Second):
		t.Fatal("cancellation watcher did not retire after external shutdown")
	}
}

func TestServerShutdownTimeoutDefaultsToThirtySeconds(t *testing.T) {
	srv := New("127.0.0.1:0", nil, nil, zap.NewNop(), nil)
	if got, want := srv.ShutdownTimeout, 30*time.Second; got != want {
		t.Fatalf("ShutdownTimeout = %s, want %s", got, want)
	}

	for _, timeout := range []time.Duration{0, -time.Second} {
		srv.ShutdownTimeout = timeout
		if got, want := srv.shutdownTimeout(), 30*time.Second; got != want {
			t.Fatalf("shutdownTimeout() with %s = %s, want %s", timeout, got, want)
		}
	}
}

func TestServerStartContextCancellationShutsDownWithoutRPCs(t *testing.T) {
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	srv, _, cancel, errCh := startGRPCServer(t, &stubBackend{}, bus, nil)

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not return after cancellation")
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown after cancellation: %v", err)
	}
}

func TestServerShutdownGracefullyDrainsUnaryAndRefusesNewRPCs(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer func() { releaseOnce.Do(func() { close(release) }) }()
	var calls atomic.Int32
	backend := &stubBackend{listChannelsFn: func(context.Context) []query.ChannelInfo {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return nil
	}}
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	srv, addr, cancel, errCh := startGRPCServer(t, backend, bus, nil)
	defer cancel()

	client := voicxv1.NewControlClient(dialGRPC(t, addr))
	firstDone := make(chan error, 1)
	go func() {
		_, err := client.ListChannels(authCtx(t, "admin-uid", "pw"), &voicxv1.ListChannelsRequest{})
		firstDone <- err
	}()
	<-entered

	shutdownDone := make(chan error, 1)
	go func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
		defer shutdownCancel()
		shutdownDone <- srv.Shutdown(shutdownCtx)
	}()
	waitForGRPCListenerClosed(t, addr)

	_, err := client.ListChannels(authCtx(t, "admin-uid", "pw"), &voicxv1.ListChannelsRequest{})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("new RPC during graceful shutdown = %v, want Unavailable", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("backend calls during graceful shutdown = %d, want 1", got)
	}

	releaseOnce.Do(func() { close(release) })
	if err := <-firstDone; err != nil {
		t.Fatalf("draining RPC: %v", err)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Start: %v", err)
	}
}

func TestServerShutdownForcesOpenEventStreamAtDeadline(t *testing.T) {
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	srv, addr, cancel, errCh := startGRPCServer(t, &stubBackend{}, bus, nil)
	defer cancel()

	stream, err := voicxv1.NewEventsClient(dialGRPC(t, addr)).Subscribe(
		authCtx(t, "admin-uid", "pw"), &voicxv1.SubscribeEventsRequest{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	waitForSubscribers(t, bus, 1)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown with open stream = %v, want DeadlineExceeded", err)
	}
	if _, err := stream.Recv(); err == nil {
		t.Fatal("open stream remained active after forced shutdown")
	}
	waitForSubscribers(t, bus, 0)
	if err := <-errCh; err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := srv.Shutdown(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("later Shutdown = %v, want persisted DeadlineExceeded", err)
	}
}

func TestServerShutdownBeforeStartSkipsOccupiedListener(t *testing.T) {
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	occupied, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy listener: %v", err)
	}
	defer func() { _ = occupied.Close() }()
	core, observed := observer.New(zap.InfoLevel)
	logger := zap.New(core)
	backend := &stubBackend{}
	limiter := query.New("127.0.0.1:0", logger, backend)
	srv := New(occupied.Addr().String(), backend, bus, logger, limiter)
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown before Start: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start after Shutdown: %v", err)
	}
	for _, entry := range observed.All() {
		if entry.Message == "gRPC listener started" {
			t.Fatal("Start logged a listener after completed pre-start shutdown")
		}
	}
}

func TestServerStartReturnsStoredTerminalShutdownErrorBeforeListen(t *testing.T) {
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	occupied, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy listener: %v", err)
	}
	defer func() { _ = occupied.Close() }()
	backend := &stubBackend{}
	srv := New(occupied.Addr().String(), backend, bus, zap.NewNop(), query.New("127.0.0.1:0", zap.NewNop(), backend))
	srv.shutdownOnce.Do(func() {})
	srv.setShutdownError(context.DeadlineExceeded)
	close(srv.shutdownDone)
	if err := srv.Start(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start terminal result = %v, want DeadlineExceeded", err)
	}
}

func TestServerConcurrentShutdownAndStartListenRaceIsNormal(t *testing.T) {
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	srv, _ := newGRPCServer(t, &stubBackend{}, bus, nil)
	enteredListen := make(chan struct{})
	releaseListen := make(chan struct{})
	srv.listen = func(network, address string) (net.Listener, error) {
		close(enteredListen)
		<-releaseListen
		return (&net.ListenConfig{}).Listen(t.Context(), network, address)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(context.Background()) }()
	<-enteredListen
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown during Start: %v", err)
	}
	close(releaseListen)
	if err := <-errCh; err != nil {
		t.Fatalf("Start after concurrent Shutdown: %v", err)
	}
}

var errFaultingListener = errors.New("faulting listener accept")

type faultingListener struct {
	net.Listener
	failAfterFirst <-chan struct{}
	accepted       atomic.Bool
}

func (l *faultingListener) Accept() (net.Conn, error) {
	if l.accepted.CompareAndSwap(false, true) {
		return l.Listener.Accept()
	}
	<-l.failAfterFirst
	return nil, errFaultingListener
}

func TestServerFatalServeErrorShutsDownExistingTransport(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()
	failAccept := make(chan struct{})
	faulting := &faultingListener{Listener: listener, failAfterFirst: failAccept}
	entered := make(chan struct{})
	backendDone := make(chan error, 1)
	var calls atomic.Int32
	backend := &stubBackend{listChannelsFn: func(ctx context.Context) []query.ChannelInfo {
		if calls.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
			backendDone <- ctx.Err()
		}
		return nil
	}}
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	limiter := query.New("127.0.0.1:0", zap.NewNop(), backend)
	srv := New(listener.Addr().String(), backend, bus, zap.NewNop(), limiter)
	srv.ShutdownTimeout = 100 * time.Millisecond
	srv.listen = func(string, string) (net.Listener, error) { return faulting, nil }
	startCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(startCtx) }()

	client := voicxv1.NewControlClient(dialGRPC(t, listener.Addr().String()))
	firstDone := make(chan error, 1)
	firstCtx := authCtx(t, "admin-uid", "pw")
	go func() {
		_, err := client.ListChannels(firstCtx, &voicxv1.ListChannelsRequest{})
		firstDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("existing transport did not reach the backend")
	}
	close(failAccept)

	deadline := time.Now().Add(time.Second)
	for !srv.draining.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !srv.draining.Load() {
		t.Fatal("fatal Serve error did not initiate shutdown")
	}
	_, newErr := client.ListChannels(authCtx(t, "admin-uid", "pw"), &voicxv1.ListChannelsRequest{})
	if status.Code(newErr) != codes.Unavailable {
		t.Fatalf("new RPC after fatal Serve error = %v, want Unavailable", newErr)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("backend calls after fatal Serve error = %d, want 1", got)
	}

	var startErr error
	select {
	case startErr = <-errCh:
	case <-time.After(time.Second):
		t.Fatal("Start did not return after fatal Serve error")
	}
	if !errors.Is(startErr, errFaultingListener) || !errors.Is(startErr, context.DeadlineExceeded) {
		t.Fatalf("Start fatal error = %v, want joined listener and shutdown deadline errors", startErr)
	}
	select {
	case err := <-firstDone:
		if err == nil {
			t.Fatal("active transport remained open after fatal Serve shutdown")
		}
	case <-time.After(time.Second):
		t.Fatal("active transport did not close after fatal Serve shutdown")
	}
	select {
	case <-backendDone:
	case <-time.After(time.Second):
		t.Fatal("active backend context was not cancelled")
	}
	srv.watchMu.Lock()
	watchDone := srv.watchDone
	srv.watchMu.Unlock()
	select {
	case <-watchDone:
	case <-time.After(time.Second):
		t.Fatal("watcher did not exit after fatal Serve error")
	}
}

func TestServerConcurrentShutdownAndContextCancellation(t *testing.T) {
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	srv, _, cancel, errCh := startGRPCServer(t, &stubBackend{}, bus, nil)

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
			defer shutdownCancel()
			errs <- srv.Shutdown(shutdownCtx)
		}()
	}
	cancel()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Shutdown: %v", err)
		}
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Start: %v", err)
	}
}

func waitForGRPCListenerClosed(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		conn, err := (&net.Dialer{Timeout: 20 * time.Millisecond}).DialContext(t.Context(), "tcp", addr)
		if err != nil {
			return
		}
		_ = conn.Close()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("gRPC listener %s remained open during shutdown", addr)
}

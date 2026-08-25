package filetransfer

import (
	"context"
	"net"
	"testing"
	"time"
)

func startLimitedServer(t *testing.T, maxConnections int) (string, *Server, <-chan error) {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve listener: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()

	s := New(Config{Addr: addr, RootDir: t.TempDir(), MaxConnections: maxConnections}, newFakeFileStore(), nil)
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(context.Background()) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := (&net.Dialer{Timeout: 50 * time.Millisecond}).DialContext(t.Context(), "tcp", addr)
		if err == nil {
			// Keep the probe open until the server has registered it. Closing it
			// first lets this helper observe zero before Accept/admit runs, so the
			// probe can later consume the only test slot and make the next dial
			// nondeterministically look like an overflow connection.
			waitForAcceptedConnections(t, s, 1)
			_ = conn.Close()
			waitForAcceptedConnections(t, s, 0)
			return addr, s, errCh
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = s.Close()
	<-errCh
	t.Fatal("file-transfer listener did not start")
	return "", nil, nil
}

func waitForAcceptedConnections(t *testing.T, s *Server, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.lifecycleMu.Lock()
		got := len(s.accepted)
		s.lifecycleMu.Unlock()
		if got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.lifecycleMu.Lock()
	got := len(s.accepted)
	s.lifecycleMu.Unlock()
	t.Fatalf("accepted connections = %d, want %d", got, want)
}

func TestServerConnectionCapRejectsOverflowPromptly(t *testing.T) {
	addr, s, errCh := startLimitedServer(t, 1)
	first, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	defer func() { _ = first.Close() }()
	waitForAcceptedConnections(t, s, 1)

	second, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("second dial: %v", err)
	}
	defer func() { _ = second.Close() }()
	_ = second.SetReadDeadline(time.Now().Add(time.Second))
	var byteBuffer [1]byte
	if _, err := second.Read(byteBuffer[:]); err == nil {
		t.Fatal("overflow connection remained open")
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Start: %v", err)
	}
}

func TestServerCloseClosesAcceptedConnectionsAndWaits(t *testing.T) {
	addr, s, errCh := startLimitedServer(t, 1)
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	waitForAcceptedConnections(t, s, 1)

	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var byteBuffer [1]byte
	if _, err := conn.Read(byteBuffer[:]); err == nil {
		t.Fatal("accepted connection was not closed")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close deadlocked on accepted connection")
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Start: %v", err)
	}
}

func TestServerDefaultConnectionCap(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", RootDir: t.TempDir()}, newFakeFileStore(), nil)
	if s.cfg.MaxConnections != 128 || cap(s.connSlots) != 128 {
		t.Fatalf("default connection cap = %d/%d, want 128", s.cfg.MaxConnections, cap(s.connSlots))
	}
}

package main

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"voicx/internal/netproto"
)

type fragmentingConn struct{ net.Conn }

type recordingSink struct {
	mu    sync.Mutex
	names []string
}

func (s *recordingSink) Emit(name string, _ any) {
	s.mu.Lock()
	s.names = append(s.names, name)
	s.mu.Unlock()
}

func (s *recordingSink) count(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, got := range s.names {
		if got == name {
			count++
		}
	}
	return count
}

func newTestConnManager() *connManager {
	cm := newConnManager(context.Background())
	cm.sink = nil
	return cm
}

func (c fragmentingConn) Write(p []byte) (int, error) {
	for _, b := range p {
		if _, err := c.Conn.Write([]byte{b}); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func TestConnWritesSerializeWholeFrames(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	cm := newTestConnManager()
	cm.writeTimeout = time.Second
	cm.mu.Lock()
	cm.conn = fragmentingConn{client}
	cm.mu.Unlock()

	readDone := make(chan error, 1)
	go func() {
		for range 100 {
			frame, err := netproto.ReadFrame(server)
			if err != nil {
				readDone <- err
				return
			}
			if netproto.MessageType(frame.Type) != netproto.MsgPing {
				readDone <- &unexpectedFrameError{got: netproto.MessageType(frame.Type)}
				return
			}
		}
		readDone <- nil
	}()

	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- cm.write(netproto.MsgPing, netproto.Ping{})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent write: %v", err)
		}
	}
	if err := <-readDone; err != nil {
		t.Fatalf("fragmented frames: %v", err)
	}
}

func TestWriteTimeoutTerminatesInstalledConnectionExactlyOnce(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	cm := newTestConnManager()
	sink := &recordingSink{}
	cm.sink = sink
	cm.writeTimeout = 10 * time.Millisecond
	cm.mu.Lock()
	cm.conn = client
	cm.mu.Unlock()
	if err := cm.write(netproto.MsgPing, netproto.Ping{}); err == nil {
		t.Fatal("blocked write unexpectedly succeeded")
	}
	if got := sink.count("disconnected"); got != 1 {
		t.Fatalf("disconnected events = %d, want 1", got)
	}
	cm.mu.Lock()
	installed := cm.conn
	cm.mu.Unlock()
	if installed != nil {
		t.Fatal("timed-out write left connection installed")
	}
}

func TestEstablishmentWriteFailureDoesNotEmitDisconnected(t *testing.T) {
	client, server := net.Pipe()
	_ = server.Close()
	t.Cleanup(func() { _ = client.Close() })
	cm := newTestConnManager()
	sink := &recordingSink{}
	cm.sink = sink
	if err := cm.writeConn(client, netproto.MsgKeyPublish, netproto.KeyPublish{}); err == nil {
		t.Fatal("uninstalled establishment write unexpectedly succeeded")
	}
	if sink.count("disconnected") != 0 {
		t.Fatal("uninstalled establishment write emitted disconnected")
	}
	cm.mu.Lock()
	installed := cm.conn
	cm.mu.Unlock()
	if installed != nil {
		t.Fatal("failed establishment write installed a connection")
	}
}

type reentrantDebugSink struct {
	cm    *connManager
	fired atomic.Bool
	done  chan error
}

func (s *reentrantDebugSink) Emit(name string, _ any) {
	if name != "debug_frame" {
		return
	}
	if s.fired.CompareAndSwap(false, true) {
		s.done <- s.cm.write(netproto.MsgPing, netproto.Ping{})
	}
}

func TestWriteTelemetryDoesNotHoldWriteMutexDuringEmit(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	cm := newTestConnManager()
	cm.debugFrames = true
	sink := &reentrantDebugSink{cm: cm, done: make(chan error, 1)}
	cm.sink = sink
	cm.mu.Lock()
	cm.conn = client
	cm.mu.Unlock()
	read := make(chan error, 1)
	go func() {
		for range 2 {
			if _, err := netproto.ReadFrame(server); err != nil {
				read <- err
				return
			}
		}
		read <- nil
	}()
	if err := cm.write(netproto.MsgPing, netproto.Ping{}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-sink.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("reentrant telemetry write deadlocked")
	}
	if err := <-read; err != nil {
		t.Fatal(err)
	}
}

type unexpectedFrameError struct{ got netproto.MessageType }

func (e *unexpectedFrameError) Error() string { return "unexpected frame " + e.got.String() }

func TestConnRequestsUseIndependentReplyGatesAndMatchErrors(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	cm := newTestConnManager()
	cm.readTimeout = time.Second
	cm.mu.Lock()
	cm.conn = client
	cm.mu.Unlock()
	go cm.readLoop(client)

	differentDone := make(chan error, 2)
	go func() {
		_, err := cm.request(netproto.MsgPermissionsQuery, netproto.MsgPermissionsResponse, netproto.PermissionsQuery{}, time.Second)
		differentDone <- err
	}()
	go func() {
		_, err := cm.request(netproto.MsgClientInfoQuery, netproto.MsgClientInfoResponse, netproto.ClientInfoQuery{}, time.Second)
		differentDone <- err
	}()
	first := readFrame(t, server)
	second := readFrame(t, server)
	if netproto.MessageType(second.Type) == netproto.MessageType(first.Type) {
		t.Fatal("distinct reply types did not proceed concurrently")
	}
	// Respond in the opposite order: a correlated failure must complete only
	// its matching request while the other reply succeeds.
	if err := netproto.WriteFrame(server, mustEncode(netproto.MsgError, netproto.Error{
		Message:    "second failed",
		OriginType: uint16(second.Type),
	})); err != nil {
		t.Fatalf("second error: %v", err)
	}
	var response any
	switch netproto.MessageType(first.Type) {
	case netproto.MsgPermissionsQuery:
		response = netproto.PermissionsResponse{}
		if err := netproto.WriteFrame(server, mustEncode(netproto.MsgPermissionsResponse, response)); err != nil {
			t.Fatal(err)
		}
	case netproto.MsgClientInfoQuery:
		response = netproto.ClientInfoResponse{}
		if err := netproto.WriteFrame(server, mustEncode(netproto.MsgClientInfoResponse, response)); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unexpected first request %v", netproto.MessageType(first.Type))
	}
	gotError := false
	for range 2 {
		if err := <-differentDone; err != nil {
			gotError = true
		}
	}
	if !gotError {
		t.Fatal("generic MsgError did not fail its sole in-flight request")
	}

	sameDone := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := cm.request(netproto.MsgAvatarGet, netproto.MsgAvatarData, netproto.AvatarGet{}, time.Second)
			sameDone <- err
		}()
	}
	frame := readFrame(t, server)
	if got := netproto.MessageType(frame.Type); got != netproto.MsgAvatarGet {
		t.Fatalf("first avatar request = %v", got)
	}
	_ = server.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := netproto.ReadFrame(server); err == nil {
		t.Fatal("same reply type wrote a second request before the first response")
	}
	_ = server.SetReadDeadline(time.Time{})
	if err := netproto.WriteFrame(server, mustEncode(netproto.MsgAvatarData, netproto.AvatarData{})); err != nil {
		t.Fatalf("first avatar response: %v", err)
	}
	frame = readFrame(t, server)
	if got := netproto.MessageType(frame.Type); got != netproto.MsgAvatarGet {
		t.Fatalf("second avatar request = %v", got)
	}
	if err := netproto.WriteFrame(server, mustEncode(netproto.MsgAvatarData, netproto.AvatarData{})); err != nil {
		t.Fatalf("second avatar response: %v", err)
	}
	for range 2 {
		if err := <-sameDone; err != nil {
			t.Fatalf("same reply request: %v", err)
		}
	}
}

func TestConnErrorOriginOnlyCompletesMatchingRequest(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	cm := newTestConnManager()
	sink := &recordingSink{}
	cm.sink = sink
	cm.mu.Lock()
	cm.conn = client
	cm.mu.Unlock()
	go cm.readLoop(client)

	// A fire-and-forget command has already been accepted by the wire.
	fireDone := make(chan error, 1)
	go func() { fireDone <- cm.write(netproto.MsgChatSend, netproto.ChatSend{}) }()
	if got := netproto.MessageType(readFrame(t, server).Type); got != netproto.MsgChatSend {
		t.Fatalf("fire-and-forget frame = %v", got)
	}
	if err := <-fireDone; err != nil {
		t.Fatal(err)
	}

	requestDone := make(chan error, 1)
	go func() {
		_, err := cm.request(netproto.MsgAvatarGet, netproto.MsgAvatarData, netproto.AvatarGet{}, time.Second)
		requestDone <- err
	}()
	if got := netproto.MessageType(readFrame(t, server).Type); got != netproto.MsgAvatarGet {
		t.Fatalf("request frame = %v", got)
	}
	if err := netproto.WriteFrame(server, mustEncode(netproto.MsgError, netproto.Error{
		Message: "chat send failed", OriginType: uint16(netproto.MsgChatSend),
	})); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-requestDone:
		t.Fatalf("unrelated fire-and-forget error completed request: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	if sink.count("servererror") != 1 {
		t.Fatal("unmatched correlated error was not emitted globally")
	}
	if err := netproto.WriteFrame(server, mustEncode(netproto.MsgAvatarData, netproto.AvatarData{})); err != nil {
		t.Fatal(err)
	}
	if err := <-requestDone; err != nil {
		t.Fatalf("matching typed reply failed: %v", err)
	}

	matching := make(chan error, 1)
	go func() {
		_, err := cm.request(netproto.MsgPermissionsQuery, netproto.MsgPermissionsResponse, netproto.PermissionsQuery{}, time.Second)
		matching <- err
	}()
	_ = readFrame(t, server)
	if err := netproto.WriteFrame(server, mustEncode(netproto.MsgError, netproto.Error{
		Message: "denied", OriginType: uint16(netproto.MsgPermissionsQuery),
	})); err != nil {
		t.Fatal(err)
	}
	if err := <-matching; err == nil || err.Error() != "denied" {
		t.Fatalf("matching correlated error = %v", err)
	}
}

func TestLegacyErrorIsGlobalAndTimeoutClosesExactConnection(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	cm := newTestConnManager()
	sink := &recordingSink{}
	cm.sink = sink
	cm.mu.Lock()
	cm.conn = client
	cm.mu.Unlock()
	go cm.readLoop(client)
	done := make(chan error, 2)
	go func() {
		_, err := cm.request(netproto.MsgAvatarGet, netproto.MsgAvatarData, netproto.AvatarGet{}, time.Second)
		done <- err
	}()
	go func() {
		_, err := cm.request(netproto.MsgPermissionsQuery, netproto.MsgPermissionsResponse, netproto.PermissionsQuery{}, time.Second)
		done <- err
	}()
	_ = readFrame(t, server)
	_ = readFrame(t, server)
	if err := netproto.WriteFrame(server, mustEncode(netproto.MsgError, netproto.Error{Message: "legacy failure"})); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("legacy error did not fail a current waiter")
			}
		case <-time.After(200 * time.Millisecond):
			t.Fatal("legacy error waited for request timeout")
		}
	}
	if sink.count("servererror") != 1 || sink.count("disconnected") != 1 {
		t.Fatalf("legacy error events servererror/disconnected = %d/%d", sink.count("servererror"), sink.count("disconnected"))
	}
	cm.mu.Lock()
	installed := cm.conn
	cm.mu.Unlock()
	if installed != nil {
		t.Fatal("timeout left old transport installed")
	}
}

func TestStaleFrameCannotCompleteReplacementRequest(t *testing.T) {
	oldClient, oldServer := net.Pipe()
	newClient, newServer := net.Pipe()
	t.Cleanup(func() {
		_ = oldClient.Close()
		_ = oldServer.Close()
		_ = newClient.Close()
		_ = newServer.Close()
	})
	cm := newTestConnManager()
	cm.mu.Lock()
	cm.conn = oldClient
	cm.connEpoch = 1
	cm.mu.Unlock()
	parsed := make(chan struct{})
	release := make(chan struct{})
	staleDone := make(chan struct{})
	cm.beforeDispatchLock = func() {
		close(parsed)
		<-release
	}
	// The old read loop has already parsed a reply but has not yet reached the
	// source validation/waiter claim critical section.
	go func() {
		cm.dispatchFrom(oldClient, 1, mustEncode(netproto.MsgAvatarData, netproto.AvatarData{}))
		close(staleDone)
	}()
	<-parsed
	cm.mu.Lock()
	cm.conn = newClient
	cm.connEpoch = 2
	cm.mu.Unlock()
	go cm.readLoop(newClient)

	done := make(chan error, 1)
	go func() {
		_, err := cm.request(netproto.MsgAvatarGet, netproto.MsgAvatarData, netproto.AvatarGet{}, time.Second)
		done <- err
	}()
	_ = readFrame(t, newServer)
	close(release)
	<-staleDone
	cm.beforeDispatchLock = nil
	select {
	case err := <-done:
		t.Fatalf("stale frame completed replacement request: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	if err := netproto.WriteFrame(newServer, mustEncode(netproto.MsgAvatarData, netproto.AvatarData{})); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestLateReplyAfterTimeoutCannotReachReplacementRequest(t *testing.T) {
	oldClient, oldServer := net.Pipe()
	newClient, newServer := net.Pipe()
	t.Cleanup(func() {
		_ = oldClient.Close()
		_ = oldServer.Close()
		_ = newClient.Close()
		_ = newServer.Close()
	})
	cm := newTestConnManager()
	cm.mu.Lock()
	cm.conn = oldClient
	cm.connEpoch = 1
	cm.mu.Unlock()
	go cm.readLoop(oldClient)
	first := make(chan error, 1)
	go func() {
		_, err := cm.request(netproto.MsgAvatarGet, netproto.MsgAvatarData, netproto.AvatarGet{}, 25*time.Millisecond)
		first <- err
	}()
	_ = readFrame(t, oldServer)
	if err := <-first; err == nil {
		t.Fatal("first request did not time out")
	}

	cm.mu.Lock()
	oldEpoch := uint64(1)
	cm.conn = newClient
	cm.connEpoch++
	cm.closed = false
	cm.mu.Unlock()
	go cm.readLoop(newClient)
	second := make(chan error, 1)
	go func() {
		_, err := cm.request(netproto.MsgAvatarGet, netproto.MsgAvatarData, netproto.AvatarGet{}, time.Second)
		second <- err
	}()
	_ = readFrame(t, newServer)
	cm.dispatchFrom(oldClient, oldEpoch, mustEncode(netproto.MsgAvatarData, netproto.AvatarData{}))
	select {
	case err := <-second:
		t.Fatalf("late timeout reply completed replacement request: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	if err := netproto.WriteFrame(newServer, mustEncode(netproto.MsgAvatarData, netproto.AvatarData{})); err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
}

func readFrame(t *testing.T, conn net.Conn) *netproto.Frame {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	frame, err := netproto.ReadFrame(conn)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return frame
}

func TestConnDisconnectWakesRequestAndStaleReadLoopKeepsReplacement(t *testing.T) {
	client, server := net.Pipe()
	cm := newTestConnManager()
	cm.mu.Lock()
	cm.conn = client
	cm.mu.Unlock()
	requestDone := make(chan error, 1)
	go func() {
		_, err := cm.request(netproto.MsgAvatarGet, netproto.MsgAvatarData, netproto.AvatarGet{}, time.Second)
		requestDone <- err
	}()
	_, _ = netproto.ReadFrame(server)
	cm.disconnect()
	select {
	case err := <-requestDone:
		if err == nil {
			t.Fatal("disconnect did not fail pending request")
		}
	case <-time.After(time.Second):
		t.Fatal("disconnect did not wake pending request")
	}
	_ = server.Close()

	oldClient, oldServer := net.Pipe()
	newClient, newServer := net.Pipe()
	t.Cleanup(func() {
		_ = oldClient.Close()
		_ = oldServer.Close()
		_ = newClient.Close()
		_ = newServer.Close()
	})
	cm = newTestConnManager()
	cm.mu.Lock()
	cm.conn = oldClient
	cm.mu.Unlock()
	staleDone := make(chan struct{})
	go func() {
		cm.readLoop(oldClient)
		close(staleDone)
	}()
	cm.mu.Lock()
	cm.conn = newClient
	cm.closed = false
	cm.mu.Unlock()
	_ = oldServer.Close()
	select {
	case <-staleDone:
	case <-time.After(time.Second):
		t.Fatal("stale read loop did not stop")
	}
	cm.mu.Lock()
	got := cm.conn
	cm.mu.Unlock()
	if got != newClient {
		t.Fatal("stale read loop cleared replacement connection")
	}
}

func TestConnDispatchDisconnectStress(t *testing.T) {
	cm := newTestConnManager()
	for range 1000 {
		waiter := make(chan requestResult, 1)
		cm.mu.Lock()
		cm.pending[netproto.MsgAvatarData] = pendingRequest{
			request: netproto.MsgAvatarGet,
			reply:   netproto.MsgAvatarData,
			result:  waiter,
		}
		cm.mu.Unlock()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			cm.dispatch(mustEncode(netproto.MsgAvatarData, netproto.AvatarData{}))
		}()
		go func() {
			defer wg.Done()
			cm.disconnect()
		}()
		wg.Wait()

		select {
		case <-waiter:
		default:
			t.Fatal("dispatch/disconnect dropped a detached waiter")
		}
	}
}

func TestConnStaleWriteFailureKeepsReplacement(t *testing.T) {
	oldClient, oldServer := net.Pipe()
	newClient, newServer := net.Pipe()
	t.Cleanup(func() {
		_ = oldClient.Close()
		_ = oldServer.Close()
		_ = newClient.Close()
		_ = newServer.Close()
	})
	cm := newTestConnManager()
	cm.writeTimeout = time.Second
	cm.mu.Lock()
	cm.conn = newClient
	cm.mu.Unlock()
	_ = oldServer.Close()
	if err := cm.writeConn(oldClient, netproto.MsgPing, netproto.Ping{}); err == nil {
		t.Fatal("write on closed stale connection unexpectedly succeeded")
	}
	cm.mu.Lock()
	got := cm.conn
	cm.mu.Unlock()
	if got != newClient {
		t.Fatal("stale write failure cleared replacement connection")
	}
}

func TestConnHeartbeatPongAndReadTimeout(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	cm := newTestConnManager()
	cm.heartbeatInterval = 10 * time.Millisecond
	cm.readTimeout = 80 * time.Millisecond
	cm.writeTimeout = time.Second
	cm.mu.Lock()
	cm.conn = client
	cm.mu.Unlock()
	go cm.readLoop(client)
	go cm.heartbeatLoop(client)
	ping := readFrame(t, server)
	if got := netproto.MessageType(ping.Type); got != netproto.MsgPing {
		t.Fatalf("heartbeat = %v, want Ping", got)
	}
	if err := netproto.WriteFrame(server, mustEncode(netproto.MsgPong, netproto.Pong{})); err != nil {
		t.Fatalf("pong: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for cm.connected() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if cm.connected() {
		t.Fatal("read timeout did not disconnect silent peer")
	}
}

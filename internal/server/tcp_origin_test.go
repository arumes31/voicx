package server

import (
	"net"
	"testing"
	"time"

	"voicx/internal/netproto"
)

func TestExplicitErrorOriginsDoNotLeakAcrossConcurrentWrites(t *testing.T) {
	serverConn, peer := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = peer.Close()
	})
	client := &Client{ID: "origin-test", Conn: serverConn}
	s := &TCPServer{}
	done := make(chan error, 3)

	// The first net.Pipe write blocks before its peer starts reading. Queue two
	// unrelated responses behind it; all three calls share one Client but their
	// immutable arguments must survive the write serialization unchanged.
	go func() { done <- s.sendErrorFor(client, netproto.MsgPermissionsQuery, errCodeMalformed, "permissions") }()
	time.Sleep(10 * time.Millisecond)
	go func() { done <- s.sendErrorFor(client, netproto.MsgClientInfoQuery, errCodeNotFound, "client") }()
	go func() { done <- s.sendGlobalError(client, errCodeUnavailable, "global") }()

	origins := map[netproto.MessageType]int{}
	for range 3 {
		_ = peer.SetReadDeadline(time.Now().Add(time.Second))
		frame, err := netproto.ReadFrame(peer)
		if err != nil {
			t.Fatal(err)
		}
		var got netproto.Error
		if err := netproto.Decode(frame, &got); err != nil {
			t.Fatal(err)
		}
		origins[netproto.MessageType(got.OriginType)]++
	}
	for range 3 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if origins[netproto.MsgPermissionsQuery] != 1 || origins[netproto.MsgClientInfoQuery] != 1 || origins[0] != 1 {
		t.Fatalf("error origins = %#v", origins)
	}
}

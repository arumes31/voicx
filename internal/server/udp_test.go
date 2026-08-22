package server

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"voicx/internal/config"
	"voicx/internal/metrics"
	"voicx/internal/netproto"
)

// freeUDPPort returns a UDP address string bound to an ephemeral free port
// on the loopback interface.
func freeUDPPort(t *testing.T) string {
	t.Helper()
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("freeUDPPort: %v", err)
	}
	addr := ln.LocalAddr().String()
	_ = ln.Close()
	return addr
}

// TestNewUDPServer verifies that NewUDP returns a non-nil UDPServer.
func TestNewUDPServer(t *testing.T) {
	cfg := &config.Config{UDPAddr: "127.0.0.1:0"}
	s := NewUDP(cfg, testLogger())
	if s == nil {
		t.Fatal("NewUDP returned nil UDPServer")
	}
}

// TestUDPServerStartShutdownPingPong exercises the full lifecycle: start the
// server, send a UDPMsgPing packet, expect a UDPMsgPong reply, then cancel the
// context and assert Shutdown returns cleanly. It also asserts that Stats()
// reports non-zero counters after the exchange.
func TestUDPServerStartShutdownPingPong(t *testing.T) {
	addr := freeUDPPort(t)
	cfg := &config.Config{UDPAddr: addr}
	s := NewUDP(cfg, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startErr := make(chan error, 1)
	go func() {
		startErr <- s.Start(ctx)
	}()
	select {
	case <-s.started:
		// The socket is bound; UDP dial cannot race the listener startup.
	case err := <-startErr:
		t.Fatalf("Start returned before binding: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("UDP server did not bind within 3 seconds")
	}

	// Resolve the server address and dial a UDP client.
	srvAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatalf("resolve server addr: %v", err)
	}

	conn, err := net.DialUDP("udp", nil, srvAddr)
	if err != nil {
		t.Fatalf("dial udp server: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Send a UDPMsgPing packet and expect a UDPMsgPong reply. The ping is
	// retried because UDP delivery itself is lossy even after the socket binds.
	buf := make([]byte, 64)
	deadline := time.Now().Add(5 * time.Second)
	var n int
	for {
		if _, err := conn.Write([]byte{netproto.UDPMsgPing}); err != nil {
			t.Fatalf("write ping: %v", err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		n, _, err = conn.ReadFromUDP(buf)
		if err == nil {
			break
		}
		if nerr, ok := err.(net.Error); ok && nerr.Timeout() && time.Now().Before(deadline) {
			continue
		}
		t.Fatalf("read pong: %v", err)
	}
	if n < 1 || buf[0] != netproto.UDPMsgPong {
		t.Fatalf("reply = %x, want %x", buf[:n], netproto.UDPMsgPong)
	}

	// Wait for the server to process the packet so Stats() reflects it.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if stats := s.Stats(); stats.PacketsReceived > 0 && stats.PacketsProcessed > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	stats := s.Stats()
	if stats.PacketsReceived == 0 {
		t.Errorf("Stats().PacketsReceived = 0, want > 0")
	}
	if stats.PacketsProcessed == 0 {
		t.Errorf("Stats().PacketsProcessed = 0, want > 0")
	}

	// Cancel context and assert Shutdown returns cleanly.
	cancel()
	if err := <-startErr; err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if err := s.Shutdown(); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}
}

func TestUDPServerRateLimitReportsStatsAndMetrics(t *testing.T) {
	addr := freeUDPPort(t)
	s := NewUDP(&config.Config{
		UDPAddr:         addr,
		UDPRateLimitPPS: 1,
		UDPRateBurst:    1,
	}, testLogger())
	m := metrics.New()
	s.Metrics = m
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	startErr := make(chan error, 1)
	go func() { startErr <- s.Start(ctx) }()
	select {
	case <-s.started:
	case err := <-startErr:
		t.Fatalf("Start returned before binding: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("UDP server did not bind within 3 seconds")
	}
	t.Cleanup(func() {
		cancel()
		if err := <-startErr; err != nil {
			t.Errorf("Start returned error: %v", err)
		}
	})

	serverAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatalf("resolve UDP address: %v", err)
	}
	conn, err := net.DialUDP("udp", nil, serverAddr)
	if err != nil {
		t.Fatalf("dial UDP: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte{netproto.UDPMsgPing}); err != nil {
		t.Fatalf("write admitted ping: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for s.Stats().PacketsReceived == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	before := s.Stats()
	if before.PacketsReceived == 0 {
		t.Fatal("admitted packet was not counted")
	}
	for range 2 {
		if _, err := conn.Write([]byte{netproto.UDPMsgPing}); err != nil {
			t.Fatalf("write rate-limited ping: %v", err)
		}
	}

	deadline = time.Now().Add(2 * time.Second)
	for s.Stats().PacketsRateLimited == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := s.Stats().PacketsRateLimited; got == 0 {
		t.Fatal("rate-limited packets = 0, want > 0")
	}
	after := s.Stats()
	if after.PacketsReceived != before.PacketsReceived || after.PacketsDropped != before.PacketsDropped {
		t.Fatalf(
			"pre-admission rate limit changed admitted counters: received/dropped %d/%d, want %d/%d",
			after.PacketsReceived,
			after.PacketsDropped,
			before.PacketsReceived,
			before.PacketsDropped,
		)
	}
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	values := map[string]float64{}
	for _, family := range families {
		if family.GetName() == "voicx_udp_packets_rate_limited_total" || family.GetName() == "voicx_udp_packets_dropped_total" {
			values[family.GetName()] = family.GetMetric()[0].GetCounter().GetValue()
		}
	}
	for name, value := range values {
		if value == 0 {
			t.Fatalf("%s = 0, want > 0", name)
		}
	}
	if len(values) != 2 {
		t.Fatalf("UDP Prometheus counters = %v, want rate-limited and generic dropped", values)
	}
}

func TestUDPInboundQueueDepth(t *testing.T) {
	s := NewUDP(&config.Config{UDPAddr: "127.0.0.1:0"}, testLogger())
	s.inbound <- udpPacket{}
	if got := s.InboundQueueDepth(); got != 1 {
		t.Fatalf("InboundQueueDepth = %d, want 1", got)
	}
	<-s.inbound
}

func TestUDPWorkerRecoversOnePacketPanicAndContinues(t *testing.T) {
	core, observed := observer.New(zapcore.DebugLevel)
	s := NewUDP(&config.Config{UDPAddr: "127.0.0.1:0"}, zap.New(core))
	s.Metrics = metrics.New()
	var calls atomic.Int32
	s.beforeProcess = func(udpPacket) {
		if calls.Add(1) == 1 {
			panic(udpPanic{secret: "udp-panic-secret"})
		}
	}
	s.inbound = make(chan udpPacket, 2)
	s.wg.Add(1)
	go s.worker()
	s.inbound <- udpPacket{payload: []byte{netproto.UDPMsgPing, 0xaa, 0xbb}}
	s.inbound <- udpPacket{payload: []byte{0xff}}
	close(s.inbound)
	s.wg.Wait()

	stats := s.Stats()
	if stats.PacketsProcessed != 2 {
		t.Fatalf("PacketsProcessed = %d, want 2", stats.PacketsProcessed)
	}
	if stats.PacketsDropped != 2 {
		t.Fatalf("PacketsDropped = %d, want 2 (panic plus unknown packet)", stats.PacketsDropped)
	}
	entries := observed.FilterMessage("udp packet handler panic").All()
	if len(entries) != 1 {
		t.Fatalf("panic logs = %d, want 1", len(entries))
	}
	if got := entries[0].ContextMap()["remote"]; got != "<unknown>" {
		t.Fatalf("panic remote = %#v, want safe unknown marker", got)
	}
	fields := entries[0].ContextMap()
	if got := fields["panic_type"]; got != "server.udpPanic" {
		t.Fatalf("panic type = %#v, want server.udpPanic", got)
	}
	if stack, ok := fields["stack"].(string); !ok || stack == "" {
		t.Fatalf("panic log is missing stack: %#v", fields)
	}
	if strings.Contains(entries[0].Message+fmt.Sprint(fields), "udp-panic-secret") || strings.Contains(fmt.Sprint(fields), "aabb") {
		t.Fatalf("panic log leaked recovered value or packet payload: %#v", fields)
	}
	families, err := s.Metrics.(*metrics.Metrics).Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() == "voicx_udp_packets_dropped_total" {
			if got := family.GetMetric()[0].GetCounter().GetValue(); got != 2 {
				t.Fatalf("Prometheus dropped packets = %v, want 2", got)
			}
			return
		}
	}
	t.Fatal("UDP dropped-packets metric was not registered")
}

type udpPanic struct{ secret string }

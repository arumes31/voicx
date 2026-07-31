package metrics

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRegistryGather verifies the registry collects the voicx metrics and
// the Go collector.
func TestRegistryGather(t *testing.T) {
	m := New()
	m.SetClientsConnected(3)
	m.IncChatMessage("global")
	m.IncRTPForwarded("audio", 5)

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	names := make(map[string]bool, len(families))
	for _, f := range families {
		names[f.GetName()] = true
	}
	for _, want := range []string{
		"voicx_clients_connected", "voicx_chat_messages_total",
		"voicx_rtp_packets_forwarded_total", "go_goroutines",
	} {
		if !names[want] {
			t.Errorf("metric family %q not gathered", want)
		}
	}
}

// TestCounterValues verifies instrumented increments land in the registry.
func TestCounterValues(t *testing.T) {
	m := New()
	m.IncTCPConnections()
	m.IncTCPConnections()
	m.IncUDPPacketsDropped()
	m.IncFileTransfer("upload", "ok")

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	value := func(name string) float64 {
		for _, f := range families {
			if f.GetName() == name {
				return f.GetMetric()[0].GetCounter().GetValue()
			}
		}
		return -1
	}
	if got := value("voicx_tcp_connections_total"); got != 2 {
		t.Errorf("tcp_connections = %v, want 2", got)
	}
	if got := value("voicx_udp_packets_dropped_total"); got != 1 {
		t.Errorf("udp_dropped = %v, want 1", got)
	}
}

// TestMetricsHandler verifies the /metrics endpoint serves the text format.
func TestMetricsHandler(t *testing.T) {
	m := New()
	m.SetWebRTCPeers(2)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Result().Body)
	if !strings.Contains(string(body), "voicx_webrtc_peers 2") {
		t.Fatalf("body missing voicx_webrtc_peers 2:\n%s", body)
	}
}

// TestNoop verifies the Noop sink satisfies Sink and does not panic.
func TestNoop(t *testing.T) {
	var s Sink = Noop{}
	s.SetClientsConnected(1)
	s.IncChatMessage("channel")
	s.IncFileTransfer("download", "error")
}

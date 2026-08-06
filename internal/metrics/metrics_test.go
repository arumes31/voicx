package metrics

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
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

func TestGaugeValuesCannotBecomeNegative(t *testing.T) {
	m := New()
	m.SetClientsConnected(-1)
	m.SetChannelsActive(-2)
	m.SetWebRTCPeers(-3)

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, name := range []string{
		"voicx_clients_connected",
		"voicx_channels_active",
		"voicx_webrtc_peers",
	} {
		family := metricFamily(t, families, name)
		if got := family.GetMetric()[0].GetGauge().GetValue(); got != 0 {
			t.Errorf("%s = %v, want 0", name, got)
		}
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

func TestMetricsHandlerNegotiatesOpenMetrics(t *testing.T) {
	m := New()
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Accept", "application/openmetrics-text")
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/openmetrics-text") {
		t.Fatalf("Content-Type = %q, want OpenMetrics", got)
	}
	if !strings.HasSuffix(rec.Body.String(), "# EOF\n") {
		t.Fatal("OpenMetrics response is missing the EOF marker")
	}
}

func TestLabelsHaveBoundedCardinality(t *testing.T) {
	m := New()
	m.IncUDPPackets("attacker-controlled-kind")
	m.IncChatMessage("attacker-controlled-scope")
	m.IncRTPForwarded("attacker-controlled-media", -10)
	m.IncRTPForwarded("attacker-controlled-media", 2)
	m.IncFileTransfer("attacker-controlled-direction", "attacker-controlled-result")

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, familyName := range []string{
		"voicx_udp_packets_total",
		"voicx_chat_messages_total",
		"voicx_rtp_packets_forwarded_total",
		"voicx_file_transfers_total",
	} {
		family := metricFamily(t, families, familyName)
		if len(family.GetMetric()) != 1 {
			t.Fatalf("%s metric count = %d, want 1", familyName, len(family.GetMetric()))
		}
		for _, label := range family.GetMetric()[0].GetLabel() {
			if label.GetValue() != "unknown" {
				t.Errorf("%s label %s = %q, want unknown", familyName, label.GetName(), label.GetValue())
			}
		}
	}
	if got := metricFamily(t, families, "voicx_rtp_packets_forwarded_total").GetMetric()[0].GetCounter().GetValue(); got != 2 {
		t.Fatalf("RTP forwarded count = %v, want 2", got)
	}
}

func TestRegisterDBPoolExportsLimitsAndCounters(t *testing.T) {
	db := sql.OpenDB(inertConnector{})
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(17)
	m := New()
	m.RegisterDBPool(db)
	m.RegisterDBPool(db) // Registration is deliberately idempotent.

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	maxOpen := metricFamily(t, families, "voicx_db_pool_max_open_connections")
	if got := maxOpen.GetMetric()[0].GetGauge().GetValue(); got != 17 {
		t.Fatalf("max open connections = %v, want 17", got)
	}
	for _, name := range []string{
		"voicx_db_pool_wait_count_total",
		"voicx_db_pool_wait_duration_seconds_total",
		"voicx_db_pool_closed_max_idle_total",
		"voicx_db_pool_closed_max_idle_time_total",
		"voicx_db_pool_closed_max_lifetime_total",
	} {
		family := metricFamily(t, families, name)
		if family.GetType().String() != "COUNTER" {
			t.Errorf("%s type = %s, want COUNTER", name, family.GetType())
		}
	}
}

func metricFamily(t *testing.T, families []*dto.MetricFamily, name string) *dto.MetricFamily {
	t.Helper()
	for _, family := range families {
		if family.GetName() == name {
			return family
		}
	}
	t.Fatalf("metric family %q not gathered", name)
	return nil
}

type inertConnector struct{}

func (inertConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("inert test connector cannot connect")
}

func (inertConnector) Driver() driver.Driver { return inertDriver{} }

type inertDriver struct{}

func (inertDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("inert test driver cannot connect")
}

// TestNoop verifies the Noop sink satisfies Sink and does not panic.
func TestNoop(t *testing.T) {
	var s Sink = Noop{}
	s.SetClientsConnected(1)
	s.IncChatMessage("channel")
	s.IncFileTransfer("download", "error")
}

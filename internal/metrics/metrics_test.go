package metrics

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
)

// TestRegistryGather verifies the registry collects the voicx metrics and
// the Go collector.
func TestRegistryGather(t *testing.T) {
	m := New()
	m.RegisterStateStats(func() (int, int) { return 3, 1 })
	m.IncChatMessage("global")
	m.IncRTPForwarded("audio", 5)
	m.IncAuthFailure("tcp", "invalid_credentials")

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
		"voicx_rtp_packets_forwarded_total", "voicx_auth_failures_total", "go_goroutines",
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
	m.RegisterStateStats(func() (int, int) { return -1, -2 })
	m.RegisterWebRTCPeerCount(func() int { return -3 })
	m.RegisterUDPInboundQueueDepth(func() int { return -4 })
	m.RegisterRecorderSessionCount(func() int { return -5 })

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, name := range []string{
		"voicx_clients_connected",
		"voicx_channels_active",
		"voicx_webrtc_peers",
		"voicx_udp_inbound_queue_depth",
		"voicx_recordings_active",
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
	m.RegisterWebRTCPeerCount(func() int { return 2 })

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Result().Body)
	if !strings.Contains(string(body), "voicx_webrtc_peers 2") {
		t.Fatalf("body missing voicx_webrtc_peers 2:\n%s", body)
	}
}

func TestGaugeFuncsSampleCallbacksAndKeepFirstRegistration(t *testing.T) {
	m := New()
	var clients atomic.Int64
	clients.Store(3)
	m.RegisterStateStats(func() (int, int) { return int(clients.Load()), 2 })
	m.RegisterStateStats(func() (int, int) { return 99, 99 })
	m.RegisterWebRTCPeerCount(func() int { return 4 })
	m.RegisterUDPInboundQueueDepth(func() int { return 5 })
	m.RegisterRecorderSessionCount(func() int { return 6 })

	assertGauge := func(name string, want float64) {
		t.Helper()
		families, err := m.Registry().Gather()
		if err != nil {
			t.Fatalf("Gather: %v", err)
		}
		if got := metricFamily(t, families, name).GetMetric()[0].GetGauge().GetValue(); got != want {
			t.Fatalf("%s = %v, want %v", name, got, want)
		}
	}
	assertGauge("voicx_clients_connected", 3)
	assertGauge("voicx_channels_active", 2)
	assertGauge("voicx_webrtc_peers", 4)
	assertGauge("voicx_udp_inbound_queue_depth", 5)
	assertGauge("voicx_recordings_active", 6)
	clients.Store(7)
	assertGauge("voicx_clients_connected", 7)
}

func TestBoundedCollectorsUseFixedLabelsAndBuckets(t *testing.T) {
	m := New()
	m.IncUDPPacketsRateLimited()
	m.IncRecordingError("start")
	m.IncRecordingError("close")
	m.IncChatCryptoFailure("decrypt")
	m.IncChatCryptoFailure("untrusted input")
	m.ObserveReadiness("postgres", "ok", 10*time.Millisecond)
	m.ObserveReadiness("storage", "ok", 5*time.Millisecond)
	m.ObserveReadiness("redis", "error", 25*time.Millisecond)
	m.ObserveReadiness(
		"untrusted component",
		"untrusted result",
		-time.Second,
	)
	m.ObserveBroadcastSnapshot(-time.Second)
	m.ObserveBroadcastClientBacklog(-1)

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if got := metricFamily(t, families, "voicx_udp_packets_rate_limited_total").GetMetric()[0].GetCounter().GetValue(); got != 1 {
		t.Fatalf("rate-limited UDP packets = %v, want 1", got)
	}
	for _, name := range []string{
		"voicx_recording_errors_total",
		"voicx_chat_crypto_failures_total",
	} {
		for _, metric := range metricFamily(t, families, name).GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetValue() == "untrusted input" || label.GetValue() == "close" {
					t.Fatalf("%s leaked unbounded label value %q", name, label.GetValue())
				}
			}
		}
	}
	for _, name := range []string{
		"voicx_broadcast_snapshot_duration_seconds",
		"voicx_broadcast_client_backlog_depth",
		"voicx_readiness_probe_duration_seconds",
	} {
		family := metricFamily(t, families, name)
		if family.GetType() != dto.MetricType_HISTOGRAM {
			t.Fatalf("%s type = %s, want HISTOGRAM", name, family.GetType())
		}
		for _, metric := range family.GetMetric() {
			if metric.GetHistogram().GetSampleCount() == 0 {
				t.Fatalf("%s has an empty histogram", name)
			}
		}
	}
	histogramFor := func(name string, labels map[string]string) *dto.Histogram {
		t.Helper()
		for _, metric := range metricFamily(t, families, name).GetMetric() {
			matched := true
			for _, label := range metric.GetLabel() {
				if labels[label.GetName()] != label.GetValue() {
					matched = false
					break
				}
			}
			if matched && len(metric.GetLabel()) == len(labels) {
				return metric.GetHistogram()
			}
		}
		t.Fatalf("%s missing labels %v", name, labels)
		return nil
	}
	if got := histogramFor("voicx_broadcast_snapshot_duration_seconds", nil).GetSampleSum(); got != 0 {
		t.Fatalf("negative snapshot duration sum = %v, want 0", got)
	}
	if got := histogramFor("voicx_broadcast_client_backlog_depth", nil).GetSampleSum(); got != 0 {
		t.Fatalf("negative broadcast backlog sum = %v, want 0", got)
	}
	postgresOK := histogramFor("voicx_readiness_probe_duration_seconds", map[string]string{
		"component": "postgres",
		"result":    "ok",
	})
	if postgresOK.GetSampleCount() != 1 || postgresOK.GetSampleSum() != .01 {
		t.Fatalf("postgres readiness count/sum = %d/%v, want 1/0.01", postgresOK.GetSampleCount(), postgresOK.GetSampleSum())
	}
	storageOK := histogramFor("voicx_readiness_probe_duration_seconds", map[string]string{
		"component": "storage",
		"result":    "ok",
	})
	if storageOK.GetSampleCount() != 1 || storageOK.GetSampleSum() != .005 {
		t.Fatalf("storage readiness count/sum = %d/%v, want 1/0.005", storageOK.GetSampleCount(), storageOK.GetSampleSum())
	}
	redisError := histogramFor("voicx_readiness_probe_duration_seconds", map[string]string{
		"component": "redis",
		"result":    "error",
	})
	if redisError.GetSampleCount() != 1 || redisError.GetSampleSum() != .025 {
		t.Fatalf("Redis readiness count/sum = %d/%v, want 1/0.025", redisError.GetSampleCount(), redisError.GetSampleSum())
	}
	unknownReadiness := histogramFor("voicx_readiness_probe_duration_seconds", map[string]string{
		"component": "unknown",
		"result":    "unknown",
	})
	if unknownReadiness.GetSampleCount() != 1 || unknownReadiness.GetSampleSum() != 0 {
		t.Fatalf("unknown readiness count/sum = %d/%v, want 1/0", unknownReadiness.GetSampleCount(), unknownReadiness.GetSampleSum())
	}
	if got := len(metricFamily(t, families, "voicx_readiness_probe_duration_seconds").GetMetric()); got != 4 {
		t.Fatalf("readiness metric series = %d, want bounded 4", got)
	}

	durationBuckets := metricFamily(t, families, "voicx_broadcast_snapshot_duration_seconds").GetMetric()[0].GetHistogram().GetBucket()
	if len(durationBuckets) != 11 || durationBuckets[0].GetUpperBound() != .001 || durationBuckets[10].GetUpperBound() != 5 {
		t.Fatalf("duration buckets = %+v, want fixed .001..5 buckets", durationBuckets)
	}
	backlogBuckets := metricFamily(t, families, "voicx_broadcast_client_backlog_depth").GetMetric()[0].GetHistogram().GetBucket()
	if len(backlogBuckets) != 7 || backlogBuckets[0].GetUpperBound() != 0 || backlogBuckets[6].GetUpperBound() != 16 {
		t.Fatalf("backlog buckets = %+v, want fixed 0..16 buckets", backlogBuckets)
	}
}

func TestMetricsHandlerNegotiatesOpenMetrics(t *testing.T) {
	m := New()
	req := httptest.NewRequestWithContext(t.Context(), "GET", "/metrics", nil)
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
	m.IncAuthFailure("attacker-controlled-transport", "attacker-controlled-reason")

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, familyName := range []string{
		"voicx_udp_packets_total",
		"voicx_chat_messages_total",
		"voicx_rtp_packets_forwarded_total",
		"voicx_file_transfers_total",
		"voicx_auth_failures_total",
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

func TestAuthFailureMetricsUseOnlyStableLabels(t *testing.T) {
	m := New()
	m.IncAuthFailure("tcp", "invalid_credentials")
	m.IncAuthFailure("grpc", "locked_out")
	m.IncAuthFailure("ws", "invalid_credentials")
	m.IncAuthFailure("tcp", "198.51.100.1:attacker@example.invalid")

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	family := metricFamily(t, families, "voicx_auth_failures_total")
	if len(family.GetMetric()) != 4 {
		t.Fatalf("auth failure metric count = %d, want 4", len(family.GetMetric()))
	}
	for _, metric := range family.GetMetric() {
		if len(metric.GetLabel()) != 2 || metric.GetLabel()[0].GetName() != "reason" || metric.GetLabel()[1].GetName() != "transport" {
			t.Fatalf("auth failure labels = %+v, want reason and transport", metric.GetLabel())
		}
		for _, label := range metric.GetLabel() {
			if label.GetValue() == "198.51.100.1:attacker@example.invalid" {
				t.Fatalf("attacker-controlled value leaked into %s", label.GetName())
			}
		}
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
	s.IncUDPPacketsRateLimited()
	s.IncChatMessage("channel")
	s.IncFileTransfer("download", "error")
	s.IncAuthFailure("tcp", "invalid_credentials")
	s.IncRecordingError("start")
	s.IncChatCryptoFailure("decrypt")
}

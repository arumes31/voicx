// Package metrics wraps the Prometheus client library with a voicx-specific
// registry and a narrow Sink interface. Server components consume Sink (or
// the Noop implementation in tests) so no Prometheus calls are sprinkled
// through handlers.
package metrics

import (
	"database/sql"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"voicx/internal/version"
)

const (
	metricsMaxRequestsInFlight = 5
	metricsHandlerTimeout      = 10 * time.Second
)

// Sink is the narrow metrics interface used across voicx. *Metrics and Noop
// implement it.
type Sink interface {
	SetClientsConnected(n int)
	SetChannelsActive(n int)
	IncUDPPackets(kind string)
	IncUDPPacketsDropped()
	IncTCPConnections()
	IncChatMessage(scope string)
	SetWebRTCPeers(n int)
	IncRTPForwarded(media string, n int)
	IncFileTransfer(direction, result string)
}

// RegisterDBPool exports database/sql pool pressure so MaxOpenConns,
// MaxIdleConns and MaxConnLifetime can be tuned from measured saturation.
// GaugeFuncs sample DB.Stats at scrape time and add no hot-path work.
func (m *Metrics) RegisterDBPool(db *sql.DB) {
	if m == nil || db == nil {
		return
	}
	m.dbPoolOnce.Do(func() {
		poolCollectors := []prometheus.Collector{
			prometheus.NewGaugeFunc(prometheus.GaugeOpts{Namespace: "voicx", Subsystem: "db_pool", Name: "max_open_connections", Help: "Configured maximum PostgreSQL pool connections."}, func() float64 { return float64(db.Stats().MaxOpenConnections) }),
			prometheus.NewGaugeFunc(prometheus.GaugeOpts{Namespace: "voicx", Subsystem: "db_pool", Name: "open_connections", Help: "Open PostgreSQL pool connections."}, func() float64 { return float64(db.Stats().OpenConnections) }),
			prometheus.NewGaugeFunc(prometheus.GaugeOpts{Namespace: "voicx", Subsystem: "db_pool", Name: "in_use_connections", Help: "PostgreSQL pool connections currently in use."}, func() float64 { return float64(db.Stats().InUse) }),
			prometheus.NewGaugeFunc(prometheus.GaugeOpts{Namespace: "voicx", Subsystem: "db_pool", Name: "idle_connections", Help: "Idle PostgreSQL pool connections."}, func() float64 { return float64(db.Stats().Idle) }),
			prometheus.NewCounterFunc(prometheus.CounterOpts{Namespace: "voicx", Subsystem: "db_pool", Name: "wait_count_total", Help: "Requests that waited for a PostgreSQL pool connection."}, func() float64 { return float64(db.Stats().WaitCount) }),
			prometheus.NewCounterFunc(prometheus.CounterOpts{Namespace: "voicx", Subsystem: "db_pool", Name: "wait_duration_seconds_total", Help: "Total time spent waiting for PostgreSQL pool connections."}, func() float64 { return db.Stats().WaitDuration.Seconds() }),
			prometheus.NewCounterFunc(prometheus.CounterOpts{Namespace: "voicx", Subsystem: "db_pool", Name: "closed_max_idle_total", Help: "PostgreSQL connections closed after exceeding the idle pool limit."}, func() float64 { return float64(db.Stats().MaxIdleClosed) }),
			prometheus.NewCounterFunc(prometheus.CounterOpts{Namespace: "voicx", Subsystem: "db_pool", Name: "closed_max_idle_time_total", Help: "PostgreSQL connections closed after exceeding the idle time limit."}, func() float64 { return float64(db.Stats().MaxIdleTimeClosed) }),
			prometheus.NewCounterFunc(prometheus.CounterOpts{Namespace: "voicx", Subsystem: "db_pool", Name: "closed_max_lifetime_total", Help: "PostgreSQL connections closed after exceeding the lifetime limit."}, func() float64 { return float64(db.Stats().MaxLifetimeClosed) }),
		}
		m.registry.MustRegister(poolCollectors...)
	})
}

// Metrics is the Prometheus-backed Sink.
type Metrics struct {
	registry   *prometheus.Registry
	dbPoolOnce sync.Once

	clientsConnected prometheus.Gauge
	channelsActive   prometheus.Gauge
	udpPackets       *prometheus.CounterVec
	udpDropped       prometheus.Counter
	tcpConnections   prometheus.Counter
	chatMessages     *prometheus.CounterVec
	webrtcPeers      prometheus.Gauge
	rtpForwarded     *prometheus.CounterVec
	fileTransfers    *prometheus.CounterVec
	buildInfo        *prometheus.GaugeVec
}

// New constructs a Metrics with its own registry (voicx_* metrics plus the
// default Go collectors).
func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	m := &Metrics{
		registry: reg,
		clientsConnected: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "voicx", Name: "clients_connected",
			Help: "Currently connected control-channel clients.",
		}),
		channelsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "voicx", Name: "channels_active",
			Help: "Currently active channels.",
		}),
		udpPackets: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "voicx", Name: "udp_packets_total",
			Help: "UDP packets processed by message kind.",
		}, []string{"kind"}),
		udpDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "voicx", Name: "udp_packets_dropped_total",
			Help: "UDP packets dropped (queue full or rate limited).",
		}),
		tcpConnections: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "voicx", Name: "tcp_connections_total",
			Help: "TCP control connections accepted.",
		}),
		chatMessages: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "voicx", Name: "chat_messages_total",
			Help: "Chat messages routed by scope (channel/direct/global).",
		}, []string{"scope"}),
		webrtcPeers: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "voicx", Name: "webrtc_peers",
			Help: "Active WebRTC peer connections.",
		}),
		rtpForwarded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "voicx", Name: "rtp_packets_forwarded_total",
			Help: "RTP packets forwarded by the SFU by media type.",
		}, []string{"media"}),
		fileTransfers: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "voicx", Name: "file_transfers_total",
			Help: "File transfers by direction and result.",
		}, []string{"direction", "result"}),
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "voicx", Name: "build_info",
			Help: "Embedded build metadata (always 1).",
		}, []string{"version", "commit"}),
	}
	reg.MustRegister(
		m.clientsConnected, m.channelsActive, m.udpPackets, m.udpDropped,
		m.tcpConnections, m.chatMessages, m.webrtcPeers, m.rtpForwarded,
		m.fileTransfers, m.buildInfo,
	)
	m.buildInfo.WithLabelValues(version.String(), version.Commit).Set(1)
	return m
}

// Registry returns the underlying registry (for tests).
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// Handler returns an HTTP handler serving the /metrics text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		EnableOpenMetrics:   true,
		MaxRequestsInFlight: metricsMaxRequestsInFlight,
		Timeout:             metricsHandlerTimeout,
	})
}

func (m *Metrics) SetClientsConnected(n int) { m.clientsConnected.Set(nonNegative(n)) }
func (m *Metrics) SetChannelsActive(n int)   { m.channelsActive.Set(nonNegative(n)) }
func (m *Metrics) IncUDPPackets(kind string) {
	m.udpPackets.WithLabelValues(udpKindLabel(kind)).Inc()
}
func (m *Metrics) IncUDPPacketsDropped() { m.udpDropped.Inc() }
func (m *Metrics) IncTCPConnections()    { m.tcpConnections.Inc() }
func (m *Metrics) IncChatMessage(scope string) {
	m.chatMessages.WithLabelValues(chatScopeLabel(scope)).Inc()
}
func (m *Metrics) SetWebRTCPeers(n int) { m.webrtcPeers.Set(nonNegative(n)) }
func (m *Metrics) IncRTPForwarded(media string, n int) {
	if n <= 0 {
		return
	}
	m.rtpForwarded.WithLabelValues(mediaLabel(media)).Add(float64(n))
}
func (m *Metrics) IncFileTransfer(direction, result string) {
	m.fileTransfers.WithLabelValues(transferDirectionLabel(direction), transferResultLabel(result)).Inc()
}

func udpKindLabel(value string) string {
	switch value {
	case "ping", "pong":
		return value
	default:
		return "unknown"
	}
}

func chatScopeLabel(value string) string {
	switch value {
	case "global", "channel", "direct", "rejected":
		return value
	default:
		return "unknown"
	}
}

func mediaLabel(value string) string {
	switch value {
	case "audio", "video":
		return value
	default:
		return "unknown"
	}
}

func transferDirectionLabel(value string) string {
	switch value {
	case "upload", "download":
		return value
	default:
		return "unknown"
	}
}

func transferResultLabel(value string) string {
	switch value {
	case "ok", "error":
		return value
	default:
		return "unknown"
	}
}

func nonNegative(value int) float64 {
	if value < 0 {
		return 0
	}
	return float64(value)
}

// Noop is a Sink that discards everything, for tests and partial startups.
type Noop struct{}

func (Noop) SetClientsConnected(int)        {}
func (Noop) SetChannelsActive(int)          {}
func (Noop) IncUDPPackets(string)           {}
func (Noop) IncUDPPacketsDropped()          {}
func (Noop) IncTCPConnections()             {}
func (Noop) IncChatMessage(string)          {}
func (Noop) SetWebRTCPeers(int)             {}
func (Noop) IncRTPForwarded(string, int)    {}
func (Noop) IncFileTransfer(string, string) {}

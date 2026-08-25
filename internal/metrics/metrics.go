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
	IncUDPPackets(kind string)
	IncUDPPacketsDropped()
	IncUDPPacketsRateLimited()
	IncTCPConnections()
	IncChatMessage(scope string)
	IncRTPForwarded(media string, n int)
	IncFileTransfer(direction, result string)
	IncAuthFailure(transport, reason string)
	IncRecordingError(operation string)
	IncChatCryptoFailure(operation string)
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
	registry     *prometheus.Registry
	dbPoolOnce   sync.Once
	stateOnce    sync.Once
	webrtcOnce   sync.Once
	udpOnce      sync.Once
	recorderOnce sync.Once

	callbacksMu          sync.RWMutex
	stateStats           func() (clients, channels int)
	webrtcPeerCount      func() int
	udpInboundQueueDepth func() int
	recorderSessionCount func() int

	udpPackets        *prometheus.CounterVec
	udpDropped        prometheus.Counter
	udpRateLimited    prometheus.Counter
	tcpConnections    prometheus.Counter
	chatMessages      *prometheus.CounterVec
	rtpForwarded      *prometheus.CounterVec
	fileTransfers     *prometheus.CounterVec
	authFailures      *prometheus.CounterVec
	recordingErrors   *prometheus.CounterVec
	chatCryptoFailure *prometheus.CounterVec
	readinessDuration *prometheus.HistogramVec
	broadcastDuration prometheus.Histogram
	broadcastBacklog  prometheus.Histogram
	buildInfo         *prometheus.GaugeVec
}

// New constructs a Metrics with its own registry (voicx_* metrics plus the
// default Go collectors).
func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	m := &Metrics{
		registry: reg,
		udpPackets: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "voicx", Name: "udp_packets_total",
			Help: "UDP packets processed by message kind.",
		}, []string{"kind"}),
		udpDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "voicx", Name: "udp_packets_dropped_total",
			Help: "UDP packets rejected or dropped by rate limiting, queue pressure, or protocol validation.",
		}),
		udpRateLimited: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "voicx", Name: "udp_packets_rate_limited_total",
			Help: "UDP packets dropped by the per-source rate limiter.",
		}),
		tcpConnections: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "voicx", Name: "tcp_connections_total",
			Help: "TCP control connections accepted.",
		}),
		chatMessages: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "voicx", Name: "chat_messages_total",
			Help: "Chat messages routed by scope (channel/direct/global).",
		}, []string{"scope"}),
		rtpForwarded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "voicx", Name: "rtp_packets_forwarded_total",
			Help: "RTP packets forwarded by the SFU by media type.",
		}, []string{"media"}),
		fileTransfers: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "voicx", Name: "file_transfers_total",
			Help: "File transfers by direction and result.",
		}, []string{"direction", "result"}),
		authFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "voicx", Name: "auth_failures_total",
			Help: "Rejected authentication attempts by transport and stable reason.",
		}, []string{"transport", "reason"}),
		recordingErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "voicx", Name: "recording_errors_total",
			Help: "Recording lifecycle failures by stable operation.",
		}, []string{"operation"}),
		chatCryptoFailure: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "voicx", Name: "chat_crypto_failures_total",
			Help: "Chat cryptography failures by stable public operation.",
		}, []string{"operation"}),
		readinessDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "voicx", Name: "readiness_probe_duration_seconds",
			Help:    "Duration of dependency readiness probes.",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"component", "result"}),
		broadcastDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "voicx", Name: "broadcast_snapshot_duration_seconds",
			Help:    "Time to build and marshal a broadcast snapshot.",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}),
		broadcastBacklog: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "voicx", Name: "broadcast_client_backlog_depth",
			Help:    "Client outbound queue depth after a broadcast send attempt.",
			Buckets: []float64{0, 1, 2, 4, 8, 12, 16},
		}),
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "voicx", Name: "build_info",
			Help: "Embedded build metadata (always 1).",
		}, []string{"version", "commit"}),
	}
	reg.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "voicx", Name: "clients_connected",
			Help: "Currently connected control-channel clients.",
		}, func() float64 {
			clients, _ := m.currentStateStats()
			return nonNegative(clients)
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "voicx", Name: "channels_active",
			Help: "Currently active channels.",
		}, func() float64 {
			_, channels := m.currentStateStats()
			return nonNegative(channels)
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "voicx", Name: "webrtc_peers",
			Help: "Active WebRTC peer connections.",
		}, func() float64 { return nonNegative(m.currentWebRTCPeerCount()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "voicx", Name: "udp_inbound_queue_depth",
			Help: "UDP packets waiting for worker processing.",
		}, func() float64 { return nonNegative(m.currentUDPInboundQueueDepth()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "voicx", Name: "recordings_active",
			Help: "Currently active recording sessions.",
		}, func() float64 { return nonNegative(m.currentRecorderSessionCount()) }),
		m.udpPackets, m.udpDropped, m.udpRateLimited, m.tcpConnections,
		m.chatMessages, m.rtpForwarded, m.fileTransfers, m.authFailures,
		m.recordingErrors, m.chatCryptoFailure, m.readinessDuration,
		m.broadcastDuration, m.broadcastBacklog, m.buildInfo,
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

func (m *Metrics) IncUDPPackets(kind string) {
	m.udpPackets.WithLabelValues(udpKindLabel(kind)).Inc()
}
func (m *Metrics) IncUDPPacketsDropped()     { m.udpDropped.Inc() }
func (m *Metrics) IncUDPPacketsRateLimited() { m.udpRateLimited.Inc() }
func (m *Metrics) IncTCPConnections()        { m.tcpConnections.Inc() }
func (m *Metrics) IncChatMessage(scope string) {
	m.chatMessages.WithLabelValues(chatScopeLabel(scope)).Inc()
}
func (m *Metrics) IncRTPForwarded(media string, n int) {
	if n <= 0 {
		return
	}
	m.rtpForwarded.WithLabelValues(mediaLabel(media)).Add(float64(n))
}
func (m *Metrics) IncFileTransfer(direction, result string) {
	m.fileTransfers.WithLabelValues(transferDirectionLabel(direction), transferResultLabel(result)).Inc()
}
func (m *Metrics) IncAuthFailure(transport, reason string) {
	m.authFailures.WithLabelValues(authTransportLabel(transport), authFailureReasonLabel(reason)).Inc()
}
func (m *Metrics) IncRecordingError(operation string) {
	m.recordingErrors.WithLabelValues(recordingOperationLabel(operation)).Inc()
}
func (m *Metrics) IncChatCryptoFailure(operation string) {
	m.chatCryptoFailure.WithLabelValues(chatCryptoOperationLabel(operation)).Inc()
}
func (m *Metrics) ObserveReadiness(component, result string, duration time.Duration) {
	m.readinessDuration.WithLabelValues(readinessComponentLabel(component), readinessResultLabel(result)).Observe(nonNegativeDurationSeconds(duration))
}
func (m *Metrics) ObserveBroadcastSnapshot(duration time.Duration) {
	m.broadcastDuration.Observe(nonNegativeDurationSeconds(duration))
}
func (m *Metrics) ObserveBroadcastClientBacklog(depth int) {
	m.broadcastBacklog.Observe(nonNegative(depth))
}

// RegisterStateStats installs the state snapshot callback used by the client
// and channel gauges. The first callback wins so composition roots can safely
// register the dependency more than once during staged startup.
func (m *Metrics) RegisterStateStats(stats func() (clients, channels int)) {
	if m == nil || stats == nil {
		return
	}
	m.stateOnce.Do(func() {
		m.callbacksMu.Lock()
		m.stateStats = stats
		m.callbacksMu.Unlock()
	})
}

// RegisterWebRTCPeerCount installs the scrape-time WebRTC peer counter.
func (m *Metrics) RegisterWebRTCPeerCount(peerCount func() int) {
	if m == nil || peerCount == nil {
		return
	}
	m.webrtcOnce.Do(func() {
		m.callbacksMu.Lock()
		m.webrtcPeerCount = peerCount
		m.callbacksMu.Unlock()
	})
}

// RegisterUDPInboundQueueDepth installs the scrape-time UDP queue callback.
func (m *Metrics) RegisterUDPInboundQueueDepth(queueDepth func() int) {
	if m == nil || queueDepth == nil {
		return
	}
	m.udpOnce.Do(func() {
		m.callbacksMu.Lock()
		m.udpInboundQueueDepth = queueDepth
		m.callbacksMu.Unlock()
	})
}

// RegisterRecorderSessionCount installs the scrape-time recording session callback.
func (m *Metrics) RegisterRecorderSessionCount(sessionCount func() int) {
	if m == nil || sessionCount == nil {
		return
	}
	m.recorderOnce.Do(func() {
		m.callbacksMu.Lock()
		m.recorderSessionCount = sessionCount
		m.callbacksMu.Unlock()
	})
}

func (m *Metrics) currentStateStats() (int, int) {
	m.callbacksMu.RLock()
	stats := m.stateStats
	m.callbacksMu.RUnlock()
	if stats == nil {
		return 0, 0
	}
	return stats()
}

func (m *Metrics) currentWebRTCPeerCount() int {
	m.callbacksMu.RLock()
	peerCount := m.webrtcPeerCount
	m.callbacksMu.RUnlock()
	if peerCount == nil {
		return 0
	}
	return peerCount()
}

func (m *Metrics) currentUDPInboundQueueDepth() int {
	m.callbacksMu.RLock()
	queueDepth := m.udpInboundQueueDepth
	m.callbacksMu.RUnlock()
	if queueDepth == nil {
		return 0
	}
	return queueDepth()
}

func (m *Metrics) currentRecorderSessionCount() int {
	m.callbacksMu.RLock()
	sessionCount := m.recorderSessionCount
	m.callbacksMu.RUnlock()
	if sessionCount == nil {
		return 0
	}
	return sessionCount()
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

func authTransportLabel(value string) string {
	switch value {
	case "tcp", "grpc", "query", "ssh", "ws":
		return value
	default:
		return "unknown"
	}
}

func authFailureReasonLabel(value string) string {
	switch value {
	case "invalid_credentials", "server_password", "not_admin", "locked_out", "banned", "malformed_metadata":
		return value
	default:
		return "unknown"
	}
}

func recordingOperationLabel(value string) string {
	switch value {
	case "start", "stop", "unexpected_exit":
		return value
	default:
		return "unknown"
	}
}

func chatCryptoOperationLabel(value string) string {
	switch value {
	case "ensure", "rotate", "seal_key", "encrypt", "decrypt":
		return value
	default:
		return "unknown"
	}
}

func readinessComponentLabel(value string) string {
	switch value {
	case "postgres", "redis", "storage":
		return value
	default:
		return "unknown"
	}
}

func readinessResultLabel(value string) string {
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

func nonNegativeDurationSeconds(value time.Duration) float64 {
	if value < 0 {
		return 0
	}
	return value.Seconds()
}

// Noop is a Sink that discards everything, for tests and partial startups.
type Noop struct{}

func (Noop) IncUDPPackets(string)           {}
func (Noop) IncUDPPacketsDropped()          {}
func (Noop) IncUDPPacketsRateLimited()      {}
func (Noop) IncTCPConnections()             {}
func (Noop) IncChatMessage(string)          {}
func (Noop) IncRTPForwarded(string, int)    {}
func (Noop) IncFileTransfer(string, string) {}
func (Noop) IncAuthFailure(string, string)  {}
func (Noop) IncRecordingError(string)       {}
func (Noop) IncChatCryptoFailure(string)    {}

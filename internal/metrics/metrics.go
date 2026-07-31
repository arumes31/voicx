// Package metrics wraps the Prometheus client library with a voicx-specific
// registry and a narrow Sink interface. Server components consume Sink (or
// the Noop implementation in tests) so no Prometheus calls are sprinkled
// through handlers.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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

// Metrics is the Prometheus-backed Sink.
type Metrics struct {
	registry *prometheus.Registry

	clientsConnected prometheus.Gauge
	channelsActive   prometheus.Gauge
	udpPackets       *prometheus.CounterVec
	udpDropped       prometheus.Counter
	tcpConnections   prometheus.Counter
	chatMessages     *prometheus.CounterVec
	webrtcPeers      prometheus.Gauge
	rtpForwarded     *prometheus.CounterVec
	fileTransfers    *prometheus.CounterVec
}

// New constructs a Metrics with its own registry (voicx_* metrics plus the
// default Go collectors).
func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())

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
	}
	reg.MustRegister(
		m.clientsConnected, m.channelsActive, m.udpPackets, m.udpDropped,
		m.tcpConnections, m.chatMessages, m.webrtcPeers, m.rtpForwarded,
		m.fileTransfers,
	)
	return m
}

// Registry returns the underlying registry (for tests).
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// Handler returns an HTTP handler serving the /metrics text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *Metrics) SetClientsConnected(n int)   { m.clientsConnected.Set(float64(n)) }
func (m *Metrics) SetChannelsActive(n int)     { m.channelsActive.Set(float64(n)) }
func (m *Metrics) IncUDPPackets(kind string)   { m.udpPackets.WithLabelValues(kind).Inc() }
func (m *Metrics) IncUDPPacketsDropped()       { m.udpDropped.Inc() }
func (m *Metrics) IncTCPConnections()          { m.tcpConnections.Inc() }
func (m *Metrics) IncChatMessage(scope string) { m.chatMessages.WithLabelValues(scope).Inc() }
func (m *Metrics) SetWebRTCPeers(n int)        { m.webrtcPeers.Set(float64(n)) }
func (m *Metrics) IncRTPForwarded(media string, n int) {
	m.rtpForwarded.WithLabelValues(media).Add(float64(n))
}
func (m *Metrics) IncFileTransfer(direction, result string) {
	m.fileTransfers.WithLabelValues(direction, result).Inc()
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

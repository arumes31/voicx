// Package server hosts the long-running voicx server components. This file
// implements the UDP keepalive listener: it reads datagrams into pooled
// buffers, dispatches them by a 1-byte message-type header to a bounded worker
// pool, and exposes basic atomic counters via Stats(). The UDP surface is a
// ping/pong connectivity probe only — media runs over WebRTC (DTLS-SRTP) and
// signaling over the TCP control channel; anything else is dropped.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"voicx/internal/config"
	"voicx/internal/metrics"
	"voicx/internal/netproto"
)

// udpMTU is the typical maximum transmission unit we size receive buffers
// for. 1500 bytes covers a standard Ethernet frame minus headers, which is
// more than enough for Opus media packets and small signaling fragments.
const udpMTU = 1500

// udpWorkerCount is the number of goroutines that drain the inbound packet
// channel. It is intentionally small to bound goroutine growth under load;
// the channel provides natural backpressure when workers fall behind.
const udpWorkerCount = 8

// udpInboundCap bounds the number of packets queued for worker processing.
// When full, the read loop drops new packets (incrementing packetsDropped)
// rather than blocking the reader or growing the queue unboundedly.
const udpInboundCap = 1024

// udpPacket is the unit of work handed from the read loop to a worker. It
// owns its buffer, which the worker must return to the pool when done.
type udpPacket struct {
	remote  *net.UDPAddr
	payload []byte // header byte + body, owned by this struct
}

// UDPStats is a point-in-time snapshot of UDP server counters.
type UDPStats struct {
	PacketsReceived    uint64
	PacketsDropped     uint64
	PacketsProcessed   uint64
	PacketsRateLimited uint64
}

// UDPServer reads and dispatches UDP keepalive datagrams. It uses a
// sync.Pool of MTU-sized buffers to avoid per-packet allocations and a
// bounded worker pool to avoid unbounded goroutine creation under load.
type UDPServer struct {
	cfg    *config.Config
	logger *zap.Logger
	conn   *net.UDPConn

	bufPool sync.Pool

	inbound chan udpPacket
	wg      sync.WaitGroup // workers

	rateLimiter *ipRateLimiter

	// Metrics is an optional metrics sink; nil means no-op.
	Metrics metrics.Sink

	stopOnce sync.Once
	stopCh   chan struct{}

	// Atomic counters. Use atomic.Load/Store to access.
	packetsReceived    atomic.Uint64
	packetsDropped     atomic.Uint64
	packetsProcessed   atomic.Uint64
	packetsRateLimited atomic.Uint64
}

// NewUDP constructs a UDPServer. The socket is created lazily in Start so
// that construction never blocks and Shutdown is always safe to call. When
// udp_rate_limit_pps is set, per-source-IP rate limiting is enabled.
func NewUDP(cfg *config.Config, logger *zap.Logger) *UDPServer {
	return &UDPServer{
		cfg:    cfg,
		logger: logger,
		bufPool: sync.Pool{
			New: func() any {
				b := make([]byte, udpMTU)
				return &b
			},
		},
		inbound:     make(chan udpPacket, udpInboundCap),
		stopCh:      make(chan struct{}),
		rateLimiter: newIPRateLimiter(cfg.UDPRateLimitPPS, cfg.UDPRateBurst),
	}
}

// Start resolves the configured UDPAddr, binds the socket, launches the
// worker pool, and runs the read loop until ctx is cancelled or Shutdown is
// called. It returns once the read loop has exited and workers have drained.
func (s *UDPServer) Start(ctx context.Context) error {
	addr, err := net.ResolveUDPAddr("udp", s.cfg.UDPAddr)
	if err != nil {
		return fmt.Errorf("resolve udp addr %s: %w", s.cfg.UDPAddr, err)
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("udp listen on %s: %w", s.cfg.UDPAddr, err)
	}
	s.conn = conn
	s.logger.Info("UDP media listener started", zap.String("addr", s.cfg.UDPAddr))

	// Close the socket when the context is cancelled so ReadFromUDP unblocks.
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Shutdown()
		case <-s.stopCh:
		}
	}()

	// Launch the bounded worker pool.
	n := udpWorkerCount
	if n < 1 {
		n = runtime.NumCPU()
	}
	for i := 0; i < n; i++ {
		s.wg.Add(1)
		go s.worker()
	}

	s.readLoop()

	// Drain workers: close the inbound channel and wait for them to finish
	// processing anything already queued.
	close(s.inbound)
	s.wg.Wait()
	s.logger.Info("UDP media listener stopped",
		zap.Uint64("received", s.packetsReceived.Load()),
		zap.Uint64("dropped", s.packetsDropped.Load()),
		zap.Uint64("processed", s.packetsProcessed.Load()),
	)
	return nil
}

// readLoop reads datagrams into pooled buffers and enqueues them for worker
// processing. It exits when the socket is closed (via Shutdown) or the stop
// channel is closed.
func (s *UDPServer) readLoop() {
	for {
		select {
		case <-s.stopCh:
			return
		default:
		}

		bufp := s.bufPool.Get().(*[]byte)
		buf := *bufp

		n, remote, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			// Return the buffer to the pool regardless of error path.
			s.bufPool.Put(bufp)
			if s.isClosed(err) {
				return
			}
			s.logger.Warn("udp read error", zap.Error(err))
			continue
		}

		// Per-source-IP rate limiting: over-budget packets are dropped and
		// counted before they cost any further processing.
		if s.rateLimiter != nil && !s.rateLimiter.allow(remote.IP.String(), time.Now()) {
			s.bufPool.Put(bufp)
			s.packetsRateLimited.Add(1)
			s.metric().IncUDPPacketsDropped()
			continue
		}

		s.packetsReceived.Add(1)

		// Copy the relevant slice into a buffer the worker owns. We cannot
		// hand the pooled buffer directly to the channel because the read
		// loop would reuse it on the next iteration; instead we allocate a
		// fresh buffer per packet. (A future optimization could use a
		// second pool of exactly-sized buffers.)
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		s.bufPool.Put(bufp)

		select {
		case s.inbound <- udpPacket{remote: remote, payload: pkt}:
		default:
			// Queue full: drop to apply backpressure rather than block the
			// reader or grow unboundedly.
			s.packetsDropped.Add(1)
			s.metric().IncUDPPacketsDropped()
			if s.logger.Core().Enabled(zap.DebugLevel) {
				s.logger.Debug("udp inbound queue full, dropping packet",
					zap.String("remote", remote.String()))
			}
		}
	}
}

// worker drains the inbound channel and dispatches packets to handlers.
func (s *UDPServer) worker() {
	defer s.wg.Done()
	for pkt := range s.inbound {
		s.dispatch(pkt)
		s.packetsProcessed.Add(1)
	}
}

// dispatch parses the header byte and routes the packet to the appropriate
// stub handler. Unknown or malformed packets are logged and dropped.
func (s *UDPServer) dispatch(pkt udpPacket) {
	msgType, payload, err := netproto.ParseUDPHeader(pkt.payload)
	if err != nil {
		s.packetsDropped.Add(1)
		s.metric().IncUDPPacketsDropped()
		s.logger.Warn("udp malformed packet",
			zap.String("remote", pkt.remote.String()),
			zap.Error(err),
		)
		return
	}

	switch msgType {
	case netproto.UDPMsgPing:
		s.metric().IncUDPPackets("ping")
		s.handlePing(pkt.remote, payload)
	case netproto.UDPMsgPong:
		s.metric().IncUDPPackets("pong")
		// Unsolicited pong from a peer; nothing to do.
		s.logger.Debug("udp pong received", zap.String("remote", pkt.remote.String()))
	default:
		s.packetsDropped.Add(1)
		s.metric().IncUDPPacketsDropped()
		s.logger.Warn("udp unknown message type",
			zap.String("remote", pkt.remote.String()),
			zap.Uint8("msg_type", msgType),
		)
	}
}

// metric returns the metrics sink, or a no-op when none is wired.
func (s *UDPServer) metric() metrics.Sink {
	if s.Metrics == nil {
		return metrics.Noop{}
	}
	return s.Metrics
}

// handlePing replies with a Pong datagram to the sender's address.
func (s *UDPServer) handlePing(remote *net.UDPAddr, _ []byte) {
	if _, err := s.conn.WriteToUDP([]byte{netproto.UDPMsgPong}, remote); err != nil {
		if !s.isClosed(err) {
			s.logger.Warn("udp pong write error",
				zap.String("remote", remote.String()),
				zap.Error(err),
			)
		}
	}
}

// Shutdown closes the UDP socket and signals the read loop to exit. It is
// safe to call multiple times.
func (s *UDPServer) Shutdown() error {
	var err error
	s.stopOnce.Do(func() {
		close(s.stopCh)
		if s.conn != nil {
			err = s.conn.Close()
		}
		if s.rateLimiter != nil {
			s.rateLimiter.close()
		}
	})
	return err
}

// Stats returns a snapshot of the UDP server counters.
func (s *UDPServer) Stats() UDPStats {
	return UDPStats{
		PacketsReceived:    s.packetsReceived.Load(),
		PacketsDropped:     s.packetsDropped.Load(),
		PacketsProcessed:   s.packetsProcessed.Load(),
		PacketsRateLimited: s.packetsRateLimited.Load(),
	}
}

// isClosed reports whether err indicates a closed socket, which is expected
// during shutdown and should not be logged as a warning.
func (s *UDPServer) isClosed(err error) bool {
	return errors.Is(err, net.ErrClosed)
}

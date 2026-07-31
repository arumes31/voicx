// udp_ratelimit.go implements per-source-IP token-bucket rate limiting for
// the UDP media listener (DDoS mitigation). The bucket map is bounded by a
// periodic sweep that evicts idle entries, so a flood of spoofed source IPs
// cannot exhaust memory.
package server

import (
	"sync"
	"time"
)

// sweepInterval is how often idle buckets are evicted.
const sweepInterval = time.Minute

// bucketIdleTTL is how long a bucket may sit unused before eviction.
const bucketIdleTTL = 2 * time.Minute

// ipBucket is one source IP's token bucket.
type ipBucket struct {
	tokens float64
	last   time.Time
	seen   time.Time
}

// ipRateLimiter tracks per-IP token buckets. It is safe for concurrent use.
type ipRateLimiter struct {
	pps   float64
	burst float64

	mu      sync.Mutex
	buckets map[string]*ipBucket

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// newIPRateLimiter returns a limiter allowing pps packets per second per IP
// with the given burst, or nil when pps <= 0 (rate limiting disabled).
func newIPRateLimiter(pps, burst int) *ipRateLimiter {
	if pps <= 0 {
		return nil
	}
	if burst <= 0 {
		burst = pps
	}
	l := &ipRateLimiter{
		pps:     float64(pps),
		burst:   float64(burst),
		buckets: make(map[string]*ipBucket),
		stopCh:  make(chan struct{}),
	}
	l.wg.Add(1)
	go l.sweepLoop()
	return l
}

// allow reports whether a packet from ip is within budget at time now.
func (l *ipRateLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[ip]
	if !ok {
		b = &ipBucket{tokens: l.burst, last: now}
		l.buckets[ip] = b
	}
	b.seen = now
	b.tokens += now.Sub(b.last).Seconds() * l.pps
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// bucketCount returns the number of tracked IPs (for tests/observability).
func (l *ipRateLimiter) bucketCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// sweepLoop periodically evicts buckets idle longer than bucketIdleTTL.
func (l *ipRateLimiter) sweepLoop() {
	defer l.wg.Done()
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-l.stopCh:
			return
		case now := <-ticker.C:
			l.sweep(now)
		}
	}
}

// sweep evicts idle buckets. It is called by sweepLoop and by tests.
func (l *ipRateLimiter) sweep(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, b := range l.buckets {
		if now.Sub(b.seen) > bucketIdleTTL {
			delete(l.buckets, ip)
		}
	}
}

// close stops the sweep goroutine.
func (l *ipRateLimiter) close() {
	close(l.stopCh)
	l.wg.Wait()
}

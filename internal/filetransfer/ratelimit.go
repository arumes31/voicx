// ratelimit.go implements a simple token-bucket bandwidth limiter (the
// module has no rate-limit dependency, so this is hand-rolled).
package filetransfer

import (
	"context"
	"sync"
	"time"
)

// rateLimiter paces byte flow to at most bytesPerSec. A zero rateLimiter is
// unlimited.
type rateLimiter struct {
	bytesPerSec float64
	burst       float64

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// inQuietHours reports whether hour falls inside the [start, end) window
// (276). Equal bounds disable the window; a start after the end wraps past
// midnight, which is the shape an overnight window actually has (22 -> 6).
func inQuietHours(hour, start, end int) bool {
	if start == end || start < 0 || start > 23 || end < 0 || end > 23 {
		return false
	}
	if start < end {
		return hour >= start && hour < end
	}
	return hour >= start || hour < end
}

// limiterFor returns the limiter a transfer starting now should use: the
// configured cap, or an unlimited one inside the quiet-hours window (276).
func (s *Server) limiterFor(now time.Time) *rateLimiter {
	if inQuietHours(now.Hour(), s.cfg.QuietHoursStart, s.cfg.QuietHoursEnd) {
		return newRateLimiter(0)
	}
	return newRateLimiter(s.cfg.MaxKBps)
}

// newRateLimiter returns a limiter for the given KiB/s cap. kbps <= 0 means
// unlimited (wait never blocks).
func newRateLimiter(kbps int) *rateLimiter {
	if kbps <= 0 {
		return &rateLimiter{}
	}
	rate := float64(kbps) * 1024
	return &rateLimiter{
		bytesPerSec: rate,
		burst:       rate, // one second of burst
		tokens:      rate,
		last:        time.Now(),
	}
}

// wait blocks until n bytes have been allowed by the bucket or ctx is
// cancelled. Bytes are consumed in installments, so n may exceed the burst
// size.
func (r *rateLimiter) wait(ctx context.Context, n int) error {
	if r.bytesPerSec <= 0 {
		return nil
	}
	remaining := float64(n)
	for remaining > 0 {
		r.mu.Lock()
		now := time.Now()
		r.tokens += now.Sub(r.last).Seconds() * r.bytesPerSec
		if r.tokens > r.burst {
			r.tokens = r.burst
		}
		r.last = now

		if r.tokens > 0 {
			take := r.tokens
			if take > remaining {
				take = remaining
			}
			r.tokens -= take
			remaining -= take
			r.mu.Unlock()
			continue
		}
		r.mu.Unlock()

		// Bucket empty: sleep for a fraction of the refill time (capped to
		// stay responsive to cancellation).
		delay := time.Duration(remaining/r.bytesPerSec*float64(time.Second)) + time.Millisecond
		if delay > 100*time.Millisecond {
			delay = 100 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil
}

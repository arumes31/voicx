package server

import (
	"testing"
	"time"
)

// TestIPRateLimiterAllowDeny verifies bucket semantics: burst is allowed,
// sustained traffic above the rate is denied, and tokens refill over time.
func TestIPRateLimiterAllowDeny(t *testing.T) {
	l := newIPRateLimiter(10, 20)
	defer l.close()
	now := time.Now()

	// Full burst allowed.
	for i := 0; i < 20; i++ {
		if !l.allow("1.2.3.4", now) {
			t.Fatalf("packet %d within burst denied", i)
		}
	}
	// Bucket empty: denied.
	if l.allow("1.2.3.4", now) {
		t.Fatal("packet over burst allowed")
	}

	// After 1s, 10 tokens refilled.
	now = now.Add(time.Second)
	for i := 0; i < 10; i++ {
		if !l.allow("1.2.3.4", now) {
			t.Fatalf("refilled packet %d denied", i)
		}
	}
	if l.allow("1.2.3.4", now) {
		t.Fatal("packet over refilled budget allowed")
	}

	// Other IPs have their own bucket.
	if !l.allow("5.6.7.8", now) {
		t.Fatal("other IP denied")
	}
}

// TestIPRateLimiterSweep verifies idle buckets are evicted and active ones
// are kept.
func TestIPRateLimiterSweep(t *testing.T) {
	l := newIPRateLimiter(10, 10)
	defer l.close()
	now := time.Now()

	l.allow("1.1.1.1", now)
	l.allow("2.2.2.2", now)
	if got := l.bucketCount(); got != 2 {
		t.Fatalf("buckets = %d, want 2", got)
	}

	// Sweep with both idle: all evicted.
	l.sweep(now.Add(bucketIdleTTL + time.Second))
	if got := l.bucketCount(); got != 0 {
		t.Fatalf("buckets after sweep = %d, want 0", got)
	}

	// A recently-seen bucket survives the sweep.
	l.allow("3.3.3.3", now.Add(bucketIdleTTL+2*time.Second))
	l.sweep(now.Add(bucketIdleTTL + 2*time.Second))
	if got := l.bucketCount(); got != 1 {
		t.Fatalf("buckets after second sweep = %d, want 1", got)
	}
}

// TestIPRateLimiterDisabled verifies pps <= 0 disables the limiter.
func TestIPRateLimiterDisabled(t *testing.T) {
	if l := newIPRateLimiter(0, 0); l != nil {
		t.Fatal("newIPRateLimiter(0) returned non-nil limiter")
	}
	if l := newIPRateLimiter(-5, 0); l != nil {
		t.Fatal("newIPRateLimiter(-5) returned non-nil limiter")
	}
}

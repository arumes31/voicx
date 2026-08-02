// follow_test.go covers the live log subscription behind `logview follow`
// (223).
package logging

import (
	"testing"
	"time"

	"go.uber.org/zap"
)

// isolateRing empties the global ring for one test and puts the previous
// contents back afterwards, so writing test log lines cannot disturb a test
// that asserts on the ring's contents.
func isolateRing(t *testing.T) {
	t.Helper()
	globalRing.mu.Lock()
	saved := globalRing.lines
	globalRing.lines = nil
	globalRing.mu.Unlock()
	t.Cleanup(func() {
		globalRing.mu.Lock()
		globalRing.lines = saved
		globalRing.mu.Unlock()
	})
}

// TestFollowStreamsNewLines verifies a follower receives lines emitted after
// it subscribed, and stops receiving after it cancels.
func TestFollowStreamsNewLines(t *testing.T) {
	isolateRing(t)
	logger, err := zap.NewDevelopment()
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	logger = logger.WithOptions(Tee())

	lines, cancel := Follow()
	logger.Info("follow-marker-one")

	select {
	case line := <-lines:
		if line == "" {
			t.Fatal("empty line")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no line delivered to the follower")
	}

	cancel()
	cancel() // idempotent
	logger.Info("follow-marker-two")
	select {
	case line := <-lines:
		t.Fatalf("cancelled follower still received %q", line)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestFollowDropsWhenNotDrained verifies a stalled follower never blocks a log
// write.
func TestFollowDropsWhenNotDrained(t *testing.T) {
	isolateRing(t)
	logger, err := zap.NewDevelopment()
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	logger = logger.WithOptions(Tee())

	_, cancel := Follow()
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < followBuffer*2; i++ {
			logger.Info("flood")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("logging blocked on a follower that never reads")
	}
}

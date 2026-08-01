// ringbuf_test.go exercises the log ring buffer and the zap tee (223).
package logging

import (
	"testing"

	"go.uber.org/zap"
)

// TestRingBufTee verifies log lines land in the ring and Recent returns them
// chronologically with filtering.
func TestRingBufTee(t *testing.T) {
	logger := zap.NewExample().WithOptions(Tee())
	logger.Info("first message")
	logger.Warn("second message with marker")

	lines := Recent(10, "")
	if len(lines) != 2 {
		t.Fatalf("recent = %v", lines)
	}
	if lines[0] > lines[1] || !contains(lines[0], "first message") {
		t.Fatalf("lines = %v", lines)
	}

	filtered := Recent(10, "marker")
	if len(filtered) != 1 || !contains(filtered[0], "second message") {
		t.Fatalf("filtered = %v", filtered)
	}

	if none := Recent(10, "no-such-substring"); len(none) != 0 {
		t.Fatalf("unexpected match: %v", none)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

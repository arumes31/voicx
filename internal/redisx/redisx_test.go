package redisx

import (
	"context"
	"testing"
	"time"
)

// TestNew verifies the constructor wires the address and password into the
// underlying client options.
func TestNew(t *testing.T) {
	c := New("localhost:6390", "secret", nil)
	if c == nil {
		t.Fatal("New returned nil")
	}
	opts := c.Raw().Options()
	if opts.Addr != "localhost:6390" {
		t.Errorf("Addr = %q, want %q", opts.Addr, "localhost:6390")
	}
	if opts.Password != "secret" {
		t.Errorf("Password = %q, want %q", opts.Password, "secret")
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestPingUnreachable verifies Ping returns an error (rather than panicking
// or hanging) when no Redis server is reachable, which is what the caller
// relies on for graceful degradation.
func TestPingUnreachable(t *testing.T) {
	// Port 1 on loopback refuses connections immediately.
	c := New("127.0.0.1:1", "", nil)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err == nil {
		t.Fatal("Ping against unreachable Redis returned nil error")
	}
}

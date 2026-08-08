package webrtc

import (
	"testing"

	"go.uber.org/zap"
)

// testLogger returns a discard logger suitable for tests.
func testLogger() *zap.Logger {
	logger, _ := zap.NewDevelopment()
	return logger
}

// TestNewEngine verifies that New returns a non-nil Engine.
func TestNewEngine(t *testing.T) {
	e, err := New(testLogger(), nil, false)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if e == nil {
		t.Fatal("New returned nil Engine")
	}
	defer func() { _ = e.Close() }()
}

// TestNewEngineNilLogger verifies that New returns an error when the logger
// is nil.
func TestNewEngineNilLogger(t *testing.T) {
	e, err := New(nil, nil, false)
	if err == nil {
		t.Fatal("New(nil logger) expected error, got nil")
	}
	if e != nil {
		t.Errorf("New(nil logger) expected nil engine, got %v", e)
	}
}

// TestNewPeerConnection verifies that NewPeerConnection returns a non-nil
// PeerConnectionWrapper and that ClosePeerConnection cleans it up so that
// PeerCount drops back to 0.
func TestNewPeerConnection(t *testing.T) {
	e, err := New(testLogger(), nil, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = e.Close() }()

	w, err := e.NewPeerConnection("client-1")
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	if w == nil {
		t.Fatal("NewPeerConnection returned nil wrapper")
	}
	if got := e.PeerCount(); got != 1 {
		t.Errorf("PeerCount = %d, want 1", got)
	}

	if err := e.ClosePeerConnection("client-1"); err != nil {
		t.Fatalf("ClosePeerConnection: %v", err)
	}
	if got := e.PeerCount(); got != 0 {
		t.Errorf("PeerCount after close = %d, want 0", got)
	}
}

// TestNewPeerConnectionDuplicate verifies that creating a peer connection
// for an existing clientID returns an error.
func TestNewPeerConnectionDuplicate(t *testing.T) {
	e, err := New(testLogger(), nil, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = e.Close() }()

	if _, err := e.NewPeerConnection("client-dup"); err != nil {
		t.Fatalf("first NewPeerConnection: %v", err)
	}
	if _, err := e.NewPeerConnection("client-dup"); err == nil {
		t.Fatal("second NewPeerConnection expected error, got nil")
	}
}

// TestNewPeerConnectionEmptyID verifies that an empty clientID is rejected.
func TestNewPeerConnectionEmptyID(t *testing.T) {
	e, err := New(testLogger(), nil, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = e.Close() }()

	if _, err := e.NewPeerConnection(""); err == nil {
		t.Fatal("NewPeerConnection(\"\") expected error, got nil")
	}
}

// TestEngineClose verifies that Close shuts down all registered peer
// connections without error and that the engine is marked closed.
func TestEngineClose(t *testing.T) {
	e, err := New(testLogger(), nil, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, id := range []string{"a", "b", "c"} {
		if _, err := e.NewPeerConnection(id); err != nil {
			t.Fatalf("NewPeerConnection(%s): %v", id, err)
		}
	}
	if got := e.PeerCount(); got != 3 {
		t.Errorf("PeerCount = %d, want 3", got)
	}

	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := e.PeerCount(); got != 0 {
		t.Errorf("PeerCount after Close = %d, want 0", got)
	}

	// Close is idempotent.
	if err := e.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	// After Close, NewPeerConnection should fail.
	if _, err := e.NewPeerConnection("after-close"); err == nil {
		t.Fatal("NewPeerConnection after Close expected error, got nil")
	}
}

// TestClosePeerConnectionMissing verifies that ClosePeerConnection is a no-op
// (returns nil) for an unknown clientID.
func TestClosePeerConnectionMissing(t *testing.T) {
	e, err := New(testLogger(), nil, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = e.Close() }()

	if err := e.ClosePeerConnection("does-not-exist"); err != nil {
		t.Errorf("ClosePeerConnection(missing) = %v, want nil", err)
	}
}

// TestPeerConnectionLookup verifies that PeerConnection returns the registered
// wrapper and nil for unknown clientIDs.
func TestPeerConnectionLookup(t *testing.T) {
	e, err := New(testLogger(), nil, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = e.Close() }()

	if w := e.PeerConnection("missing"); w != nil {
		t.Errorf("PeerConnection(missing) = %v, want nil", w)
	}

	w, err := e.NewPeerConnection("client-lookup")
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	if got := e.PeerConnection("client-lookup"); got != w {
		t.Errorf("PeerConnection returned %v, want %v", got, w)
	}
}

package broadcast

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"voicx/internal/state"
)

func newTestBroadcaster(t *testing.T) (*Broadcaster, *state.Manager) {
	t.Helper()
	sm := state.New(zap.NewNop())
	return New(zap.NewNop(), sm), sm
}

func TestRegisterUnregisterLifecycle(t *testing.T) {
	b, _ := newTestBroadcaster(t)
	defer b.Close()

	ch, err := b.Register("c1")
	if err != nil {
		t.Fatalf("Register err = %v", err)
	}
	if ch == nil {
		t.Fatal("Register returned nil channel")
	}
	if b.ClientCount() != 1 {
		t.Fatalf("ClientCount = %d, want 1", b.ClientCount())
	}

	b.Unregister("c1")
	if b.ClientCount() != 0 {
		t.Fatalf("ClientCount after Unregister = %d, want 0", b.ClientCount())
	}

	// Unregister unknown is a no-op.
	b.Unregister("does-not-exist")
}

func TestRegisterTwiceError(t *testing.T) {
	b, _ := newTestBroadcaster(t)
	defer b.Close()

	if _, err := b.Register("c1"); err != nil {
		t.Fatalf("first Register err = %v", err)
	}
	if _, err := b.Register("c1"); !errors.Is(err, ErrAlreadyRegistered) {
		t.Fatalf("second Register err = %v, want ErrAlreadyRegistered", err)
	}
}

func TestBroadcastSnapshotDeliversToAll(t *testing.T) {
	b, sm := newTestBroadcaster(t)
	defer b.Close()

	// Populate state so the snapshot has content.
	sm.AddChannel(&state.Channel{ChannelID: 1, Name: "root", CreatedAt: time.Now()})

	ch1, _ := b.Register("c1")
	ch2, _ := b.Register("c2")

	b.BroadcastSnapshot(false, "")

	for i, ch := range []<-chan []byte{ch1, ch2} {
		select {
		case msg := <-ch:
			if !strings.Contains(string(msg), "root_channels") {
				t.Fatalf("client %d got invalid snapshot JSON: %s", i, string(msg))
			}
		case <-time.After(time.Second):
			t.Fatalf("client %d did not receive snapshot", i)
		}
	}
}

// TestBroadcastSnapshotHidesInvisible verifies the viewer/admin flags reach
// BuildSnapshot: a fan-out must not leak invisible users (381).
func TestBroadcastSnapshotHidesInvisible(t *testing.T) {
	b, sm := newTestBroadcaster(t)
	defer b.Close()

	sm.AddChannel(&state.Channel{ChannelID: 1, Name: "root", CreatedAt: time.Now()})
	sm.AddClient(&state.Client{ClientID: "g1", UniqueID: "ghost-uid", Nickname: "Ghost", Status: "invisible", ConnectedAt: time.Now()})
	_ = sm.JoinChannel("g1", 1)

	ch, _ := b.Register("c1")
	b.BroadcastSnapshot(false, "")

	select {
	case msg := <-ch:
		if strings.Contains(string(msg), "Ghost") {
			t.Fatalf("invisible user leaked into the fan-out snapshot: %s", string(msg))
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive snapshot")
	}
}

func TestBroadcastToChannelScoped(t *testing.T) {
	b, sm := newTestBroadcaster(t)
	defer b.Close()

	sm.AddChannel(&state.Channel{ChannelID: 10, Name: "ch10", CreatedAt: time.Now()})
	sm.AddChannel(&state.Channel{ChannelID: 20, Name: "ch20", CreatedAt: time.Now()})

	c1 := &state.Client{ClientID: "c1", ConnectedAt: time.Now()}
	c2 := &state.Client{ClientID: "c2", ConnectedAt: time.Now()}
	sm.AddClient(c1)
	sm.AddClient(c2)
	_ = sm.JoinChannel("c1", 10)
	_ = sm.JoinChannel("c2", 20)

	ch1, _ := b.Register("c1")
	ch2, _ := b.Register("c2")

	payload := []byte("hello-ch10")
	b.BroadcastToChannel(10, payload)

	select {
	case msg := <-ch1:
		if string(msg) != "hello-ch10" {
			t.Fatalf("c1 got %q, want hello-ch10", string(msg))
		}
	case <-time.After(time.Second):
		t.Fatal("c1 did not receive channel broadcast")
	}

	select {
	case msg := <-ch2:
		t.Fatalf("c2 should not receive ch10 broadcast, got %q", string(msg))
	case <-time.After(100 * time.Millisecond):
		// expected: no message
	}
}

func TestBroadcastToClient(t *testing.T) {
	b, _ := newTestBroadcaster(t)
	defer b.Close()

	ch, err := b.Register("c1")
	if err != nil {
		t.Fatalf("Register err = %v", err)
	}

	payload := []byte("direct")
	if err := b.BroadcastToClient("c1", payload); err != nil {
		t.Fatalf("BroadcastToClient err = %v", err)
	}
	select {
	case msg := <-ch:
		if string(msg) != "direct" {
			t.Fatalf("got %q, want direct", string(msg))
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive direct message")
	}

	// Unregistered client returns ErrNotRegistered.
	if err := b.BroadcastToClient("ghost", payload); !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("BroadcastToClient unknown err = %v, want ErrNotRegistered", err)
	}
}

func TestBroadcastToClientFullChannelDrops(t *testing.T) {
	b, _ := newTestBroadcaster(t)
	defer b.Close()

	ch, _ := b.Register("c1")

	// Fill the buffer (clientBufferSize messages).
	for i := 0; i < clientBufferSize; i++ {
		if err := b.BroadcastToClient("c1", []byte("fill")); err != nil {
			t.Fatalf("fill %d err = %v", i, err)
		}
	}

	// The (N+1)th send must return ErrChannelFull (non-blocking drop).
	if err := b.BroadcastToClient("c1", []byte("overflow")); !errors.Is(err, ErrChannelFull) {
		t.Fatalf("overflow err = %v, want ErrChannelFull", err)
	}

	// Drain and verify we got exactly clientBufferSize messages.
	count := 0
	drain := time.After(time.Second)
loop:
	for {
		select {
		case <-ch:
			count++
		case <-drain:
			break loop
		}
	}
	if count != clientBufferSize {
		t.Fatalf("drained %d messages, want %d", count, clientBufferSize)
	}
}

func TestBroadcastEventEnvelope(t *testing.T) {
	b, _ := newTestBroadcaster(t)
	defer b.Close()

	ch, _ := b.Register("c1")

	b.BroadcastEvent("user_joined", []byte(`{"client_id":"c2"}`))

	select {
	case msg := <-ch:
		var env struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(msg, &env); err != nil {
			t.Fatalf("unmarshal envelope: %v", err)
		}
		if env.Type != "user_joined" {
			t.Fatalf("envelope type = %q, want user_joined", env.Type)
		}
		if strings.TrimSpace(string(env.Data)) != `{"client_id":"c2"}` {
			t.Fatalf("envelope data = %q, want client_id payload", string(env.Data))
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive event")
	}
}

func TestCloseClearsRegistry(t *testing.T) {
	b, _ := newTestBroadcaster(t)

	ch, _ := b.Register("c1")
	b.Close()

	if b.ClientCount() != 0 {
		t.Fatalf("ClientCount after Close = %d, want 0", b.ClientCount())
	}

	// Channel should be closed.
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("channel should be closed after Close")
		}
	default:
		t.Fatal("channel should be closed (received value or zero)")
	}

	// Register after close should error.
	if _, err := b.Register("c2"); err == nil {
		t.Fatal("Register after Close should error")
	}
}

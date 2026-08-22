package broadcast

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

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

func TestTelemetryObserversRunAfterBroadcastLocks(t *testing.T) {
	sm := state.New(zap.NewNop())
	var broadcaster *Broadcaster
	snapshotDuration := make(chan time.Duration, 1)
	backlogDepth := make(chan int, 1)
	var unregisterOnce sync.Once
	broadcaster = New(zap.NewNop(), sm, Observers{
		ObserveSnapshotDuration: func(duration time.Duration) {
			_ = broadcaster.Stats()
			snapshotDuration <- duration
		},
		ObserveClientBacklog: func(depth int) {
			// Re-enter a write-locked operation. If telemetry ran under mu or
			// sendMu this blocks the broadcast rather than completing below.
			unregisterOnce.Do(func() { broadcaster.Unregister("c1") })
			backlogDepth <- depth
		},
	})
	defer broadcaster.Close()
	if _, err := broadcaster.Register("c1"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	done := make(chan struct{})
	go func() {
		broadcaster.BroadcastSnapshot(false, "")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("broadcast blocked while invoking telemetry observer")
	}
	select {
	case duration := <-snapshotDuration:
		if duration < 0 {
			t.Fatalf("snapshot duration = %s, want non-negative", duration)
		}
	case <-time.After(time.Second):
		t.Fatal("snapshot observer was not called")
	}
	if extra := len(snapshotDuration); extra != 0 {
		t.Fatalf("snapshot observer calls = %d, want exactly 1", extra+1)
	}
	select {
	case depth := <-backlogDepth:
		if depth < 0 || depth > clientBufferSize {
			t.Fatalf("backlog depth = %d, want 0..%d", depth, clientBufferSize)
		}
	case <-time.After(time.Second):
		t.Fatal("backlog observer was not called")
	}
}

func TestBacklogObserverReportsFullQueueDepth(t *testing.T) {
	var depths []int
	var depthsMu sync.Mutex
	b := New(zap.NewNop(), state.New(zap.NewNop()), Observers{
		ObserveClientBacklog: func(depth int) {
			depthsMu.Lock()
			depths = append(depths, depth)
			depthsMu.Unlock()
		},
	})
	defer b.Close()
	if _, err := b.Register("c1"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for range clientBufferSize {
		if err := b.BroadcastToClient("c1", []byte("fill")); err != nil {
			t.Fatalf("fill queue: %v", err)
		}
	}
	if err := b.BroadcastToClient("c1", []byte("full")); !errors.Is(err, ErrChannelFull) {
		t.Fatalf("full queue error = %v, want ErrChannelFull", err)
	}
	depthsMu.Lock()
	defer depthsMu.Unlock()
	if len(depths) != clientBufferSize+1 {
		t.Fatalf("backlog observations = %d, want %d", len(depths), clientBufferSize+1)
	}
	if got := depths[len(depths)-1]; got != clientBufferSize {
		t.Fatalf("full queue backlog depth = %d, want %d", got, clientBufferSize)
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
	stats := b.Stats()
	if stats.Delivered != clientBufferSize || stats.Dropped != 1 || stats.Clients != 1 {
		t.Fatalf("broadcast stats = %+v", stats)
	}
}

func TestBroadcastPayloadOwnership(t *testing.T) {
	b, sm := newTestBroadcaster(t)
	defer b.Close()
	sm.AddChannel(&state.Channel{ChannelID: 10, Name: "shared", CreatedAt: time.Now()})
	for _, id := range []string{"c1", "c2"} {
		sm.AddClient(&state.Client{ClientID: id, ConnectedAt: time.Now()})
		if err := sm.JoinChannel(id, 10); err != nil {
			t.Fatalf("JoinChannel(%s): %v", id, err)
		}
	}
	first, _ := b.Register("c1")
	second, _ := b.Register("c2")
	payload := []byte("immutable")
	b.BroadcastToChannel(10, payload)
	payload[0] = 'X'

	firstPayload := <-first
	firstPayload[1] = 'Y'
	if got := string(<-second); got != "immutable" {
		t.Fatalf("second client payload was aliased: %q", got)
	}
}

func TestConcurrentBroadcastsHaveConsistentRecipientOrder(t *testing.T) {
	b, sm := newTestBroadcaster(t)
	defer b.Close()
	sm.AddChannel(&state.Channel{ChannelID: 10, Name: "shared", CreatedAt: time.Now()})
	for _, id := range []string{"c1", "c2"} {
		sm.AddClient(&state.Client{ClientID: id, ConnectedAt: time.Now()})
		if err := sm.JoinChannel(id, 10); err != nil {
			t.Fatal(err)
		}
	}
	first, _ := b.Register("c1")
	second, _ := b.Register("c2")

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < clientBufferSize; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			b.BroadcastToChannel(10, []byte(fmt.Sprintf("event-%02d", id)))
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < clientBufferSize; i++ {
		one, two := string(<-first), string(<-second)
		if one != two {
			t.Fatalf("recipient order diverged at %d: %q != %q", i, one, two)
		}
	}
}

func TestDropWarningsAreLogarithmicallyBounded(t *testing.T) {
	core, observed := observer.New(zapcore.WarnLevel)
	b := New(zap.New(core), state.New(zap.NewNop()))
	defer b.Close()
	_, _ = b.Register("stalled")
	for i := 0; i < clientBufferSize+17; i++ {
		_ = b.BroadcastToClient("stalled", []byte("message"))
	}

	const wantWarnings = 5 // client drop counts 1, 2, 4, 8, and 16.
	if got := observed.FilterMessage("broadcast: client channel full, dropping message").Len(); got != wantWarnings {
		t.Fatalf("drop warning count = %d, want %d", got, wantWarnings)
	}
	if got := b.Stats().Dropped; got != 17 {
		t.Fatalf("drop metric = %d, want 17", got)
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
	if _, err := b.Register("c2"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Register after Close error = %v, want ErrClosed", err)
	}
	if err := b.BroadcastToClient("c1", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("BroadcastToClient after Close error = %v, want ErrClosed", err)
	}
}

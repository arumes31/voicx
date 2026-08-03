// eventtap_test.go covers the event-bus seam on the broadcast fan-out (231).
package broadcast

import (
	"sync"
	"testing"

	"go.uber.org/zap"

	"voicx/internal/state"
)

// TestSetEventTapObservesEvents verifies the tap sees every server-wide event
// with its raw payload, and that clients still receive the envelope.
func TestSetEventTapObservesEvents(t *testing.T) {
	b := New(zap.NewNop(), state.New(zap.NewNop()))
	defer b.Close()

	var (
		mu     sync.Mutex
		types  []string
		bodies []string
	)
	b.SetEventTap(func(eventType string, payload []byte) {
		mu.Lock()
		defer mu.Unlock()
		types = append(types, eventType)
		bodies = append(bodies, string(payload))
	})

	out, err := b.Register("c-1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	b.BroadcastEvent("user_joined", []byte(`{"client_id":"c-1"}`))

	mu.Lock()
	defer mu.Unlock()
	if len(types) != 1 || types[0] != "user_joined" {
		t.Fatalf("tap types = %v", types)
	}
	if bodies[0] != `{"client_id":"c-1"}` {
		t.Fatalf("tap payload = %q", bodies[0])
	}
	select {
	case msg := <-out:
		if string(msg) != `{"type":"user_joined","data":{"client_id":"c-1"}}` {
			t.Fatalf("client envelope = %s", msg)
		}
	default:
		t.Fatal("the tap swallowed the client broadcast")
	}
}

// TestEventTapIsOptional verifies BroadcastEvent works with no tap installed.
func TestEventTapIsOptional(t *testing.T) {
	b := New(zap.NewNop(), state.New(zap.NewNop()))
	defer b.Close()
	b.BroadcastEvent("user_left", []byte(`{}`))
}

package eventbus

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// recv reads one event or fails.
func recv(t *testing.T, sub *Subscription) Event {
	t.Helper()
	select {
	case evt, ok := <-sub.C:
		if !ok {
			t.Fatal("subscription closed")
		}
		return evt
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an event")
		return Event{}
	}
}

// TestPublishFanOut verifies every subscriber sees every event, in order (231).
func TestPublishFanOut(t *testing.T) {
	bus := New(zap.NewNop())
	defer bus.Close()

	a := bus.Subscribe("a", nil, 0)
	b := bus.Subscribe("b", nil, 0)

	bus.Publish("user_joined", []byte(`{"client_id":"c-1"}`))
	bus.Publish("user_left", []byte(`{"client_id":"c-1"}`))

	for _, sub := range []*Subscription{a, b} {
		first := recv(t, sub)
		if first.Type != "user_joined" || first.Seq != 1 {
			t.Fatalf("first event = %+v", first)
		}
		if string(first.Data) != `{"client_id":"c-1"}` {
			t.Fatalf("payload = %s", first.Data)
		}
		if second := recv(t, sub); second.Type != "user_left" || second.Seq != 2 {
			t.Fatalf("second event = %+v", second)
		}
	}
}

// TestSubscribeFilter verifies the per-subscriber event-type filter (231).
func TestSubscribeFilter(t *testing.T) {
	bus := New(zap.NewNop())
	defer bus.Close()

	sub := bus.Subscribe("filtered", []string{"user_left"}, 0)
	bus.Publish("user_joined", []byte(`{}`))
	bus.Publish("user_left", []byte(`{}`))

	if evt := recv(t, sub); evt.Type != "user_left" {
		t.Fatalf("filtered subscriber got %q", evt.Type)
	}
	select {
	case evt := <-sub.C:
		t.Fatalf("filter leaked %+v", evt)
	default:
	}
}

func TestNonPositiveConfigurationIsClamped(t *testing.T) {
	bus := New(zap.NewNop())
	bus.Buffer = 0
	bus.MaxDrops = 0
	defer bus.Close()

	sub := bus.Subscribe("clamped", nil, 0)
	if sub == nil {
		t.Fatal("Subscribe returned nil")
	}
	bus.Publish("first", nil)  // fills the clamped one-event buffer
	bus.Publish("second", nil) // exercises the clamped drop threshold
	if bus.Stats().Dropped != 1 {
		t.Fatalf("drop stats = %+v", bus.Stats())
	}
}

// TestSlowSubscriberIsDroppedThenEvicted verifies the drop policy: a consumer
// that stops reading loses events instead of blocking the publisher, and is
// evicted once it is hopeless (231).
func TestSlowSubscriberIsDroppedThenEvicted(t *testing.T) {
	bus := New(zap.NewNop())
	bus.MaxDrops = 3
	defer bus.Close()

	slow := bus.Subscribe("slow", nil, 1)
	fast := bus.Subscribe("fast", nil, 64)

	// 1 fills the buffer, the next MaxDrops publishes are dropped and evict.
	for i := 0; i < 1+3; i++ {
		bus.Publish("user_joined", []byte(`{}`))
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-slow.C:
			if !ok {
				goto evicted
			}
		case <-deadline:
			t.Fatal("slow subscriber was never evicted")
		}
	}
evicted:
	if slow.Dropped() == 0 {
		t.Fatal("evicted subscriber reports no drops")
	}
	stats := bus.Stats()
	if stats.Evicted != 1 || stats.Dropped == 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.Subscribers != 1 {
		t.Fatalf("subscriber count after eviction = %d", stats.Subscribers)
	}
	// The healthy subscriber is untouched by its neighbour's eviction.
	if evt := recv(t, fast); evt.Type != "user_joined" {
		t.Fatalf("fast subscriber = %+v", evt)
	}
}

// TestUnsubscribeAndClose verifies both teardown paths close the channel.
func TestUnsubscribeAndClose(t *testing.T) {
	bus := New(zap.NewNop())
	sub := bus.Subscribe("a", nil, 0)
	sub.Unsubscribe()
	sub.Unsubscribe() // idempotent
	if _, ok := <-sub.C; ok {
		t.Fatal("channel still open after Unsubscribe")
	}
	// Publishing to nobody must not panic and must still advance the sequence.
	bus.Publish("user_joined", []byte(`{}`))
	if got := bus.Stats().Published; got != 1 {
		t.Fatalf("published = %d", got)
	}

	other := bus.Subscribe("b", nil, 0)
	bus.Close()
	bus.Close() // idempotent
	if _, ok := <-other.C; ok {
		t.Fatal("channel still open after Close")
	}
	if bus.Subscribe("late", nil, 0) != nil {
		t.Fatal("Subscribe succeeded on a closed bus")
	}
}

// TestPublishCopiesPayload verifies the bus does not alias a caller's buffer,
// which the broadcast path reuses.
func TestPublishCopiesPayload(t *testing.T) {
	bus := New(zap.NewNop())
	defer bus.Close()
	sub := bus.Subscribe("a", nil, 0)

	payload := []byte(`{"n":1}`)
	bus.Publish("x", payload)
	payload[5] = '9'

	evt := recv(t, sub)
	var doc struct {
		N int `json:"n"`
	}
	if err := json.Unmarshal(evt.Data, &doc); err != nil || doc.N != 1 {
		t.Fatalf("payload = %s (err %v)", evt.Data, err)
	}
}

func TestSubscribersCannotMutateEachOthersPayload(t *testing.T) {
	bus := New(zap.NewNop())
	defer bus.Close()
	first := bus.Subscribe("first", nil, 1)
	second := bus.Subscribe("second", nil, 1)
	bus.Publish("x", []byte(`{"safe":true}`))

	firstEvent := recv(t, first)
	firstEvent.Data[2] = 'X'
	secondEvent := recv(t, second)
	if got := string(secondEvent.Data); got != `{"safe":true}` {
		t.Fatalf("second subscriber payload was aliased: %q", got)
	}
}

func TestConcurrentPublishPreservesSequenceOrder(t *testing.T) {
	bus := New(zap.NewNop())
	defer bus.Close()
	const publishers = 256
	sub := bus.Subscribe("ordered", nil, publishers)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < publishers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			bus.Publish("concurrent", []byte(fmt.Sprintf(`{"id":%d}`, id)))
		}(i)
	}
	close(start)
	wg.Wait()

	for want := uint64(1); want <= publishers; want++ {
		if got := recv(t, sub).Seq; got != want {
			t.Fatalf("event sequence = %d, want %d", got, want)
		}
	}
}

func TestEvictDoesNotCountAlreadyUnsubscribedConsumer(t *testing.T) {
	bus := New(zap.NewNop())
	defer bus.Close()
	sub := bus.Subscribe("gone", nil, 1)
	sub.Unsubscribe()

	bus.evict(sub)
	if got := bus.Stats().Evicted; got != 0 {
		t.Fatalf("evicted count = %d, want 0", got)
	}
}

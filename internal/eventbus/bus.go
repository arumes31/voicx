// Package eventbus is the server-side publish/subscribe registry for control
// events (231/232). Until it existed, events were pushed inline per connection
// by the control server, so nothing outside a connected client could observe
// them; the bus gives bots (WebSocket, gRPC) a subscriber slot fed by the same
// fan-out.
//
// The bus is deliberately lossy. A subscriber that stops reading must never
// stall a publisher, because publishers run on the voice/control hot path:
// sends are non-blocking, a full subscriber buffer drops the event, and a
// subscriber that keeps dropping is evicted (see DropPolicy).
package eventbus

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// Event is one published server event. Data is the JSON payload the control
// server already marshals for its own broadcast envelope.
type Event struct {
	// Seq is the bus-wide publish sequence number, starting at 1. A
	// subscriber can detect its own gaps from it.
	Seq  uint64
	Type string
	Time time.Time
	Data json.RawMessage
}

// Defaults for New. DefaultBuffer is per subscriber, not bus-wide.
const (
	DefaultBuffer = 256
	// DefaultMaxDrops evicts a subscriber after this many CONSECUTIVE drops:
	// one burst is forgiven (the buffer drains and the counter resets), a
	// consumer that never reads again is dropped for good.
	DefaultMaxDrops = 64
)

// Stats is a snapshot of bus counters, exported for /metrics and logs.
type Stats struct {
	Subscribers int
	Published   uint64
	Delivered   uint64
	Dropped     uint64
	Evicted     uint64
}

// Bus is the subscriber registry and fan-out.
type Bus struct {
	logger *zap.Logger

	// Buffer is the per-subscriber queue depth used when Subscribe is
	// called with 0.
	Buffer int
	// MaxDrops is the consecutive-drop eviction threshold.
	MaxDrops int

	mu     sync.RWMutex
	closed bool
	nextID uint64
	subs   map[uint64]*Subscription

	seq       atomic.Uint64
	delivered atomic.Uint64
	dropped   atomic.Uint64
	evicted   atomic.Uint64
}

// New constructs an empty bus.
func New(logger *zap.Logger) *Bus {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Bus{
		logger:   logger,
		Buffer:   DefaultBuffer,
		MaxDrops: DefaultMaxDrops,
		subs:     make(map[uint64]*Subscription),
	}
}

// Subscription is one consumer's slot. Read from C until it is closed; the
// bus closes it on Unsubscribe, on eviction, or when the bus shuts down.
type Subscription struct {
	// C delivers events matching the subscription's type filter.
	C <-chan Event

	bus   *Bus
	id    uint64
	name  string
	ch    chan Event
	types map[string]bool

	// Publish runs under a read lock, so several publishers can touch these
	// concurrently: consecutive drops since the last delivery (the eviction
	// trigger), the lifetime drop count, and the evict-once latch.
	drops    atomic.Int64
	dropped  atomic.Uint64
	evicting atomic.Bool

	closeMu sync.Mutex
	closed  bool
}

// Subscribe registers a consumer. name identifies it in logs, types filters by
// event type (nil/empty = everything), buffer 0 uses Bus.Buffer. It returns
// nil after the bus is closed.
func (b *Bus) Subscribe(name string, types []string, buffer int) *Subscription {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	if buffer <= 0 {
		buffer = b.Buffer
	}
	var filter map[string]bool
	if len(types) > 0 {
		filter = make(map[string]bool, len(types))
		for _, t := range types {
			filter[t] = true
		}
	}
	ch := make(chan Event, buffer)
	b.nextID++
	sub := &Subscription{C: ch, bus: b, id: b.nextID, name: name, ch: ch, types: filter}
	b.subs[sub.id] = sub
	b.logger.Debug("eventbus subscriber added",
		zap.String("name", name), zap.Uint64("id", sub.id),
		zap.Int("buffer", buffer), zap.Int("types", len(filter)))
	return sub
}

// Unsubscribe removes the subscription and closes its channel. It is
// idempotent.
func (s *Subscription) Unsubscribe() {
	if s == nil {
		return
	}
	s.bus.mu.Lock()
	delete(s.bus.subs, s.id)
	s.bus.mu.Unlock()
	s.close()
}

// Dropped reports how many events this subscriber missed.
func (s *Subscription) Dropped() uint64 { return s.dropped.Load() }

// close closes the delivery channel exactly once.
func (s *Subscription) close() {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.ch)
}

// wants reports whether the subscription's filter accepts an event type.
func (s *Subscription) wants(eventType string) bool {
	return s.types == nil || s.types[eventType]
}

// Publish fans an event out to every matching subscriber. It never blocks:
// this is called from the control server's broadcast path.
func (b *Bus) Publish(eventType string, data []byte) {
	b.mu.RLock()
	if b.closed || len(b.subs) == 0 {
		b.mu.RUnlock()
		// Sequence numbers still advance so a subscriber that joins later
		// cannot mistake "nobody was listening" for "nothing happened".
		b.seq.Add(1)
		return
	}
	evt := Event{Seq: b.seq.Add(1), Type: eventType, Time: time.Now().UTC(), Data: append(json.RawMessage(nil), data...)}
	var evict []*Subscription
	for _, sub := range b.subs {
		if !sub.wants(eventType) {
			continue
		}
		select {
		case sub.ch <- evt:
			sub.drops.Store(0)
			b.delivered.Add(1)
		default:
			sub.dropped.Add(1)
			b.dropped.Add(1)
			if sub.drops.Add(1) >= int64(b.MaxDrops) && sub.evicting.CompareAndSwap(false, true) {
				evict = append(evict, sub)
			}
		}
	}
	b.mu.RUnlock()

	for _, sub := range evict {
		b.evict(sub)
	}
}

// evict removes a subscriber that stopped draining its buffer.
func (b *Bus) evict(sub *Subscription) {
	b.mu.Lock()
	delete(b.subs, sub.id)
	b.mu.Unlock()
	b.evicted.Add(1)
	b.logger.Warn("eventbus subscriber evicted: not draining its buffer",
		zap.String("name", sub.name), zap.Uint64("id", sub.id),
		zap.Uint64("dropped", sub.Dropped()))
	sub.close()
}

// Stats returns a counter snapshot.
func (b *Bus) Stats() Stats {
	b.mu.RLock()
	n := len(b.subs)
	b.mu.RUnlock()
	return Stats{
		Subscribers: n,
		Published:   b.seq.Load(),
		Delivered:   b.delivered.Load(),
		Dropped:     b.dropped.Load(),
		Evicted:     b.evicted.Load(),
	}
}

// Close drops every subscriber and refuses new ones. It is idempotent.
func (b *Bus) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	subs := make([]*Subscription, 0, len(b.subs))
	for id, sub := range b.subs {
		subs = append(subs, sub)
		delete(b.subs, id)
	}
	b.mu.Unlock()
	for _, sub := range subs {
		sub.close()
	}
}

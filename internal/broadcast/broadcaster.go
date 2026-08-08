// Package broadcast ... (see snapshot.go for package doc)
package broadcast

import (
	"bytes"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"

	"voicx/internal/state"
)

// ErrNotRegistered is returned when a broadcast targets a client that has no
// registered outbound channel.
var ErrNotRegistered = errors.New("client not registered for broadcasts")

// ErrChannelFull is returned when a non-blocking send to a registered client
// cannot complete because the client's outbound channel is full.
var ErrChannelFull = errors.New("client broadcast channel is full")

// ErrAlreadyRegistered is returned by Register when a clientID is already in
// the registry.
var ErrAlreadyRegistered = errors.New("client already registered for broadcasts")

// ErrClosed is returned when an operation needs an active broadcaster after
// Close has started.
var ErrClosed = errors.New("broadcaster is closed")

// clientBufferSize is the per-client outbound channel capacity.
const clientBufferSize = 16

// Broadcaster maintains a registry of connected clients that wish to receive
// broadcast messages (snapshots, channel-scoped payloads, events). Each client
// gets a buffered outbound channel; sends are non-blocking so a slow consumer
// never blocks the broadcaster.
type Broadcaster struct {
	logger *zap.Logger
	sm     *state.Manager

	// sendMu preserves message order when multiple server goroutines publish
	// concurrently. Registry lifetime remains protected by mu.
	sendMu sync.Mutex
	mu     sync.RWMutex
	closed bool
	// clients maps clientID -> bounded outbound queue.
	clients map[string]*clientQueue
	// tap observes every server-wide event (231/232). It is the single seam
	// between the per-connection fan-out and the event bus, so a bot sees
	// exactly what connected clients see.
	tap func(eventType string, payload []byte)

	delivered atomic.Uint64
	dropped   atomic.Uint64
}

type clientQueue struct {
	ch      chan []byte
	dropped atomic.Uint64
}

// Stats is an operational snapshot of fan-out delivery and pressure.
type Stats struct {
	Clients   int
	Delivered uint64
	Dropped   uint64
}

// New constructs a Broadcaster wired to the provided logger and state manager.
func New(logger *zap.Logger, sm *state.Manager) *Broadcaster {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Broadcaster{
		logger:  logger,
		sm:      sm,
		clients: make(map[string]*clientQueue),
	}
}

// Register creates a buffered outbound channel for the given client and returns
// a read-only receive channel the caller can select on. Returns an error if the
// client is already registered or the broadcaster is closed.
func (b *Broadcaster) Register(clientID string) (<-chan []byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrClosed
	}
	if _, ok := b.clients[clientID]; ok {
		return nil, ErrAlreadyRegistered
	}
	queue := &clientQueue{ch: make(chan []byte, clientBufferSize)}
	b.clients[clientID] = queue
	return queue.ch, nil
}

// Unregister closes and removes the outbound channel for the given client. It
// is a no-op if the client is not registered.
func (b *Broadcaster) Unregister(clientID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	queue, ok := b.clients[clientID]
	if !ok {
		return
	}
	delete(b.clients, clientID)
	close(queue.ch)
}

// BroadcastSnapshot builds a TreeSnapshot from the state manager, JSON-marshals
// it, and sends it to all registered clients. Sends are non-blocking: if a
// client's channel is full, the message is dropped for that client and a
// warning is logged.
//
// The snapshot is built for one viewer (forAdmin/viewerUniqueID gate invisible
// users, 381), so every registered client sees that viewer's view: pass the
// least-privileged values (false, "") for a true fan-out and use
// BroadcastToClient with a per-client BuildSnapshot when visibility differs.
func (b *Broadcaster) BroadcastSnapshot(forAdmin bool, viewerUniqueID string) {
	snap := BuildSnapshot(b.sm, forAdmin, viewerUniqueID)
	payload, err := json.Marshal(snap)
	if err != nil {
		b.logger.Error("broadcast: failed to marshal snapshot", zap.Error(err))
		return
	}
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	b.broadcastToAllLocked(payload)
}

// BroadcastToChannel sends a raw payload to all clients currently in the given
// channel (looked up via state.Manager.ChannelMembers). Sends are non-blocking.
func (b *Broadcaster) BroadcastToChannel(channelID int64, payload []byte) {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	if b.sm == nil {
		return
	}
	members := b.sm.ChannelMembers(channelID)
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}
	for _, c := range members {
		queue, ok := b.clients[c.ClientID]
		if !ok {
			continue
		}
		b.trySend(queue, c.ClientID, payload)
	}
}

// BroadcastToClient sends a payload to a single registered client. Returns an
// error if the client is not registered or its channel is full.
func (b *Broadcaster) BroadcastToClient(clientID string, payload []byte) error {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return ErrClosed
	}
	queue, ok := b.clients[clientID]
	if !ok {
		return ErrNotRegistered
	}
	if b.trySend(queue, clientID, payload) {
		return nil
	}
	return ErrChannelFull
}

// SetEventTap installs an observer invoked for every BroadcastEvent, before
// the client fan-out. The tap must not block (231).
func (b *Broadcaster) SetEventTap(tap func(eventType string, payload []byte)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tap = tap
}

// BroadcastEvent wraps the payload in a small envelope {"type": eventType,
// "data": <payload>} and broadcasts it to all registered clients.
func (b *Broadcaster) BroadcastEvent(eventType string, payload []byte) {
	envelope := struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}{
		Type: eventType,
		Data: payload,
	}
	wrapped, err := json.Marshal(envelope)
	if err != nil {
		b.logger.Error("broadcast: failed to marshal event envelope",
			zap.String("event_type", eventType), zap.Error(err))
		return
	}

	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	b.mu.RLock()
	tap := b.tap
	b.mu.RUnlock()
	if tap != nil {
		tap(eventType, payload)
	}
	b.broadcastToAllLocked(wrapped)
}

// ClientCount returns the number of currently registered clients.
func (b *Broadcaster) ClientCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.clients)
}

// Stats returns current client count plus lifetime delivery/drop counters.
func (b *Broadcaster) Stats() Stats {
	b.mu.RLock()
	clients := len(b.clients)
	b.mu.RUnlock()
	return Stats{Clients: clients, Delivered: b.delivered.Load(), Dropped: b.dropped.Load()}
}

// Close closes all client channels and clears the registry. It is idempotent.
func (b *Broadcaster) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, queue := range b.clients {
		close(queue.ch)
		delete(b.clients, id)
	}
}

// broadcastToAllLocked sends a payload to every registered client. The caller
// holds sendMu so each queue observes the same publish order.
func (b *Broadcaster) broadcastToAllLocked(payload []byte) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}
	for id, queue := range b.clients {
		b.trySend(queue, id, payload)
	}
}

// trySend performs a non-blocking, ownership-safe send. The caller holds
// sendMu and at least a read lock on b.mu, so no other producer can fill the
// queue after the capacity check and the channel cannot close.
func (b *Broadcaster) trySend(queue *clientQueue, clientID string, payload []byte) bool {
	if len(queue.ch) == cap(queue.ch) {
		b.noteDrop(queue, clientID)
		return false
	}
	queue.ch <- bytes.Clone(payload)
	b.delivered.Add(1)
	return true
}

func (b *Broadcaster) noteDrop(queue *clientQueue, clientID string) {
	b.dropped.Add(1)
	clientDrops := queue.dropped.Add(1)
	// Log the first drop and powers of two thereafter. This preserves a clear
	// pressure signal without allowing a stalled client to create a log storm.
	if clientDrops == 1 || clientDrops&(clientDrops-1) == 0 {
		b.logger.Warn("broadcast: client channel full, dropping message",
			zap.String("client_id", clientID), zap.Uint64("client_drops", clientDrops))
	}
}

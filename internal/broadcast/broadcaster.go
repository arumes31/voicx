// Package broadcast ... (see snapshot.go for package doc)
package broadcast

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

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

// clientBufferSize is the per-client outbound channel capacity.
const clientBufferSize = 16

// Broadcaster maintains a registry of connected clients that wish to receive
// broadcast messages (snapshots, channel-scoped payloads, events). Each client
// gets a buffered outbound channel; sends are non-blocking so a slow consumer
// never blocks the broadcaster.
type Broadcaster struct {
	logger *zap.Logger
	sm     *state.Manager

	mu     sync.RWMutex
	closed bool
	// clients maps clientID -> outbound channel.
	clients map[string]chan []byte
}

// New constructs a Broadcaster wired to the provided logger and state manager.
func New(logger *zap.Logger, sm *state.Manager) *Broadcaster {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Broadcaster{
		logger:  logger,
		sm:      sm,
		clients: make(map[string]chan []byte),
	}
}

// Register creates a buffered outbound channel for the given client and returns
// a read-only receive channel the caller can select on. Returns an error if the
// client is already registered or the broadcaster is closed.
func (b *Broadcaster) Register(clientID string) (<-chan []byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, errors.New("broadcaster is closed")
	}
	if _, ok := b.clients[clientID]; ok {
		return nil, ErrAlreadyRegistered
	}
	ch := make(chan []byte, clientBufferSize)
	b.clients[clientID] = ch
	return ch, nil
}

// Unregister closes and removes the outbound channel for the given client. It
// is a no-op if the client is not registered.
func (b *Broadcaster) Unregister(clientID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch, ok := b.clients[clientID]
	if !ok {
		return
	}
	delete(b.clients, clientID)
	safeClose(ch)
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
	b.broadcastToAll(payload)
}

// BroadcastToChannel sends a raw payload to all clients currently in the given
// channel (looked up via state.Manager.ChannelMembers). Sends are non-blocking.
func (b *Broadcaster) BroadcastToChannel(channelID int64, payload []byte) {
	members := b.sm.ChannelMembers(channelID)
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}
	for _, c := range members {
		ch, ok := b.clients[c.ClientID]
		if !ok {
			continue
		}
		b.trySend(ch, c.ClientID, payload)
	}
}

// BroadcastToClient sends a payload to a single registered client. Returns an
// error if the client is not registered or its channel is full.
func (b *Broadcaster) BroadcastToClient(clientID string, payload []byte) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return errors.New("broadcaster is closed")
	}
	ch, ok := b.clients[clientID]
	if !ok {
		return ErrNotRegistered
	}
	select {
	case ch <- payload:
		return nil
	default:
		b.logger.Warn("broadcast: client channel full, dropping message",
			zap.String("client_id", clientID))
		return ErrChannelFull
	}
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
	b.broadcastToAll(wrapped)
}

// ClientCount returns the number of currently registered clients.
func (b *Broadcaster) ClientCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.clients)
}

// Close closes all client channels and clears the registry. It is idempotent.
func (b *Broadcaster) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, ch := range b.clients {
		safeClose(ch)
		delete(b.clients, id)
	}
}

// broadcastToAll sends a payload to every registered client. Sends are
// non-blocking; full channels drop the message with a warning log.
func (b *Broadcaster) broadcastToAll(payload []byte) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}
	for id, ch := range b.clients {
		b.trySend(ch, id, payload)
	}
}

// trySend performs a non-blocking send to ch. On a full channel it logs a
// warning and drops the message. It must be called while holding at least an
// RLock on b.mu so the channel is guaranteed not to be closed concurrently.
func (b *Broadcaster) trySend(ch chan []byte, clientID string, payload []byte) {
	select {
	case ch <- payload:
	default:
		b.logger.Warn("broadcast: client channel full, dropping message",
			zap.String("client_id", clientID))
	}
}

// safeClose closes a channel guarded by a recover, so a double-close (which
// should not happen given the mutex discipline) cannot panic.
func safeClose(ch chan []byte) {
	defer func() {
		if r := recover(); r != nil {
			// Already closed; ignore.
			_ = fmt.Sprintf("%v", r)
		}
	}()
	close(ch)
}

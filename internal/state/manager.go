package state

import (
	"errors"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ErrClientNotFound is returned when an operation references an unknown client.
var ErrClientNotFound = errors.New("client not found")

// ErrChannelNotFound is returned when an operation references an unknown channel.
var ErrChannelNotFound = errors.New("channel not found")

// ErrNotInChannel is returned when a client attempts to leave a channel it is
// not currently a member of.
var ErrNotInChannel = errors.New("client is not in a channel")

// Manager tracks connected clients, active channels, channel membership, and
// current speaking states in memory. All methods are goroutine-safe.
type Manager struct {
	logger *zap.Logger

	mu sync.RWMutex

	// clients maps clientID -> *Client.
	clients map[string]*Client
	// channels maps channelID -> *Channel.
	channels map[int64]*Channel
	// membership maps channelID -> set of clientIDs (map[clientID]bool).
	membership map[int64]map[string]bool
	// speaking maps clientID -> *SpeakingState for clients currently speaking.
	speaking map[string]*SpeakingState
	// subs maps clientID -> set of EXPLICITLY subscribed channelIDs (312).
	// The channel a client stands in is implicit and never stored here, so
	// leaving a channel cannot silently promote it to a subscription.
	subs map[string]map[int64]bool
	// subsByChannel is the reverse index the chat fan-out reads on every
	// relayed message; walking every client's set instead would make the cost
	// of one message scale with the whole server.
	subsByChannel map[int64]map[string]bool
}

// New constructs a Manager wired to the provided logger.
func New(logger *zap.Logger) *Manager {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Manager{
		logger:        logger,
		clients:       make(map[string]*Client),
		channels:      make(map[int64]*Channel),
		membership:    make(map[int64]map[string]bool),
		speaking:      make(map[string]*SpeakingState),
		subs:          make(map[string]map[int64]bool),
		subsByChannel: make(map[int64]map[string]bool),
	}
}

// ---------------------------------------------------------------------------
// Client methods
// ---------------------------------------------------------------------------

// AddClient registers a client. If a client with the same ClientID already
// exists it is replaced.
func (m *Manager) AddClient(c *Client) {
	if c == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clients[c.ClientID] = c
	m.logger.Debug("state: client added", zap.String("client_id", c.ClientID))
}

// RemoveClient removes a client from the manager, leaving any channel it was
// a member of and clearing its speaking state.
func (m *Manager) RemoveClient(clientID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.clients[clientID]
	if !ok {
		return
	}

	// Leave channel membership if any.
	if c.ChannelID != 0 {
		if members, ok := m.membership[c.ChannelID]; ok {
			delete(members, clientID)
			if len(members) == 0 {
				delete(m.membership, c.ChannelID)
			}
			if ch, ok := m.channels[c.ChannelID]; ok && ch.ClientCount > 0 {
				ch.ClientCount--
			}
		}
	}

	delete(m.speaking, clientID)
	// (312) a disconnect drops every subscription: the registry is per
	// connection, and a stale entry would keep fanning chat at a dead client.
	m.dropSubsLocked(clientID)
	delete(m.clients, clientID)
	m.logger.Debug("state: client removed", zap.String("client_id", clientID))
}

// GetClient returns a snapshot of the client with the given id and whether it
// was found. Returning the manager's live pointer would let a caller race
// MoveClient, SetStatus, or SetSpeaking after the read lock is released.
func (m *Manager) GetClient(clientID string) (*Client, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.clients[clientID]
	if !ok {
		return nil, false
	}
	return cloneClient(c), true
}

// GetClientByUniqueID returns the online client with the given unique ID and
// whether it was found.
func (m *Manager) GetClientByUniqueID(uniqueID string) (*Client, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, c := range m.clients {
		if c.UniqueID == uniqueID {
			return cloneClient(c), true
		}
	}
	return nil, false
}

// ClientCount returns the number of registered clients.
func (m *Manager) ClientCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.clients)
}

// ListClients returns a snapshot slice of all registered clients. The returned
// slice is safe to use without holding the lock.
func (m *Manager) ListClients() []*Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Client, 0, len(m.clients))
	for _, c := range m.clients {
		out = append(out, cloneClient(c))
	}
	return out
}

func cloneClient(c *Client) *Client {
	if c == nil {
		return nil
	}
	out := *c
	if c.Metadata != nil {
		out.Metadata = make(map[string]string, len(c.Metadata))
		for key, value := range c.Metadata {
			out.Metadata[key] = value
		}
	}
	return &out
}

// ---------------------------------------------------------------------------
// Channel methods
// ---------------------------------------------------------------------------

// AddChannel registers a channel. If a channel with the same ChannelID already
// exists it is replaced.
func (m *Manager) AddChannel(ch *Channel) {
	if ch == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.channels[ch.ChannelID] = ch
	m.logger.Debug("state: channel added", zap.Int64("channel_id", ch.ChannelID), zap.String("name", ch.Name))
}

// RemoveChannel removes a channel. All clients still in the channel have their
// ChannelID reset to 0, and the channel's membership set is dropped.
func (m *Manager) RemoveChannel(channelID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	members, ok := m.membership[channelID]
	if ok {
		for clientID := range members {
			if c, ok := m.clients[clientID]; ok {
				c.ChannelID = 0
			}
		}
		delete(m.membership, channelID)
	}

	// Clear speaking states for clients that were speaking in this channel.
	for cid, ss := range m.speaking {
		if ss.ChannelID == channelID {
			if c, ok := m.clients[cid]; ok {
				c.IsSpeaking = false
			}
			delete(m.speaking, cid)
		}
	}

	// (312) a deleted channel takes its subscriptions with it. This is also
	// what covers the temporary-channel cleanup watcher, which deletes
	// channels without going through a handler.
	for clientID := range m.subsByChannel[channelID] {
		if set, ok := m.subs[clientID]; ok {
			delete(set, channelID)
			if len(set) == 0 {
				delete(m.subs, clientID)
			}
		}
	}
	delete(m.subsByChannel, channelID)

	delete(m.channels, channelID)
	m.logger.Debug("state: channel removed", zap.Int64("channel_id", channelID))
}

// GetChannel returns the channel with the given id and whether it was found.
func (m *Manager) GetChannel(channelID int64) (*Channel, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ch, ok := m.channels[channelID]
	return ch, ok
}

// ChannelCount returns the number of registered channels.
func (m *Manager) ChannelCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.channels)
}

// ListChannels returns a snapshot slice of all registered channels.
func (m *Manager) ListChannels() []*Channel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Channel, 0, len(m.channels))
	for _, ch := range m.channels {
		out = append(out, ch)
	}
	return out
}

// ChannelTree returns channels sorted by ParentID then OrderIndex, suitable
// for building a channel tree.
func (m *Manager) ChannelTree() []*Channel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Channel, 0, len(m.channels))
	for _, ch := range m.channels {
		out = append(out, ch)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ParentID != out[j].ParentID {
			return out[i].ParentID < out[j].ParentID
		}
		return out[i].OrderIndex < out[j].OrderIndex
	})
	return out
}

// ---------------------------------------------------------------------------
// Membership methods
// ---------------------------------------------------------------------------

// JoinChannel validates that the client and channel exist, updates the
// client's ChannelID, adds the client to the channel's membership set, and
// increments the channel's ClientCount.
func (m *Manager) JoinChannel(clientID string, channelID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.clients[clientID]
	if !ok {
		return ErrClientNotFound
	}
	ch, ok := m.channels[channelID]
	if !ok {
		return ErrChannelNotFound
	}

	// If already in this channel, nothing to do.
	if c.ChannelID == channelID {
		return nil
	}

	// Leave previous channel membership if any.
	if c.ChannelID != 0 {
		if members, ok := m.membership[c.ChannelID]; ok {
			delete(members, clientID)
			if len(members) == 0 {
				delete(m.membership, c.ChannelID)
			}
			if prev, ok := m.channels[c.ChannelID]; ok && prev.ClientCount > 0 {
				prev.ClientCount--
			}
		}
	}

	c.ChannelID = channelID
	members, ok := m.membership[channelID]
	if !ok {
		members = make(map[string]bool)
		m.membership[channelID] = members
	}
	members[clientID] = true
	ch.ClientCount++

	m.logger.Debug("state: client joined channel",
		zap.String("client_id", clientID),
		zap.Int64("channel_id", channelID))
	return nil
}

// LeaveChannel removes the client from its current channel's membership set,
// decrements the channel's ClientCount, and sets the client's ChannelID to 0.
func (m *Manager) LeaveChannel(clientID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.clients[clientID]
	if !ok {
		return ErrClientNotFound
	}
	if c.ChannelID == 0 {
		return ErrNotInChannel
	}

	channelID := c.ChannelID
	if members, ok := m.membership[channelID]; ok {
		delete(members, clientID)
		if len(members) == 0 {
			delete(m.membership, channelID)
		}
		if ch, ok := m.channels[channelID]; ok && ch.ClientCount > 0 {
			ch.ClientCount--
		}
	}

	// Clear speaking state for the client when leaving a channel.
	if _, ok := m.speaking[clientID]; ok {
		delete(m.speaking, clientID)
		c.IsSpeaking = false
	}

	c.ChannelID = 0
	m.logger.Debug("state: client left channel",
		zap.String("client_id", clientID),
		zap.Int64("channel_id", channelID))
	return nil
}

// ChannelMembers returns a snapshot slice of all clients currently members of
// the given channel. Returns nil if the channel does not exist or has no
// members.
func (m *Manager) ChannelMembers(channelID int64) []*Client {
	m.mu.RLock()
	defer m.mu.RUnlock()

	members, ok := m.membership[channelID]
	if !ok || len(members) == 0 {
		return nil
	}
	out := make([]*Client, 0, len(members))
	for clientID := range members {
		if c, ok := m.clients[clientID]; ok {
			out = append(out, c)
		}
	}
	return out
}

// MoveClient atomically moves a client from its current channel to the target
// channel. It is equivalent to LeaveChannel followed by JoinChannel but
// performed under a single lock acquisition.
func (m *Manager) MoveClient(clientID string, targetChannelID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.clients[clientID]
	if !ok {
		return ErrClientNotFound
	}
	ch, ok := m.channels[targetChannelID]
	if !ok {
		return ErrChannelNotFound
	}

	if c.ChannelID == targetChannelID {
		return nil
	}

	// Leave previous channel.
	if c.ChannelID != 0 {
		if members, ok := m.membership[c.ChannelID]; ok {
			delete(members, clientID)
			if len(members) == 0 {
				delete(m.membership, c.ChannelID)
			}
			if prev, ok := m.channels[c.ChannelID]; ok && prev.ClientCount > 0 {
				prev.ClientCount--
			}
		}
		// Clear speaking state on move.
		if _, ok := m.speaking[clientID]; ok {
			delete(m.speaking, clientID)
			c.IsSpeaking = false
		}
	}

	// Join target channel.
	c.ChannelID = targetChannelID
	members, ok := m.membership[targetChannelID]
	if !ok {
		members = make(map[string]bool)
		m.membership[targetChannelID] = members
	}
	members[clientID] = true
	ch.ClientCount++

	m.logger.Debug("state: client moved",
		zap.String("client_id", clientID),
		zap.Int64("target_channel_id", targetChannelID))
	return nil
}

// ---------------------------------------------------------------------------
// Channel subscriptions (312)
// ---------------------------------------------------------------------------

// Subscribe records explicit channel subscriptions for a client. Unknown
// clients and unknown channels are ignored: the caller has already resolved
// both, and inventing a subscription to a channel that does not exist would
// hand the fan-out a target it can never validate.
func (m *Manager) Subscribe(clientID string, channelIDs []int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.clients[clientID]; !ok {
		return
	}
	for _, channelID := range channelIDs {
		if _, ok := m.channels[channelID]; !ok {
			continue
		}
		set, ok := m.subs[clientID]
		if !ok {
			set = make(map[int64]bool)
			m.subs[clientID] = set
		}
		set[channelID] = true
		rev, ok := m.subsByChannel[channelID]
		if !ok {
			rev = make(map[string]bool)
			m.subsByChannel[channelID] = rev
		}
		rev[clientID] = true
	}
}

// Unsubscribe removes explicit subscriptions. It is a no-op for entries that
// are not present.
func (m *Manager) Unsubscribe(clientID string, channelIDs []int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	set, ok := m.subs[clientID]
	if !ok {
		return
	}
	for _, channelID := range channelIDs {
		delete(set, channelID)
		if rev, ok := m.subsByChannel[channelID]; ok {
			delete(rev, clientID)
			if len(rev) == 0 {
				delete(m.subsByChannel, channelID)
			}
		}
	}
	if len(set) == 0 {
		delete(m.subs, clientID)
	}
}

// Subscriptions returns the client's explicit subscriptions, ascending. The
// channel the client stands in is NOT included: it is implicit and resolved
// by the caller, which is what makes it unremovable.
func (m *Manager) Subscriptions(clientID string) []int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	set := m.subs[clientID]
	out := make([]int64, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// IsSubscribed reports whether the client explicitly subscribed to channelID.
func (m *Manager) IsSubscribed(clientID string, channelID int64) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.subs[clientID][channelID]
}

// ClientChannelState returns the mutable fields subscription and chat-scope
// checks need while holding the manager read lock. GetClient returns a clone,
// so callers that need a coherent ChannelID and E2EPublicKey pair use this
// method instead of taking separate snapshots.
func (m *Manager) ClientChannelState(clientID string) (channelID int64, e2ePublicKey string, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.clients[clientID]
	if !ok {
		return 0, "", false
	}
	return c.ChannelID, c.E2EPublicKey, true
}

// ChannelSubscribers returns a snapshot of the clients that explicitly
// subscribed to channelID.
func (m *Manager) ChannelSubscribers(channelID int64) []*Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rev := m.subsByChannel[channelID]
	out := make([]*Client, 0, len(rev))
	for clientID := range rev {
		if c, ok := m.clients[clientID]; ok {
			out = append(out, cloneClient(c))
		}
	}
	return out
}

// dropSubsLocked removes every subscription held by a client. The caller must
// hold m.mu.
func (m *Manager) dropSubsLocked(clientID string) {
	for channelID := range m.subs[clientID] {
		if rev, ok := m.subsByChannel[channelID]; ok {
			delete(rev, clientID)
			if len(rev) == 0 {
				delete(m.subsByChannel, channelID)
			}
		}
	}
	delete(m.subs, clientID)
}

// ---------------------------------------------------------------------------
// Speaking methods
// ---------------------------------------------------------------------------

// SetSpeaking updates the client's IsSpeaking flag and the speaking state map.
// When speaking is true a SpeakingState with StartedAt=now is recorded; when
// false the speaking state is removed.
func (m *Manager) SetSpeaking(clientID string, speaking bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.clients[clientID]
	if !ok {
		return
	}

	if speaking {
		c.IsSpeaking = true
		m.speaking[clientID] = &SpeakingState{
			ClientID:  clientID,
			ChannelID: c.ChannelID,
			StartedAt: time.Now(),
		}
		return
	}

	c.IsSpeaking = false
	delete(m.speaking, clientID)
}

// SetPrioritySpeaker updates the client's PrioritySpeaker flag. It is a no-op
// for unknown clients.
func (m *Manager) SetPrioritySpeaker(clientID string, active bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.clients[clientID]; ok {
		c.PrioritySpeaker = active
	}
}

// SetE2EPublicKey records the client's X25519 public key. It is a no-op for
// unknown clients.
func (m *Manager) SetE2EPublicKey(clientID, publicKey string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.clients[clientID]; ok {
		c.E2EPublicKey = publicKey
	}
}

// SetStatus updates the client's presence status and message (wave 8b). It
// is a no-op for unknown clients.
func (m *Manager) SetStatus(clientID, status, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.clients[clientID]; ok {
		c.Status = status
		c.StatusMessage = message
	}
}

// SpeakingClients returns a snapshot slice of all current SpeakingStates.
func (m *Manager) SpeakingClients() []*SpeakingState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*SpeakingState, 0, len(m.speaking))
	for _, ss := range m.speaking {
		out = append(out, ss)
	}
	return out
}

// IsSpeaking reports whether the given client is currently speaking.
func (m *Manager) IsSpeaking(clientID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.speaking[clientID]
	return ok
}

// ---------------------------------------------------------------------------
// Stats
// ---------------------------------------------------------------------------

// Stats is a monitoring snapshot of the Manager's current state.
type Stats struct {
	ClientCount         int
	ChannelCount        int
	SpeakingCount       int
	ChannelClientCounts map[int64]int
}

// Stats returns a snapshot of current counts for monitoring.
func (m *Manager) Stats() Stats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s := Stats{
		ClientCount:         len(m.clients),
		ChannelCount:        len(m.channels),
		SpeakingCount:       len(m.speaking),
		ChannelClientCounts: make(map[int64]int, len(m.channels)),
	}
	for id, ch := range m.channels {
		s.ChannelClientCounts[id] = ch.ClientCount
	}
	return s
}

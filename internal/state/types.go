// Package state provides an in-memory, goroutine-safe manager for connected
// clients, active channels, channel membership, and current speaking states.
//
// The Manager uses a single sync.RWMutex to guard all of its maps. Read
// operations take RLock; write operations take Lock. Critical sections are kept
// short and never call out to external code while holding the lock, which
// avoids deadlocks and minimizes contention.
package state

import (
	"net"
	"sort"
	"time"
)

// Client represents a connected user tracked in memory.
//
// Conn is nullable: headless clients (e.g. bots or server-side entities) may
// have a nil Conn. Metadata is an arbitrary string map for per-client extras.
type Client struct {
	ClientID   string
	UniqueID   string
	Nickname   string
	ChannelID  int64 // 0 means no channel
	IsSpeaking bool
	// PrioritySpeaker marks TS3-style priority speakers (channel commanders):
	// clients duck other publishers while a priority speaker talks.
	PrioritySpeaker bool
	// Status/StatusMessage carry the client's presence (wave 8b, 307-309):
	// "online" ("" counts as online), "away", or "busy", plus a free-form
	// status message.
	Status        string
	StatusMessage string
	// E2EPublicKey is the client's X25519 public key (base64) for E2EE
	// direct messages and sealed chat-key distribution (wave 4b). Registered
	// users also persist it in the database; guests live here only.
	E2EPublicKey string `json:"-"`
	// IsBot marks accounts holding b_client_is_bot (180); the client renders a
	// bot badge instead of a normal presence entry.
	IsBot       bool
	ConnectedAt time.Time
	Conn        net.Conn // nullable for headless clients
	Metadata    map[string]string
}

// Channel represents an active channel tracked in memory.
//
// ClientCount is a derived field maintained by the Manager as clients join
// and leave channels; it is not authoritative against the database.
//
// PasswordHash holds the Argon2id hash of the channel password (empty when
// the channel has no password). It is excluded from JSON serialization so it
// never leaks into snapshots sent to clients. NeededJoinPower is the
// i_channel_join_power a client must meet or exceed to join the channel.
// HasIcon reports whether a channel icon has been uploaded (icons live on
// disk under the file root).
type Channel struct {
	ChannelID       int64
	ParentID        int64
	Name            string
	Topic           string
	OrderIndex      int
	ChannelType     int // 0=temp, 1=semi-perm, 2=perm
	MaxClients      int
	ClientCount     int // derived, maintained by Manager
	CreatedAt       time.Time
	PasswordHash    string `json:"-"`
	NeededJoinPower int
	HasIcon         bool
	// HasPassword reports (without exposing the hash) that a join password is
	// set (304 lock icon); it is populated by the snapshot builder. No field
	// here is renamed for JSON (the one tag present is an exclusion), so
	// snapshots marshal Go field names and the client reads "HasPassword".
	HasPassword bool
	// Per-channel Opus audio quality (migration 005). OpusBitrate is the
	// target bitrate in bits/s; 0 means the server default (32000). A channel
	// with OpusStereo and OpusBitrate >= 96000 counts as a music channel: the
	// talk-power gate is bypassed for its publishers.
	OpusBitrate int
	OpusFEC     bool
	OpusDTX     bool
	OpusStereo  bool
	// SlowModeSeconds is the minimum delay between one user's chat messages
	// in this channel (114; 0 = off). Privileged users (b_chat_slowmode_bypass
	// or admins) skip it.
	SlowModeSeconds int
	// Description is the channel's long description (112/113), rendered
	// client-side with markdown.
	Description string
	// InheritPermissions makes a sub-channel resolve its parent's channel
	// permissions before its own (157). It also chains the needed join power,
	// so a gated parent cannot be bypassed by joining a child directly.
	InheritPermissions bool
}

// SpeakingState records that a client is currently speaking in a channel.
type SpeakingState struct {
	ClientID  string
	ChannelID int64
	StartedAt time.Time
}

// maxChannelDepth caps every parent walk. A cycle cannot be created through
// the channel manager, but a hand-edited database must not hang the server.
const maxChannelDepth = 64

// ChannelTreeOrdered returns all channels in a total order: parent, then
// order index, then channel id. The id tiebreak is what makes the order total
// (163): two siblings sharing an order index would otherwise compare equal and
// the tree would reshuffle between snapshots.
func (m *Manager) ChannelTreeOrdered() []*Channel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Channel, 0, len(m.channels))
	for _, ch := range m.channels {
		clone := *ch
		out = append(out, &clone)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.ParentID != b.ParentID {
			return a.ParentID < b.ParentID
		}
		if a.OrderIndex != b.OrderIndex {
			return a.OrderIndex < b.OrderIndex
		}
		return a.ChannelID < b.ChannelID
	})
	return out
}

// ChannelAncestors returns the parent chain of channelID, nearest parent
// first, ignoring the inheritance flag. It is the cycle guard for channel
// moves (168): a channel may not be re-parented under one of its own
// descendants.
func (m *Manager) ChannelAncestors(channelID int64) []int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ancestorsLocked(channelID)
}

// ChannelPermissionChain returns channelID followed by the ancestors whose
// channel permissions it inherits (157): the walk stops at the first channel
// that does not have InheritPermissions set. The permission resolver reads
// this to know which channels to merge, nearest (most specific) first.
func (m *Manager) ChannelPermissionChain(channelID int64) []int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.channels[channelID]; !ok {
		return nil
	}
	chain := []int64{channelID}
	id := channelID
	for i := 0; i < maxChannelDepth; i++ {
		ch, ok := m.channels[id]
		if !ok || !ch.InheritPermissions || ch.ParentID == 0 {
			break
		}
		if _, ok := m.channels[ch.ParentID]; !ok {
			break
		}
		chain = append(chain, ch.ParentID)
		id = ch.ParentID
	}
	return chain
}

// EffectiveJoinPower returns the needed join power a client must meet to enter
// channelID: the highest needed power along the inheritance chain (157/168),
// so re-parenting a channel under a gated parent cannot hand out a back door
// into the subtree.
func (m *Manager) EffectiveJoinPower(channelID int64) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ch, ok := m.channels[channelID]
	if !ok {
		return 0
	}
	needed := ch.NeededJoinPower
	id := channelID
	for i := 0; i < maxChannelDepth; i++ {
		cur, ok := m.channels[id]
		if !ok || !cur.InheritPermissions || cur.ParentID == 0 {
			break
		}
		parent, ok := m.channels[cur.ParentID]
		if !ok {
			break
		}
		if parent.NeededJoinPower > needed {
			needed = parent.NeededJoinPower
		}
		id = cur.ParentID
	}
	return needed
}

// ancestorsLocked walks the parent chain with the read lock already held.
func (m *Manager) ancestorsLocked(channelID int64) []int64 {
	var out []int64
	id := channelID
	for i := 0; i < maxChannelDepth; i++ {
		ch, ok := m.channels[id]
		if !ok || ch.ParentID == 0 {
			break
		}
		out = append(out, ch.ParentID)
		id = ch.ParentID
	}
	return out
}

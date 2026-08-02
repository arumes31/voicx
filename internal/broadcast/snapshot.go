// Package broadcast constructs snapshots of the server's channel tree and active
// users and delivers them to connected clients via per-client outbound
// channels.
//
// The Broadcaster maintains a registry of clients that wish to receive
// broadcast messages. Each registered client gets a buffered channel; the
// broadcaster performs non-blocking sends so a slow consumer never blocks the
// producer (messages are dropped for that client with a warning log).
package broadcast

import (
	"time"

	"voicx/internal/state"
)

// ClientInfo is a JSON-serializable subset of state.Client. It deliberately
// omits the net.Conn field so snapshots can be safely marshaled and sent over
// the wire.
type ClientInfo struct {
	ClientID    string    `json:"client_id"`
	UniqueID    string    `json:"unique_id"`
	Nickname    string    `json:"nickname"`
	ChannelID   int64     `json:"channel_id"`
	IsSpeaking  bool      `json:"is_speaking"`
	ConnectedAt time.Time `json:"connected_at"`
	// PrioritySpeaker marks TS3-style priority speakers (14); clients duck
	// other publishers while a priority speaker in their channel talks.
	PrioritySpeaker bool `json:"priority_speaker,omitempty"`
	// Status/StatusMessage carry the client's presence (307-309).
	Status        string `json:"status,omitempty"`
	StatusMessage string `json:"status_message,omitempty"`
	// IsBot marks accounts holding b_client_is_bot (180).
	IsBot bool `json:"is_bot,omitempty"`
}

// ChannelNode represents a single channel in the tree snapshot. It embeds the
// channel's fields and carries its child channels (recursively) plus the
// ClientInfo of the users currently in this channel.
type ChannelNode struct {
	state.Channel
	Children []*ChannelNode `json:"children"`
	Clients  []*ClientInfo  `json:"clients"`
}

// TreeSnapshot is the full nested view of the server's channel tree and the
// users currently connected.
type TreeSnapshot struct {
	RootChannels  []*ChannelNode `json:"root_channels"`
	TotalClients  int            `json:"total_clients"`
	TotalChannels int            `json:"total_channels"`
	GeneratedAt   time.Time      `json:"generated_at"`
}

// clientToInfo converts a state.Client into a serializable ClientInfo, stripping
// the non-serializable net.Conn field.
func clientToInfo(c *state.Client) ClientInfo {
	if c == nil {
		return ClientInfo{}
	}
	return ClientInfo{
		ClientID:        c.ClientID,
		UniqueID:        c.UniqueID,
		Nickname:        c.Nickname,
		ChannelID:       c.ChannelID,
		IsSpeaking:      c.IsSpeaking,
		ConnectedAt:     c.ConnectedAt,
		PrioritySpeaker: c.PrioritySpeaker,
		Status:          c.Status,
		StatusMessage:   c.StatusMessage,
		IsBot:           c.IsBot,
	}
}

// BuildSnapshot walks the state.Manager's channel tree and channel membership
// to construct a full nested TreeSnapshot. Each ChannelNode is populated with
// its child channels (recursively) and the ClientInfo of clients currently in
// that channel. Totals are computed across the whole tree.
//
// BuildSnapshot never returns nil; an empty state yields a non-nil snapshot with
// zero totals.
//
// Invisible users (381) are hidden unless the viewer is an admin or the
// invisible user themself (they still see everyone).
func BuildSnapshot(sm *state.Manager, forAdmin bool, viewerUniqueID string) *TreeSnapshot {
	snap := &TreeSnapshot{
		GeneratedAt: time.Now(),
	}
	if sm == nil {
		return snap
	}

	// Totally ordered (parent, order index, id) so siblings sharing an order
	// index keep a fixed position instead of shuffling between snapshots (163).
	channels := sm.ChannelTreeOrdered()
	members := sm.ListClients()

	// Index channels by ID for quick lookup while building the tree.
	nodeByID := make(map[int64]*ChannelNode, len(channels))
	for _, ch := range channels {
		node := &ChannelNode{Channel: *ch}
		node.HasPassword = ch.PasswordHash != "" // (304) lock icon, without leaking the hash
		nodeByID[ch.ChannelID] = node
	}

	// Attach each node to its parent, or collect as root if ParentID == 0.
	for _, ch := range channels {
		node := nodeByID[ch.ChannelID]
		if ch.ParentID == 0 {
			snap.RootChannels = append(snap.RootChannels, node)
			continue
		}
		if parent, ok := nodeByID[ch.ParentID]; ok {
			parent.Children = append(parent.Children, node)
		} else {
			// Orphan (parent missing): treat as root to avoid losing it.
			snap.RootChannels = append(snap.RootChannels, node)
		}
	}

	// Populate clients per channel (filtering invisible users per viewer).
	total := 0
	for _, c := range members {
		if c.Status == "invisible" && !forAdmin && c.UniqueID != viewerUniqueID {
			continue
		}
		total++
		info := clientToInfo(c)
		if c.ChannelID != 0 {
			if node, ok := nodeByID[c.ChannelID]; ok {
				node.Clients = append(node.Clients, &info)
			}
		}
	}

	snap.TotalChannels = len(channels)
	snap.TotalClients = total

	return snap
}

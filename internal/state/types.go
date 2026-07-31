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
	"time"
)

// Client represents a connected user tracked in memory.
//
// Conn is nullable: headless clients (e.g. bots or server-side entities) may
// have a nil Conn. Metadata is an arbitrary string map for per-client extras.
type Client struct {
	ClientID    string
	UniqueID    string
	Nickname    string
	ChannelID   int64 // 0 means no channel
	IsSpeaking  bool
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
}

// SpeakingState records that a client is currently speaking in a channel.
type SpeakingState struct {
	ClientID  string
	ChannelID int64
	StartedAt time.Time
}

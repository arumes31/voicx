// Package query implements a TS3-style ServerQuery interface: a raw TCP,
// line-based text protocol for headless administration and bot integration.
//
// Protocol flavor (TS3-like, simplified):
//   - On connect the server sends a greeting banner.
//   - Commands are one per line: a command name followed by key=value pairs
//     (login takes positional arguments instead).
//   - Responses are zero or more result lines, then a final
//     "error id=<n> msg=<text>" line; id=0 means success.
//   - Values are escaped TS3-style (see protocol.go).
//
// The Server talks to the rest of voicex through the Backend interface only;
// production wiring lives in cmd/server/main.go.
package query

import (
	"context"
	"time"
)

// ClientInfo describes an online client (clientlist row).
type ClientInfo struct {
	ClientID  string
	UniqueID  string
	Nickname  string
	ChannelID int64
}

// ChannelInfo describes a channel (channellist row).
type ChannelInfo struct {
	ChannelID   int64
	ParentID    int64
	Name        string
	Type        int // 0=temporary, 1=semi-permanent, 2=permanent
	ClientCount int
}

// Info describes the server (serverinfo row).
type Info struct {
	Name           string
	Uptime         time.Duration
	ClientsOnline  int
	MaxClients     int
	ChannelsOnline int
}

// Complaint describes one complaint row (complaintlist).
type Complaint struct {
	ID        int64
	Reporter  string
	Target    string
	Reason    string
	CreatedAt time.Time
}

// Token describes one privilege token (tokenlist).
type Token struct {
	Key     string
	Type    int
	GroupID int64
	Uses    int
	MaxUses int
}

// Backend bundles the capabilities the query server needs. All methods are
// called from per-connection goroutines and must be safe for concurrent use.
type Backend interface {
	// Authenticate verifies uniqueID/password and reports whether the user
	// is a server admin. ok=false means bad credentials.
	Authenticate(ctx context.Context, uniqueID, password string) (ok, admin bool, err error)
	ListClients(ctx context.Context) []ClientInfo
	ListChannels(ctx context.Context) []ChannelInfo
	ServerInfo(ctx context.Context) Info
	// MoveClient moves a client into a channel (no permission check; the
	// query login is the authorization).
	MoveClient(ctx context.Context, clientID string, channelID int64) error
	// KickClient kicks a client from its channel or the server.
	KickClient(ctx context.Context, clientID string, fromServer bool, reason string) error
	// SendText injects a chat message: targetMode 1 = direct to client
	// (target is a client ID), 2 = channel (target is a channel ID),
	// 3 = global.
	SendText(ctx context.Context, targetMode int, target, msg string) error
	// CreateChannel creates a channel and returns its ID. channelType:
	// 0=temporary, 1=semi-permanent, 2=permanent.
	CreateChannel(ctx context.Context, name, topic string, channelType int) (int64, error)
	DeleteChannel(ctx context.Context, channelID int64) error
	// BanClient bans a client by unique ID (seconds <= 0 = permanent) and
	// kicks it from the server.
	BanClient(ctx context.Context, clientID string, seconds int64, reason string) error
	// Complaints.
	ListComplaints(ctx context.Context) ([]Complaint, error)
	DeleteComplaint(ctx context.Context, id int64) error
	DeleteAllComplaints(ctx context.Context) error
	// Privilege tokens. TokenAdd creates a token (groupID 0 = admin grant)
	// and returns its key.
	TokenAdd(ctx context.Context, tokenType int, groupID int64) (string, error)
	TokenList(ctx context.Context) ([]Token, error)
	TokenDelete(ctx context.Context, key string) error
}

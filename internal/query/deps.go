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

// ChannelInfo describes a channel (channellist/channelinfo rows).
type ChannelInfo struct {
	ChannelID   int64
	ParentID    int64
	Name        string
	Topic       string
	Type        int // 0=temporary, 1=semi-permanent, 2=permanent
	MaxClients  int // 0 = unlimited
	ClientCount int
	// Per-channel Opus audio quality (0 bitrate = server default 32k).
	OpusBitrate     int
	OpusFEC         bool
	OpusDTX         bool
	OpusStereo      bool
	SlowModeSeconds int
}

// ChannelEditParams carries channeledit arguments. Nil pointers leave the
// field unchanged.
type ChannelEditParams struct {
	Topic           *string
	MaxClients      *int
	OpusBitrate     *int
	OpusFEC         *bool
	OpusDTX         *bool
	OpusStereo      *bool
	SlowModeSeconds *int
	// Join power (160), sort index (163), parent (168) and the permission
	// inheritance toggle (157), so ServerQuery can drive the same edits as the
	// control channel.
	NeededJoinPower    *int
	OrderIndex         *int
	ParentID           *int64
	InheritPermissions *bool
}

// ServerEditParams carries serveredit arguments (217). Nil pointers leave the
// setting untouched; a non-nil empty string (or MaxClients 0) clears the
// stored override so the config.yaml value applies again.
type ServerEditParams struct {
	Name       *string
	Welcome    *string
	MaxClients *int
}

// Info describes the server (serverinfo row).
type Info struct {
	Name           string
	Uptime         time.Duration
	ClientsOnline  int
	MaxClients     int
	ChannelsOnline int
	// TLSFingerprint is the control-channel certificate fingerprint (empty
	// when TLS is disabled).
	TLSFingerprint string
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

// AuditEntry describes one audit log row (auditlog).
type AuditEntry struct {
	ID        int64
	Actor     string
	Action    string
	Target    string
	Detail    string
	CreatedAt time.Time
}

// PermLine is one resolved permission of a user (permoverview).
type PermLine struct {
	Key   string
	Value int
	Grant int
	Tier  string
}

// ChannelPerm is one channel-tier permission entry (220).
type ChannelPerm struct {
	Key    string
	Value  int
	Grant  int
	Skip   bool
	Negate bool
}

// GroupInfo is one server group (221).
type GroupInfo struct {
	ID          int64
	Name        string
	SortID      int
	MemberCount int
}

// GroupMemberInfo is one server-group member (221).
type GroupMemberInfo struct {
	UniqueID  string
	Nickname  string
	ExpiresAt int64 // unix; 0 = permanent
}

// CustomProp is one custom client property (222).
type CustomProp struct {
	Key   string
	Value string
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
	// ChannelInfo returns one channel's details (ok=false when unknown).
	ChannelInfo(ctx context.Context, channelID int64) (ChannelInfo, bool)
	// EditChannel applies non-nil fields of params to a channel.
	EditChannel(ctx context.Context, channelID int64, params ChannelEditParams) error
	// ServerSet stores a server setting (motd, announcement); setting
	// "announcement" additionally broadcasts it to online clients.
	ServerSet(ctx context.Context, key, value string) error
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
	// AuditLog returns the newest audit entries (limit <= 0 = default page).
	AuditLog(ctx context.Context, limit int) ([]AuditEntry, error)

	// Wave 10a (217-223).
	// ServerEdit applies the non-nil fields of params; an empty string or a
	// zero max-clients CLEARS the stored override back to the config default.
	ServerEdit(ctx context.Context, params ServerEditParams) error
	// Shutdown gracefully stops the server; restart=true documents that a
	// supervisor (docker restart policy) brings it back.
	Shutdown(ctx context.Context, restart bool) error
	// PermOverview returns the resolved permissions of a user (219).
	PermOverview(ctx context.Context, uniqueID string, channelID int64) ([]PermLine, error)
	// Channel tier permissions (220). actor is the query user (audit).
	ChannelPermList(ctx context.Context, channelID int64) ([]ChannelPerm, error)
	ChannelAddPerm(ctx context.Context, actor string, channelID int64, key string, value, grant int, skip, negate bool) error
	ChannelDelPerm(ctx context.Context, actor string, channelID int64, key string) error
	// Server groups (221). durationSeconds 0 = permanent membership.
	ServerGroupAdd(ctx context.Context, actor, name string, sortID int) (int64, error)
	ServerGroupDel(ctx context.Context, actor string, groupID int64, force bool) error
	ServerGroupAddClient(ctx context.Context, actor string, groupID int64, uniqueID string, durationSeconds int64) error
	ServerGroupDelClient(ctx context.Context, actor string, groupID int64, uniqueID string) error
	ServerGroupList(ctx context.Context) ([]GroupInfo, error)
	ServerGroupClientList(ctx context.Context, groupID int64) ([]GroupMemberInfo, error)
	// Custom client properties (222).
	CustomSet(ctx context.Context, uniqueID, key, value string) error
	CustomDel(ctx context.Context, uniqueID, key string) error
	CustomInfo(ctx context.Context, uniqueID string) ([]CustomProp, error)
	// LogView returns recent server log lines (223).
	LogView(ctx context.Context, lines int, filter string) ([]string, error)
	// ServerRules returns the operator-defined rules shown on first join, the
	// content hash acceptances are keyed by, and how many users accepted the
	// current wording (215).
	ServerRules(ctx context.Context) (text, hash string, accepted int, err error)
	// LogFollow streams log lines emitted from the call onward; cancel
	// unregisters the follower (223). The channel is never closed by the
	// producer, so a caller that stops reading must call cancel.
	LogFollow() (lines <-chan string, cancel func())
}

// deps.go defines the backend services the TCP control server depends on and
// the small interfaces used to consume them. The interfaces are satisfied by
// the concrete production types (auth.AuthService, channels.ChannelManager,
// permissions.Loader, store.Store) and make the handlers testable with fakes
// that need no database.
package server

import (
	"context"
	"database/sql"
	"time"

	"voicx/internal/auth"
	"voicx/internal/broadcast"
	"voicx/internal/channels"
	"voicx/internal/chatcrypto"
	"voicx/internal/metrics"
	"voicx/internal/netproto"
	"voicx/internal/permissions"
	"voicx/internal/recorder"
	"voicx/internal/state"
	"voicx/internal/store"
	"voicx/internal/webrtc"
)

// AuthBackend is the subset of auth.AuthService the TCP server needs.
type AuthBackend interface {
	AuthenticatePassword(ctx context.Context, uniqueID, password string) (bool, error)
	AuthenticateChallenge(ctx context.Context, uniqueID string, challenge, signature []byte) (bool, error)
	AuthenticateNickname(ctx context.Context, nickname, password string) (*auth.User, error)
	LookupUser(ctx context.Context, uniqueID string) (*auth.User, error)
	LookupUserByPublicKey(ctx context.Context, publicKey string) (*auth.User, error)
	LookupActiveBan(ctx context.Context, uniqueID, ip string) (*auth.Ban, error)
	BindPublicKey(ctx context.Context, userID int64, publicKey string) error
	// E2EE key directory (wave 4b): registered users' X25519 keys persist.
	SetE2EPublicKey(ctx context.Context, userID int64, publicKey string) error
	GetE2EPublicKey(ctx context.Context, uniqueID string) (string, error)
}

// ChannelBackend is the subset of channels.ChannelManager the TCP server needs.
type ChannelBackend interface {
	CreateChannel(ctx context.Context, spec channels.ChannelSpec) (int64, error)
	DeleteChannel(ctx context.Context, channelID int64) error
	UpdateChannel(ctx context.Context, channelID int64, upd channels.ChannelUpdate) error
	OnClientJoinedChannel(channelID int64)
	OnClientLeftChannel(channelID int64)
}

// PermLoader is the subset of permissions.Loader the TCP server needs.
type PermLoader interface {
	LoadForClient(ctx context.Context, userID, channelID int64) (permissions.TieredPermissions, error)
	Invalidate(userID, channelID int64)
	// InvalidateAll clears the whole cache (after group/template writes).
	InvalidateAll()
	// LoadGroupPermissions returns one server group's permission set.
	LoadGroupPermissions(ctx context.Context, groupID int64) (permissions.PermissionSet, error)
}

// BanStore is the minimal store surface needed to record bans. It is
// satisfied by *store.Store.
type BanStore interface {
	DB() *sql.DB
}

// SpoolStore is the subset of the store needed for offline message spooling.
// Messages may be E2EE ciphertext; fromUniqueID lets the recipient fetch the
// sender's public key at delivery time. It is satisfied by *store.Store.
type SpoolStore interface {
	SpoolMessage(ctx context.Context, fromUserID, toUserID int64, fromUniqueID, message string) error
	PendingMessages(ctx context.Context, toUserID int64) ([]store.SpooledMessage, error)
	MarkMessagesDelivered(ctx context.Context, ids []int64) error
}

// VoiceBackend is the subset of webrtc.Voice the TCP server needs for the
// voice pipeline: WebRTC signaling, channel routing membership, and whisper
// configuration. It uses plain types only, so fakes need no Pion.
type VoiceBackend interface {
	// SetHandlers installs the talk-permission gate and the speaking-state
	// callback.
	SetHandlers(canTalk func(clientID string) bool, onSpeaking func(clientID string, speaking bool))
	// SetVideoHandlers installs the video-publish permission gate.
	SetVideoHandlers(canVideo func(clientID string) bool)
	// HandleOffer applies an SDP offer and returns the SDP answer.
	// onLocalCandidate is invoked asynchronously for each local ICE candidate.
	HandleOffer(clientID, offerSDP string, onLocalCandidate func(candidate, sdpMid string, mlineIndex uint16)) (string, error)
	HandleAnswer(clientID, answerSDP string) error
	AddICECandidate(clientID, candidate, sdpMid string, mlineIndex uint16) error
	ClosePeer(clientID string) error
	JoinChannel(clientID string, channelID int64)
	LeaveChannel(clientID string, channelID int64)
	SetWhisper(clientID string, clients []string, channels []int64, active bool)
	// SetVideoQuality sets the client's preferred simulcast layer.
	SetVideoQuality(clientID, quality string) error
	// AddTap registers an extra subscriber (e.g. a recorder) in a channel.
	AddTap(channelID int64, tapID string, audio, video webrtc.TrackWriter)
	RemoveTap(tapID string)
	// PeerCount returns the number of active peer connections (metrics).
	PeerCount() int
	// SetOfferSender installs the delivery callback for server-initiated
	// renegotiation offers.
	SetOfferSender(fn func(clientID, offerSDP string) error)
}

// RecordingBackend is the subset of recorder.Recorder the TCP server needs.
type RecordingBackend interface {
	Start(ctx context.Context, channelID int64, router recorder.TapRouter) (*recorder.Session, error)
	Stop(channelID int64) error
}

// FileTransferBackend is the subset of filetransfer.Server the TCP server
// needs: issuing single-use transfer tokens, listing/managing channel files,
// and minting download links. The file-transfer port itself trusts only the
// token; permission checks happen here, at issue time.
type FileTransferBackend interface {
	// InitUpload takes the caller's personal upload ceiling in MiB (266,
	// 0 = unlimited); the channel ceiling (265) is server configuration and
	// the backend already knows it.
	InitUpload(ctx context.Context, channelID int64, folder, name string, size int64, uploader string, uploaderQuotaMB int64) (transferID, token string, err error)
	InitDownload(ctx context.Context, channelID int64, folder, name string) (transferID, token string, err error)
	ListFiles(ctx context.Context, channelID int64, folder string) ([]store.FileRecord, error)
	ListFileFolders(ctx context.Context, channelID int64) ([]string, error)
	ListFileVersions(ctx context.Context, channelID int64, folder, name string) ([]store.FileRecord, error)
	DeleteFile(ctx context.Context, channelID int64, folder, name string) error
	RenameFile(ctx context.Context, channelID int64, folder, name, newFolder, newName string) error
	// MoveFile relocates a file, possibly into another channel (262). The
	// caller checks permissions on the source AND the destination.
	MoveFile(ctx context.Context, channelID int64, folder, name string, newChannelID int64, newFolder, newName string) error
	ChannelQuota(ctx context.Context, channelID int64) (used, quota int64, err error)
	CreateLink(ctx context.Context, channelID int64, folder, name string) (token string, expires time.Time, err error)
	Port() int
	// Fingerprint is the SHA-256 of the certificate the data port presents,
	// "" when its TLS is off. It travels in FileTransferInitResponse so the
	// client can pin the same certificate the control channel uses (91).
	Fingerprint() string
}

// TokenBackend is the subset of the store needed to redeem and administer
// privilege tokens (174).
type TokenBackend interface {
	UseToken(ctx context.Context, key string, userID int64) (groupID int64, err error)
	ListTokens(ctx context.Context) ([]store.Token, error)
	CreateTokenWithMeta(ctx context.Context, tokenType int, groupID, channelID int64, description string, maxUses int) (string, error)
	DeleteToken(ctx context.Context, key string) error
}

// GroupStore is the subset of the store needed for wave-6a group and
// permission management. It is satisfied by *store.Store.
type GroupStore interface {
	ListGroups(ctx context.Context, groupType string) ([]store.Group, error)
	GetGroup(ctx context.Context, groupType string, id int64) (*store.Group, error)
	CreateGroup(ctx context.Context, groupType, name string, sortID int) (int64, error)
	RenameGroup(ctx context.Context, groupType string, id int64, name string) error
	DeleteGroup(ctx context.Context, groupType string, id int64, force bool) error
	AssignServerGroup(ctx context.Context, groupID, userID int64, expiresIn time.Duration) error
	UnassignServerGroup(ctx context.Context, groupID, userID int64) error
	AssignChannelGroup(ctx context.Context, groupID, userID, channelID int64) error
	UnassignChannelGroup(ctx context.Context, userID, channelID int64) error
	UserGroupIDs(ctx context.Context, userID int64) ([]int64, error)
	FindGroupByName(ctx context.Context, groupType, name string) (*store.Group, error)
	// ExpiredGroupMembers removes expired timed memberships (145) and
	// returns the affected (userID, groupID) pairs.
	ExpiredGroupMembers(ctx context.Context, now time.Time) ([][2]int64, error)
	// UserUniqueID maps a users.id back to its unique ID (reaper notify).
	UserUniqueID(ctx context.Context, userID int64) (string, error)
	// SetGroupIcon marks a server group's icon file name.
	SetGroupIcon(ctx context.Context, groupID int64, icon string) error
	// SetGroupCosmetics writes a server group's colour/hoist/sort (178/179);
	// a nil field is left unchanged.
	SetGroupCosmetics(ctx context.Context, groupID int64, color *string, hoist *bool, sortID *int) error
	SetPermission(ctx context.Context, tier store.PermTier, target store.PermTarget, key string, value, grant int, skip, negate bool) error
	UnsetPermission(ctx context.Context, tier store.PermTier, target store.PermTarget, key string) error
	// ListPermissions is the editor read path (wave 6b).
	ListPermissions(ctx context.Context, tier store.PermTier, target store.PermTarget) ([]store.PermEntry, error)
	Audit(ctx context.Context, actor, action, target, detail string)
	AuditList(ctx context.Context, beforeID int64, limit int) ([]store.AuditEntry, error)
	// Member listings (wave 6b group management UI).
	ListServerGroupMembers(ctx context.Context, groupID int64) ([]store.GroupMember, error)
	ListChannelGroupMembers(ctx context.Context, groupID, channelID int64) ([]store.GroupMember, error)
}

// BanAdminStore is the subset of the store needed for ban administration
// (wave 6b ban list dialog). It is satisfied by *store.Store.
type BanAdminStore interface {
	ListBans(ctx context.Context) ([]store.BanRecord, error)
	DeleteBan(ctx context.Context, id int64) error
}

// ComplaintBackend is the subset of the store needed to file and review
// complaints (173).
type ComplaintBackend interface {
	AddComplaint(ctx context.Context, reporter, target, reason string) error
	ListComplaints(ctx context.Context) ([]store.Complaint, error)
	DeleteComplaintsAgainst(ctx context.Context, target, reporter string) (int64, error)
}

// ChatStore is the subset of the store needed for server-side chat
// infrastructure: history, pins, reactions, server settings, and the one-time
// legacy plaintext backfill. Bodies cross this interface as ciphertext plus
// the scope generation that sealed them (91). It is satisfied by *store.Store.
type ChatStore interface {
	StoreChatMessage(ctx context.Context, channelID int64, fromUniqueID, fromNickname, bodyEnc string, keyID uint32) (int64, error)
	ChatHistory(ctx context.Context, channelID, beforeID int64, limit int) ([]store.ChatMessage, error)
	GetChatMessage(ctx context.Context, id int64) (*store.ChatMessage, error)
	EditChatMessage(ctx context.Context, id int64, bodyEnc string, keyID uint32) error
	DeleteChatMessage(ctx context.Context, id int64) error
	PinChatMessage(ctx context.Context, channelID, messageID int64, pinnedBy string) error
	UnpinChatMessage(ctx context.Context, channelID, messageID int64) error
	ChatPins(ctx context.Context, channelID int64) ([]store.PinnedMessage, error)
	ToggleReaction(ctx context.Context, messageID int64, uniqueID, emoji string) (map[string]int, bool, error)
	ReactionsFor(ctx context.Context, ids []int64) (map[int64]map[string]int, error)
	SetServerSetting(ctx context.Context, key, value string, keyID uint32) error
	GetServerSetting(ctx context.Context, key string) (string, uint32, error)

	// Legacy plaintext backfill (012). It runs once, before the listener
	// binds, and is fatal on error: a partially encrypted table behind a
	// NOT VALID constraint is the silent-plaintext state 91 forbids.
	LegacyPlaintextPage(ctx context.Context, afterID int64, limit int) ([]store.LegacyChatRow, error)
	SetChatCiphertext(ctx context.Context, id int64, bodyEnc string, keyID uint32) error
	PurgeLegacyPlaintext(ctx context.Context) (int64, error)
	CountPlaintextBodies(ctx context.Context) (int64, error)
	ValidateChatNoPlaintext(ctx context.Context) error
}

// ScopeKeyStore persists the per-scope chat key generations. Generation ids
// are allocated by the database and never reused, so a generation can never
// be re-minted with different key material. It is satisfied by *store.Store.
type ScopeKeyStore interface {
	CountScopeKeys(ctx context.Context) (int64, error)
	AllocScopeKeyID(ctx context.Context, scope int64) (uint32, error)
	CurrentScopeKey(ctx context.Context, scope int64) (*store.ScopeKey, error)
	GetScopeKey(ctx context.Context, scope int64, keyID uint32) (*store.ScopeKey, error)
	InsertScopeKey(ctx context.Context, scope int64, keyID uint32, wrapped []byte, kekID uint16) error
	RotateScopeKey(ctx context.Context, scope int64, newKeyID uint32, wrapped []byte, kekID uint16) error
}

// Deps bundles the backend services the TCP control server relies on. Any
// field may be nil; handlers that need a missing dependency reply with an
// "unavailable" error instead of panicking, which keeps the server usable in
// tests and during partial startups.
type Deps struct {
	Auth         AuthBackend
	State        *state.Manager
	Channels     ChannelBackend
	Broadcast    *broadcast.Broadcaster
	Perms        PermLoader
	Resolver     *permissions.Resolver
	Bans         BanStore
	Spool        SpoolStore
	Voice        VoiceBackend
	Recorder     RecordingBackend
	FileTransfer FileTransferBackend
	Tokens       TokenBackend
	Complaints   ComplaintBackend
	Chat         ChatStore
	Groups       GroupStore
	BanAdmin     BanAdminStore
	Metrics      metrics.Sink

	// ScopeKeys and ChatKEK back the chat key manager (91). Without both,
	// chat is unavailable rather than silently RAM-only: a key that does not
	// survive a restart would make stored ciphertext unreadable forever.
	ScopeKeys ScopeKeyStore
	ChatKEK   *chatcrypto.KEKRing

	// ServerPasswordHash, when non-empty, requires clients to supply the
	// global server password at authenticate time (verified with Argon2id).
	ServerPasswordHash string

	// Default groups (6a): registered users with no memberships are assigned
	// DefaultMemberGroupID at login; guests virtually hold
	// DefaultGuestGroupID's permissions. 0 disables the behavior.
	DefaultGuestGroupID  int64
	DefaultMemberGroupID int64

	// ICEServers, when non-nil, returns the ICE servers a client should use
	// for WebRTC: the STUN defaults plus, when TURN is configured, a TURN
	// entry with time-limited REST API credentials minted for the client's
	// unique ID. Delivered in the auth response (446).
	ICEServers func(clientUniqueID string) []netproto.ICEServer
}

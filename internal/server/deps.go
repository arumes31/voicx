// deps.go defines the backend services the TCP control server depends on and
// the small interfaces used to consume them. The interfaces are satisfied by
// the concrete production types (auth.AuthService, channels.ChannelManager,
// permissions.Loader, store.Store) and make the handlers testable with fakes
// that need no database.
package server

import (
	"context"
	"database/sql"

	"voicx/internal/auth"
	"voicx/internal/broadcast"
	"voicx/internal/channels"
	"voicx/internal/metrics"
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
}

// ChannelBackend is the subset of channels.ChannelManager the TCP server needs.
type ChannelBackend interface {
	CreateChannel(ctx context.Context, spec channels.ChannelSpec) (int64, error)
	DeleteChannel(ctx context.Context, channelID int64) error
	OnClientJoinedChannel(channelID int64)
	OnClientLeftChannel(channelID int64)
}

// PermLoader is the subset of permissions.Loader the TCP server needs.
type PermLoader interface {
	LoadForClient(ctx context.Context, userID, channelID int64) (permissions.TieredPermissions, error)
	Invalidate(userID, channelID int64)
}

// BanStore is the minimal store surface needed to record bans. It is
// satisfied by *store.Store.
type BanStore interface {
	DB() *sql.DB
}

// SpoolStore is the subset of the store needed for offline message spooling.
// It is satisfied by *store.Store.
type SpoolStore interface {
	SpoolMessage(ctx context.Context, fromUserID, toUserID int64, message string) error
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
}

// RecordingBackend is the subset of recorder.Recorder the TCP server needs.
type RecordingBackend interface {
	Start(ctx context.Context, channelID int64, router recorder.TapRouter) (*recorder.Session, error)
	Stop(channelID int64) error
}

// FileTransferBackend is the subset of filetransfer.Server the TCP server
// needs: issuing single-use transfer tokens and listing channel files. The
// file-transfer port itself trusts only the token; permission checks happen
// here, at issue time.
type FileTransferBackend interface {
	InitUpload(ctx context.Context, channelID int64, name string, size int64, uploader string) (transferID, token string, err error)
	InitDownload(ctx context.Context, channelID int64, name string) (transferID, token string, err error)
	ListFiles(ctx context.Context, channelID int64) ([]store.FileRecord, error)
	Port() int
}

// TokenBackend is the subset of the store needed to redeem privilege tokens.
type TokenBackend interface {
	UseToken(ctx context.Context, key string, userID int64) (groupID int64, err error)
}

// ComplaintBackend is the subset of the store needed to file complaints.
type ComplaintBackend interface {
	AddComplaint(ctx context.Context, reporter, target, reason string) error
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
	Metrics      metrics.Sink

	// ServerPasswordHash, when non-empty, requires clients to supply the
	// global server password at authenticate time (verified with Argon2id).
	ServerPasswordHash string
}

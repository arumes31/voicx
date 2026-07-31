// Package netproto: codec.go defines the message type registry and a small
// set of message structs used by the control channel. Encoding is JSON for
// now (codegen is deferred); the framing layer is type-agnostic so the wire
// format can be swapped later without changing the transport.
package netproto

import (
	"encoding/json"
	"fmt"
	"time"
)

// MessageType is the numeric identifier carried in the Frame.Type field.
type MessageType uint16

// Control-channel message type constants. Keep these stable; clients and
// servers rely on them for dispatch.
const (
	MsgAuthenticate  MessageType = 1 // client -> server: authenticate request
	MsgAuthResponse  MessageType = 2 // server -> client: authentication result
	MsgCreateChannel MessageType = 3 // client -> server: create a channel
	MsgChannelList   MessageType = 4 // server -> client: list of channels
	MsgChatSend      MessageType = 5 // client -> server: send a chat message
	MsgChatBroadcast MessageType = 6 // server -> client: broadcast a chat message
	MsgError         MessageType = 7 // server -> client: error report
	MsgPing          MessageType = 8 // client -> server: liveness probe
	MsgPong          MessageType = 9 // server -> client: liveness reply

	MsgJoinChannel   MessageType = 10 // client -> server: join a channel
	MsgDeleteChannel MessageType = 11 // client -> server: delete a channel
	MsgMoveClient    MessageType = 12 // client -> server: move another client into a channel
	MsgKickClient    MessageType = 13 // client -> server: kick (or ban) a client
	MsgSnapshot      MessageType = 14 // server -> client: full channel-tree snapshot (payload is a broadcast.TreeSnapshot JSON document)
	MsgEvent         MessageType = 15 // server -> client: asynchronous event envelope {"type": ..., "data": ...}
	MsgAuthChallenge MessageType = 16 // server -> client: challenge for challenge-response auth
	MsgAuthSignature MessageType = 17 // client -> server: signed auth challenge

	MsgWebRTCOffer    MessageType = 18 // client -> server: SDP offer (server -> client: renegotiation offer)
	MsgWebRTCAnswer   MessageType = 19 // server -> client: SDP answer (client -> server: renegotiation answer)
	MsgICECandidate   MessageType = 20 // both directions: trickle ICE candidate
	MsgWhisperSet     MessageType = 21 // client -> server: configure whisper targets/mode
	MsgPositionUpdate MessageType = 22 // client -> server: 3D position publish

	MsgVideoQuality     MessageType = 23 // client -> server: request simulcast layer (high/mid/low)
	MsgRecordingControl MessageType = 24 // client -> server: start/stop channel recording

	MsgFileTransferInit         MessageType = 25 // client -> server: request a transfer token
	MsgFileTransferInitResponse MessageType = 26 // server -> client: issued transfer token
	MsgFileList                 MessageType = 27 // client -> server: list a channel's files
	MsgFileListResponse         MessageType = 28 // server -> client: channel file listing

	MsgAvatarSet      MessageType = 29 // client -> server: set own avatar (base64 image)
	MsgAvatarGet      MessageType = 30 // client -> server: request a user's avatar
	MsgAvatarData     MessageType = 31 // server -> client: avatar image data
	MsgChannelIconSet MessageType = 32 // client -> server: set a channel icon
	MsgTokenUse       MessageType = 33 // client -> server: redeem a privilege token
	MsgComplaint      MessageType = 34 // client -> server: file a complaint
	MsgScreenShare    MessageType = 35 // client -> server: declare screen-share state

	MsgPermissionsQuery    MessageType = 36 // client -> server: request own resolved permissions
	MsgPermissionsResponse MessageType = 37 // server -> client: resolved permission set

	MsgClientInfoQuery    MessageType = 38 // client -> server: request a client's connection info
	MsgClientInfoResponse MessageType = 39 // server -> client: client connection info
)

// String returns a human-readable name for the message type.
func (m MessageType) String() string {
	switch m {
	case MsgAuthenticate:
		return "Authenticate"
	case MsgAuthResponse:
		return "AuthResponse"
	case MsgCreateChannel:
		return "CreateChannel"
	case MsgChannelList:
		return "ChannelList"
	case MsgChatSend:
		return "ChatSend"
	case MsgChatBroadcast:
		return "ChatBroadcast"
	case MsgError:
		return "Error"
	case MsgPing:
		return "Ping"
	case MsgPong:
		return "Pong"
	case MsgJoinChannel:
		return "JoinChannel"
	case MsgDeleteChannel:
		return "DeleteChannel"
	case MsgMoveClient:
		return "MoveClient"
	case MsgKickClient:
		return "KickClient"
	case MsgSnapshot:
		return "Snapshot"
	case MsgEvent:
		return "Event"
	case MsgAuthChallenge:
		return "AuthChallenge"
	case MsgAuthSignature:
		return "AuthSignature"
	case MsgWebRTCOffer:
		return "WebRTCOffer"
	case MsgWebRTCAnswer:
		return "WebRTCAnswer"
	case MsgICECandidate:
		return "ICECandidate"
	case MsgWhisperSet:
		return "WhisperSet"
	case MsgPositionUpdate:
		return "PositionUpdate"
	case MsgVideoQuality:
		return "VideoQuality"
	case MsgRecordingControl:
		return "RecordingControl"
	case MsgFileTransferInit:
		return "FileTransferInit"
	case MsgFileTransferInitResponse:
		return "FileTransferInitResponse"
	case MsgFileList:
		return "FileList"
	case MsgFileListResponse:
		return "FileListResponse"
	case MsgAvatarSet:
		return "AvatarSet"
	case MsgAvatarGet:
		return "AvatarGet"
	case MsgAvatarData:
		return "AvatarData"
	case MsgChannelIconSet:
		return "ChannelIconSet"
	case MsgTokenUse:
		return "TokenUse"
	case MsgComplaint:
		return "Complaint"
	case MsgScreenShare:
		return "ScreenShare"
	case MsgPermissionsQuery:
		return "PermissionsQuery"
	case MsgPermissionsResponse:
		return "PermissionsResponse"
	case MsgClientInfoQuery:
		return "ClientInfoQuery"
	case MsgClientInfoResponse:
		return "ClientInfoResponse"
	default:
		return fmt.Sprintf("Unknown(%d)", uint16(m))
	}
}

// Authenticate is sent by a client to authenticate. Username carries the
// user's TS3-style unique ID — or, for password login, a nickname (the
// server tries unique ID first, then nickname). When Password is non-empty
// it is verified against the stored Argon2id hash; when Password is empty
// the server starts a challenge-response handshake and replies with an
// AuthChallenge. ServerPassword is required when the server has a global
// password set. PublicKey is the client's Ed25519 identity key (PEM); on a
// successful nickname login the server binds it to the account so future
// challenge logins work with the same key.
//
// Anonymous guest login (TS3-style): with Anonymous set and no Username or
// Password, the server authenticates the client immediately as a guest with
// an ephemeral guest: unique ID and the given Nickname. With Anonymous set
// and a client-derived Username (unique ID of the client's own Ed25519
// identity), the challenge handshake runs as usual; presenting the matching
// PublicKey in AuthSignature then authenticates the client as a guest with a
// stable, key-derived unique ID even when no users row exists.
type Authenticate struct {
	Username       string `json:"username"`
	Password       string `json:"password,omitempty"`
	ServerPassword string `json:"server_password,omitempty"`
	Nickname       string `json:"nickname,omitempty"`
	Anonymous      bool   `json:"anonymous,omitempty"`
	PublicKey      string `json:"public_key,omitempty"`
	Token          string `json:"token,omitempty"`
}

// AuthResponse is the server's reply to an Authenticate message.
type AuthResponse struct {
	OK       bool   `json:"ok"`
	ClientID string `json:"client_id,omitempty"`
	UniqueID string `json:"unique_id,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// AuthChallenge is the server's challenge in the challenge-response (Ed25519)
// authentication handshake. The client must sign Challenge with its identity
// private key and reply with an AuthSignature.
type AuthChallenge struct {
	Challenge []byte `json:"challenge"`
}

// AuthSignature is the client's reply to an AuthChallenge, completing the
// challenge-response handshake. PublicKey (PEM) is optional: registered
// users omit it (the server uses the stored key). Guests present it so the
// server can verify the signature and derive their key-based unique ID.
type AuthSignature struct {
	UniqueID  string `json:"unique_id"`
	PublicKey string `json:"public_key,omitempty"`
	Signature []byte `json:"signature"`
}

// CreateChannel requests the creation of a new channel. Type mirrors the
// channels.ChannelType values: 0=temporary, 1=semi-permanent, 2=permanent.
// NeededJoinPower is the i_channel_join_power required to join the channel.
type CreateChannel struct {
	Name            string `json:"name"`
	Topic           string `json:"topic,omitempty"`
	ParentID        int64  `json:"parent_id,omitempty"`
	Type            int    `json:"type"`
	MaxClients      int    `json:"max_clients,omitempty"`
	Password        string `json:"password,omitempty"`
	NeededJoinPower int    `json:"needed_join_power,omitempty"`
}

// Channel describes a single channel in a ChannelList.
type Channel struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Clients  int    `json:"clients"`
	Password bool   `json:"password"`
}

// ChannelList is sent by the server to enumerate available channels.
type ChannelList struct {
	Channels []Channel `json:"channels"`
}

// ChatSend is a chat message from a client to the server. ChannelID set means
// channel chat; ToUniqueID set means a direct message to a user (spooled when
// offline); ToClientID set means a direct message to an online connection;
// none set means a global (server-wide) message.
type ChatSend struct {
	ChannelID  string `json:"channel_id,omitempty"`
	ToClientID string `json:"to_client_id,omitempty"`
	ToUniqueID string `json:"to_unique_id,omitempty"`
	Text       string `json:"text"`
}

// ChatBroadcast is a chat message the server fans out to interested clients.
// Offline marks messages delivered from the offline-message spool after login.
type ChatBroadcast struct {
	ChannelID    string `json:"channel_id,omitempty"`
	FromClientID string `json:"from_client_id,omitempty"`
	From         string `json:"from"`
	Text         string `json:"text"`
	Offline      bool   `json:"offline,omitempty"`
}

// Error carries a server-side error to the client.
type Error struct {
	Code    uint16 `json:"code"`
	Message string `json:"message"`
}

// Ping is a liveness probe. Payload is ignored.
type Ping struct{}

// Pong is the reply to a Ping.
type Pong struct{}

// JoinChannel requests that the calling client joins (moves into) a channel.
type JoinChannel struct {
	ChannelID int64  `json:"channel_id"`
	Password  string `json:"password,omitempty"`
}

// DeleteChannel requests the deletion of a channel.
type DeleteChannel struct {
	ChannelID int64 `json:"channel_id"`
}

// MoveClient requests moving another client into a channel.
type MoveClient struct {
	ClientID  string `json:"client_id"`
	ChannelID int64  `json:"channel_id"`
}

// KickClient requests kicking a client from its channel or from the server.
// Ban additionally records a unique-ID ban before removing the client; a ban
// always implies FromServer.
type KickClient struct {
	ClientID   string `json:"client_id"`
	FromServer bool   `json:"from_server,omitempty"`
	Ban        bool   `json:"ban,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// WebRTCOffer carries an SDP offer. From a client it starts a WebRTC session;
// from the server it requests renegotiation.
type WebRTCOffer struct {
	SDP string `json:"sdp"`
}

// WebRTCAnswer carries an SDP answer to a previously received offer.
type WebRTCAnswer struct {
	SDP string `json:"sdp"`
}

// ICECandidate carries a single trickle ICE candidate in either direction.
type ICECandidate struct {
	Candidate     string `json:"candidate"`
	SDPMid        string `json:"sdp_mid,omitempty"`
	SDPMLineIndex uint16 `json:"sdp_mline_index,omitempty"`
}

// WhisperSet configures the calling client's whisper list and mode. UniqueIDs
// are target users (resolved to online connections at set time); ChannelIDs
// are target channels whose members receive the audio. While Active is true,
// the client's outgoing audio is routed to the whisper targets instead of
// their channel.
type WhisperSet struct {
	UniqueIDs  []string `json:"unique_ids,omitempty"`
	ChannelIDs []int64  `json:"channel_ids,omitempty"`
	Active     bool     `json:"active"`
}

// PositionUpdate publishes the client's 3D position for positional audio. It
// is relayed to the other members of the client's channel as a position event.
type PositionUpdate struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

// VideoQuality requests a simulcast layer for the video the client receives.
// Quality is "high", "mid", or "low" (mapped to RID f/h/q server-side, with
// fallback to the closest published layer).
type VideoQuality struct {
	Quality string `json:"quality"`
}

// RecordingControl starts or stops a server-side recording of a channel.
// Action is "start" or "stop".
type RecordingControl struct {
	ChannelID int64  `json:"channel_id"`
	Action    string `json:"action"`
}

// FileTransferInit requests a transfer token for a file upload or download.
// Direction is "upload" or "download"; Size is the declared file size in
// bytes (uploads only; the server enforces its per-transfer cap).
type FileTransferInit struct {
	ChannelID int64  `json:"channel_id"`
	Direction string `json:"direction"`
	Name      string `json:"name"`
	Size      int64  `json:"size,omitempty"`
}

// FileTransferInitResponse carries an issued single-use transfer token and
// the port of the file-transfer server the client should connect to.
type FileTransferInitResponse struct {
	TransferID string `json:"transfer_id"`
	Token      string `json:"token"`
	Port       int    `json:"port"`
}

// FileList requests the file listing of a channel. Path is reserved for
// future subdirectory support and must currently be empty or "/".
type FileList struct {
	ChannelID int64  `json:"channel_id"`
	Path      string `json:"path,omitempty"`
}

// FileEntry describes one file in a FileListResponse.
type FileEntry struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"`
	Uploader   string    `json:"uploader,omitempty"`
	UploadedAt time.Time `json:"uploaded_at"`
}

// FileListResponse carries a channel's file listing.
type FileListResponse struct {
	Entries []FileEntry `json:"entries"`
}

// AvatarSet uploads the client's avatar image (base64). Accepted image
// types: PNG, JPEG, GIF, WebP; max 256 KiB after decoding.
type AvatarSet struct {
	DataBase64 string `json:"data_base64"`
}

// AvatarGet requests another user's avatar.
type AvatarGet struct {
	UniqueID string `json:"unique_id"`
}

// AvatarData is the server's reply to AvatarGet.
type AvatarData struct {
	UniqueID    string `json:"unique_id"`
	DataBase64  string `json:"data_base64"`
	ContentType string `json:"content_type"`
}

// ChannelIconSet uploads a channel icon (same validation as avatars).
type ChannelIconSet struct {
	ChannelID  int64  `json:"channel_id"`
	DataBase64 string `json:"data_base64"`
}

// TokenUse redeems a privilege token (server-group membership or, for
// group-less tokens, server admin).
type TokenUse struct {
	Token string `json:"token"`
}

// Complaint files a complaint against a user.
type Complaint struct {
	TargetUniqueID string `json:"target_unique_id"`
	Reason         string `json:"reason"`
}

// ScreenShare declares whether the client's video track is a screen share
// (relayed to channel members as a screenshare_changed event).
type ScreenShare struct {
	Active bool `json:"active"`
}

// PermissionsQuery requests the caller's resolved permission set.
type PermissionsQuery struct{}

// PermissionEntry describes one resolved permission.
type PermissionEntry struct {
	Key    string `json:"key"`
	Value  int    `json:"value"`
	Grant  int    `json:"grant"`
	Skip   bool   `json:"skip,omitempty"`
	Negate bool   `json:"negate,omitempty"`
}

// PermissionsResponse carries the caller's resolved permission set (one
// entry per key present in any tier, with the winning tier's value).
type PermissionsResponse struct {
	Entries []PermissionEntry `json:"entries"`
}

// ClientInfoQuery requests the connection info of an online client.
type ClientInfoQuery struct {
	ClientID string `json:"client_id"`
}

// ClientInfoResponse is the connection info of an online client (TS3-style
// Client Info dialog). PingMs is -1 when unknown (the client never answered
// a server Ping). IP and Port are empty/0 unless the requester is the
// target itself, an admin, or holds b_client_remoteaddress_view.
type ClientInfoResponse struct {
	ClientID    string `json:"client_id"`
	UniqueID    string `json:"unique_id"`
	Nickname    string `json:"nickname"`
	ChannelID   int64  `json:"channel_id"`
	ConnectedAt int64  `json:"connected_at"` // unix seconds
	IdleSeconds int64  `json:"idle_seconds"`
	PingMs      int64  `json:"ping_ms"`
	IP          string `json:"ip,omitempty"`
	Port        int    `json:"port,omitempty"`
	BytesIn     int64  `json:"bytes_in"`
	BytesOut    int64  `json:"bytes_out"`
}

// Encode marshals a message into a Frame with the given type.
func Encode(mt MessageType, msg any) (*Frame, error) {
	payload, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("netproto: encoding %s: %w", mt, err)
	}
	return &Frame{Type: uint16(mt), Payload: payload}, nil
}

// Decode unmarshals a Frame's payload into the provided message struct. The
// caller is responsible for selecting the right struct based on f.Type.
func Decode(f *Frame, msg any) error {
	if f == nil {
		return fmt.Errorf("netproto: nil frame")
	}
	if len(f.Payload) == 0 {
		// Allow empty payloads (e.g. Ping) to decode into zero-value structs.
		return nil
	}
	if err := json.Unmarshal(f.Payload, msg); err != nil {
		return fmt.Errorf("netproto: decoding %s: %w", MessageType(f.Type), err)
	}
	return nil
}

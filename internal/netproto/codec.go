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

	MsgChannelEdit     MessageType = 40 // client -> server: edit channel settings (topic, opus, ...)
	MsgPrioritySpeaker MessageType = 41 // client -> server: toggle own priority-speaker flag

	MsgKeyPublish  MessageType = 42 // client -> server: publish own X25519 public key (E2EE directory)
	MsgKeyRequest  MessageType = 43 // client -> server: request a user's X25519 public key
	MsgKeyResponse MessageType = 44 // server -> client: a user's X25519 public key (empty = none)
	MsgChannelKey  MessageType = 45 // server -> client: sealed channel/global chat key (key rotation)

	MsgChatHistory         MessageType = 46 // client -> server: request paged chat history
	MsgChatHistoryResponse MessageType = 47 // server -> client: chat history page
	MsgChatEdit            MessageType = 48 // client -> server: edit own message
	MsgChatDelete          MessageType = 49 // client -> server: delete message (own or b_chat_delete_any)
	MsgChatPin             MessageType = 50 // client -> server: pin/unpin a message
	MsgChatPins            MessageType = 51 // client -> server: list channel pins
	MsgChatPinsResponse    MessageType = 52 // server -> client: channel pins
	MsgTyping              MessageType = 53 // both directions: typing indicator (relayed, not stored)
	MsgChatDelivered       MessageType = 54 // client -> server: DM delivery ack (relayed to sender)
	MsgChatRead            MessageType = 55 // client -> server: DM read receipt (relayed to sender)
	MsgEmojiUpload         MessageType = 56 // client -> server: upload custom emoji
	MsgEmojiList           MessageType = 57 // client -> server: list custom emojis
	MsgEmojiListResponse   MessageType = 58 // server -> client: custom emoji list
	MsgChatReact           MessageType = 59 // client -> server: toggle a reaction on a message
	MsgEmojiGet            MessageType = 60 // client -> server: fetch one custom emoji image
	MsgEmojiData           MessageType = 61 // server -> client: custom emoji image data

	MsgGroupList         MessageType = 62 // client -> server: list groups (server|channel)
	MsgGroupListResponse MessageType = 63 // server -> client: group list
	MsgGroupCreate       MessageType = 64 // client -> server: create a group
	MsgGroupRename       MessageType = 65 // client -> server: rename a group
	MsgGroupDelete       MessageType = 66 // client -> server: delete a group (force for non-empty)
	MsgGroupAssign       MessageType = 67 // client -> server: assign a user to a group
	MsgGroupUnassign     MessageType = 68 // client -> server: remove a user from a group
	MsgPermSet           MessageType = 69 // client -> server: set a permission entry
	MsgPermUnset         MessageType = 70 // client -> server: remove a permission entry
	MsgPermTemplateApply MessageType = 71 // client -> server: apply a permission template
	MsgPermTrace         MessageType = 72 // client -> server: trace a permission's winning tier
	MsgPermTraceResponse MessageType = 73 // server -> client: permission trace
	MsgAuditLog          MessageType = 74 // client -> server: request audit log page
	MsgAuditLogResponse  MessageType = 75 // server -> client: audit log page
	MsgGroupIconSet      MessageType = 76 // client -> server: set a server-group icon

	MsgGroupIconGet         MessageType = 77 // client -> server: fetch a server-group icon
	MsgGroupIconData        MessageType = 78 // server -> client: group icon payload
	MsgGroupMembers         MessageType = 79 // client -> server: list a group's members
	MsgGroupMembersResponse MessageType = 80 // server -> client: group member list
	MsgBanList              MessageType = 81 // client -> server: list bans
	MsgBanListResponse      MessageType = 82 // server -> client: ban list
	MsgBanRemove            MessageType = 83 // client -> server: lift a ban
	MsgPermList             MessageType = 84 // client -> server: list a target's permission entries
	MsgPermListResponse     MessageType = 85 // server -> client: permission entries

	MsgFileDelete           MessageType = 86 // client -> server: delete a channel file
	MsgFileRename           MessageType = 87 // client -> server: rename/move a channel file
	MsgFileVersions         MessageType = 88 // client -> server: list a file's old versions
	MsgFileVersionsResponse MessageType = 89 // server -> client: version list
	MsgFileLink             MessageType = 90 // client -> server: create an expiring download link
	MsgFileLinkResponse     MessageType = 91 // server -> client: the link URL + expiry
	MsgServerIconSet        MessageType = 92 // client -> server: upload the server icon (admin)
	MsgServerIconGet        MessageType = 93 // client -> server: fetch the server icon
	MsgServerIconData       MessageType = 94 // server -> client: server icon payload

	MsgSetStatus          MessageType = 95 // client -> server: set own presence status
	MsgPoke               MessageType = 96 // client -> server: poke a client
	MsgServerInfoQuery    MessageType = 97 // client -> server: request public server info
	MsgServerInfoResponse MessageType = 98 // server -> client: public server info
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
	case MsgChannelEdit:
		return "ChannelEdit"
	case MsgPrioritySpeaker:
		return "PrioritySpeaker"
	case MsgKeyPublish:
		return "KeyPublish"
	case MsgKeyRequest:
		return "KeyRequest"
	case MsgKeyResponse:
		return "KeyResponse"
	case MsgChannelKey:
		return "ChannelKey"
	case MsgChatHistory:
		return "ChatHistory"
	case MsgChatHistoryResponse:
		return "ChatHistoryResponse"
	case MsgChatEdit:
		return "ChatEdit"
	case MsgChatDelete:
		return "ChatDelete"
	case MsgChatPin:
		return "ChatPin"
	case MsgChatPins:
		return "ChatPins"
	case MsgChatPinsResponse:
		return "ChatPinsResponse"
	case MsgTyping:
		return "Typing"
	case MsgChatDelivered:
		return "ChatDelivered"
	case MsgChatRead:
		return "ChatRead"
	case MsgEmojiUpload:
		return "EmojiUpload"
	case MsgEmojiList:
		return "EmojiList"
	case MsgEmojiListResponse:
		return "EmojiListResponse"
	case MsgChatReact:
		return "ChatReact"
	case MsgEmojiGet:
		return "EmojiGet"
	case MsgEmojiData:
		return "EmojiData"
	case MsgGroupList:
		return "GroupList"
	case MsgGroupListResponse:
		return "GroupListResponse"
	case MsgGroupCreate:
		return "GroupCreate"
	case MsgGroupRename:
		return "GroupRename"
	case MsgGroupDelete:
		return "GroupDelete"
	case MsgGroupAssign:
		return "GroupAssign"
	case MsgGroupUnassign:
		return "GroupUnassign"
	case MsgPermSet:
		return "PermSet"
	case MsgPermUnset:
		return "PermUnset"
	case MsgPermTemplateApply:
		return "PermTemplateApply"
	case MsgPermTrace:
		return "PermTrace"
	case MsgPermTraceResponse:
		return "PermTraceResponse"
	case MsgAuditLog:
		return "AuditLog"
	case MsgAuditLogResponse:
		return "AuditLogResponse"
	case MsgGroupIconSet:
		return "GroupIconSet"
	case MsgGroupIconGet:
		return "GroupIconGet"
	case MsgGroupIconData:
		return "GroupIconData"
	case MsgGroupMembers:
		return "GroupMembers"
	case MsgGroupMembersResponse:
		return "GroupMembersResponse"
	case MsgBanList:
		return "BanList"
	case MsgBanListResponse:
		return "BanListResponse"
	case MsgBanRemove:
		return "BanRemove"
	case MsgPermList:
		return "PermList"
	case MsgPermListResponse:
		return "PermListResponse"
	case MsgFileDelete:
		return "FileDelete"
	case MsgFileRename:
		return "FileRename"
	case MsgFileVersions:
		return "FileVersions"
	case MsgFileVersionsResponse:
		return "FileVersionsResponse"
	case MsgFileLink:
		return "FileLink"
	case MsgFileLinkResponse:
		return "FileLinkResponse"
	case MsgServerIconSet:
		return "ServerIconSet"
	case MsgServerIconGet:
		return "ServerIconGet"
	case MsgServerIconData:
		return "ServerIconData"
	case MsgSetStatus:
		return "SetStatus"
	case MsgPoke:
		return "Poke"
	case MsgServerInfoQuery:
		return "ServerInfoQuery"
	case MsgServerInfoResponse:
		return "ServerInfoResponse"
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
	// ICEServers carries the ICE servers the client should use for WebRTC
	// (STUN defaults plus TURN entries with time-limited credentials when the
	// server has TURN configured). Empty means "client defaults".
	ICEServers []ICEServer `json:"ice_servers,omitempty"`
	// TLSFingerprint is the SHA-256 fingerprint of the server's control
	// channel certificate (empty when TLS is disabled). Clients use it for
	// TOFU verification and display.
	TLSFingerprint string `json:"tls_fingerprint,omitempty"`
	// MOTD is the server's message of the day (empty when unset).
	MOTD string `json:"motd,omitempty"`
	// IsAdmin reports whether the authenticated user is a server admin
	// (users.is_admin). Clients use it to show/hide admin-only UI.
	IsAdmin bool `json:"is_admin,omitempty"`
}

// ICEServer describes one ICE server for a WebRTC RTCPeerConnection,
// mirroring the browser's RTCIceServer dictionary.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
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
// The Opus fields set the channel's audio quality (0 bitrate = default 32k).
type CreateChannel struct {
	Name            string `json:"name"`
	Topic           string `json:"topic,omitempty"`
	ParentID        int64  `json:"parent_id,omitempty"`
	Type            int    `json:"type"`
	MaxClients      int    `json:"max_clients,omitempty"`
	Password        string `json:"password,omitempty"`
	NeededJoinPower int    `json:"needed_join_power,omitempty"`
	OpusBitrate     int    `json:"opus_bitrate,omitempty"`
	OpusFEC         bool   `json:"opus_fec,omitempty"`
	OpusDTX         bool   `json:"opus_dtx,omitempty"`
	OpusStereo      bool   `json:"opus_stereo,omitempty"`
}

// ChannelEdit requests editing a channel's settings. Nil pointer fields are
// left unchanged. Gated by b_channel_modify.
type ChannelEdit struct {
	ChannelID       int64   `json:"channel_id"`
	Topic           *string `json:"topic,omitempty"`
	MaxClients      *int    `json:"max_clients,omitempty"`
	OpusBitrate     *int    `json:"opus_bitrate,omitempty"`
	OpusFEC         *bool   `json:"opus_fec,omitempty"`
	OpusDTX         *bool   `json:"opus_dtx,omitempty"`
	OpusStereo      *bool   `json:"opus_stereo,omitempty"`
	SlowModeSeconds *int    `json:"slow_mode_seconds,omitempty"`
	Description     *string `json:"description,omitempty"`
}

// PrioritySpeaker toggles the calling client's priority-speaker flag
// (TS3-style channel commander). Gated by b_client_priority_speaker.
type PrioritySpeaker struct {
	Active bool `json:"active"`
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
//
// Encryption (wave 4b): when Enc is true, Text is base64 ciphertext —
// nacl/box (nonce-prepended) for direct messages (KeyID 0), secretbox for
// channel/global (KeyID identifies the scope key). The server validates the
// KeyID against its current scope key but cannot read the body.
type ChatSend struct {
	ChannelID  string `json:"channel_id,omitempty"`
	ToClientID string `json:"to_client_id,omitempty"`
	ToUniqueID string `json:"to_unique_id,omitempty"`
	Text       string `json:"text"`
	Enc        bool   `json:"enc,omitempty"`
	KeyID      uint32 `json:"key_id,omitempty"`
	// ClientMsgID is a client-generated reference echoed in the broadcast so
	// DM delivery/read receipts (wave 5a) can reference the message without
	// a database id (DMs are true E2EE and not stored server-side).
	ClientMsgID string `json:"client_msg_id,omitempty"`
}

// ChatBroadcast is a chat message the server fans out to interested clients.
// Offline marks messages delivered from the offline-message spool after login.
// FromUniqueID identifies the sender for E2EE direct messages (the recipient
// fetches the sender's public key from the directory). Enc/KeyID mirror
// ChatSend; E2E marks true end-to-end (box) messages for UI display.
type ChatBroadcast struct {
	ChannelID    string `json:"channel_id,omitempty"`
	FromClientID string `json:"from_client_id,omitempty"`
	FromUniqueID string `json:"from_unique_id,omitempty"`
	From         string `json:"from"`
	Text         string `json:"text"`
	Offline      bool   `json:"offline,omitempty"`
	Enc          bool   `json:"enc,omitempty"`
	KeyID        uint32 `json:"key_id,omitempty"`
	E2E          bool   `json:"e2e,omitempty"`
	// ID is the server-side message id (channel/global history, wave 5a;
	// 0 for DMs). Mentions lists mentioned users' unique IDs. ClientMsgID
	// echoes the sender's reference for receipts.
	ID          int64    `json:"id,omitempty"`
	Mentions    []string `json:"mentions,omitempty"`
	ClientMsgID string   `json:"client_msg_id,omitempty"`
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
	// DurationSeconds > 0 makes the ban temporary (0 = permanent). Only
	// meaningful with Ban.
	DurationSeconds int64 `json:"duration_seconds,omitempty"`
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
	// Folder is the virtual folder within the channel ('' = root, wave 7).
	Folder string `json:"folder,omitempty"`
}

// FileTransferInitResponse carries an issued single-use transfer token and
// the port of the file-transfer server the client should connect to.
type FileTransferInitResponse struct {
	TransferID string `json:"transfer_id"`
	Token      string `json:"token"`
	Port       int    `json:"port"`
}

// FileList requests the file listing of a channel. Folder selects a virtual
// folder (” = root, wave 7); Path is the legacy alias ("" or "/" only).
type FileList struct {
	ChannelID int64  `json:"channel_id"`
	Path      string `json:"path,omitempty"`
	Folder    string `json:"folder,omitempty"`
}

// FileEntry describes one file in a FileListResponse.
type FileEntry struct {
	Name       string    `json:"name"`
	Folder     string    `json:"folder,omitempty"`
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"`
	Uploader   string    `json:"uploader,omitempty"`
	UploadedAt time.Time `json:"uploaded_at"`
}

// FileListResponse carries a channel's file listing plus the quota state
// (265): UsedBytes counts all files in the channel; QuotaBytes 0 = unlimited.
type FileListResponse struct {
	Entries    []FileEntry `json:"entries"`
	Folders    []string    `json:"folders,omitempty"`
	UsedBytes  int64       `json:"used_bytes"`
	QuotaBytes int64       `json:"quota_bytes"`
}

// FileDelete deletes one channel file (263). Allowed for the uploader and
// holders of b_ft_delete (admins bypass).
type FileDelete struct {
	ChannelID int64  `json:"channel_id"`
	Folder    string `json:"folder,omitempty"`
	Name      string `json:"name"`
}

// FileRename renames or moves a channel file (262); same gate as FileDelete.
type FileRename struct {
	ChannelID int64  `json:"channel_id"`
	Folder    string `json:"folder,omitempty"`
	Name      string `json:"name"`
	NewName   string `json:"new_name"`
	NewFolder string `json:"new_folder,omitempty"`
}

// FileVersions lists the rotated old versions of a file (264).
type FileVersions struct {
	ChannelID int64  `json:"channel_id"`
	Folder    string `json:"folder,omitempty"`
	Name      string `json:"name"`
}

// FileVersionsResponse carries the version list (newest first).
type FileVersionsResponse struct {
	Entries []FileEntry `json:"entries"`
}

// FileLink requests an expiring download link for a file (267). Gated by
// admin or uploader.
type FileLink struct {
	ChannelID int64  `json:"channel_id"`
	Folder    string `json:"folder,omitempty"`
	Name      string `json:"name"`
}

// FileLinkResponse carries the link path and expiry. The client builds the
// full URL from its own control host plus HealthPort (the server cannot
// know its published address behind Docker/NAT).
type FileLinkResponse struct {
	Path       string `json:"path"`
	HealthPort int    `json:"health_port"`
	ExpiresAt  int64  `json:"expires_at"`
}

// ServerIconSet uploads the server icon (admin only; same validation as
// avatars).
type ServerIconSet struct {
	DataBase64 string `json:"data_base64"`
}

// ServerIconGet fetches the server icon.
type ServerIconGet struct{}

// ServerIconData carries the server icon (empty = none set).
type ServerIconData struct {
	DataBase64  string `json:"data_base64,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

// SetStatus sets the caller's presence (307-309): Status is "online",
// "away", or "busy"; Message is a free-form status line ("" clears).
type SetStatus struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// Poke pokes a client (321/322): a short attention message relayed as a
// "poke" event. Gated by b_client_poke (or poke power) with a per-target
// cooldown.
type Poke struct {
	ClientID string `json:"client_id"`
	Message  string `json:"message,omitempty"`
}

// ServerInfoQuery requests the server's public information (313).
type ServerInfoQuery struct{}

// ServerInfoResponse carries the server's public information.
type ServerInfoResponse struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	UptimeSeconds  int64  `json:"uptime_seconds"`
	ClientsOnline  int    `json:"clients_online"`
	ChannelsOnline int    `json:"channels_online"`
	MaxClients     int    `json:"max_clients"`
	MOTD           string `json:"motd,omitempty"`
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
	// CopyFromChannelID, when non-zero (and DataBase64 is empty), reuses the
	// icon already stored for another channel (271 icon library).
	CopyFromChannelID int64 `json:"copy_from_channel_id,omitempty"`
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

// KeyPublish publishes the client's X25519 public key (base64, 32 bytes) to
// the server directory. Registered users persist it; guests keep it in
// server memory for their session.
type KeyPublish struct {
	PublicKey string `json:"public_key"`
}

// KeyRequest requests a user's X25519 public key by unique ID.
type KeyRequest struct {
	UniqueID string `json:"unique_id"`
}

// KeyResponse is the server's reply to KeyRequest. PublicKey is empty when
// the user has not published a key (old client).
type KeyResponse struct {
	UniqueID  string `json:"unique_id"`
	PublicKey string `json:"public_key,omitempty"`
}

// ChannelKey delivers a scope chat key to a member, sealed with nacl/box
// (SealAnonymous) to the member's X25519 public key. ChannelID 0 is the
// global (server-wide) scope. KeyID identifies the key generation — it bumps
// on rotation (member left), and members must use the latest KeyID when
// sending.
type ChannelKey struct {
	ChannelID int64  `json:"channel_id"`
	KeyID     uint32 `json:"key_id"`
	SealedKey string `json:"sealed_key"`
}

// --- Chat infrastructure (wave 5a) ------------------------------------------

// ChatHistory requests a page of channel/global history. ChannelID 0 is the
// global scope. BeforeID pages backwards (0 = latest).
type ChatHistory struct {
	ChannelID int64 `json:"channel_id"`
	BeforeID  int64 `json:"before_id,omitempty"`
	Limit     int   `json:"limit,omitempty"`
}

// ChatHistoryEntry is one stored message in a history page. Body is empty
// for tombstoned (deleted) messages. Reactions maps emoji -> count.
type ChatHistoryEntry struct {
	ID           int64          `json:"id"`
	FromUniqueID string         `json:"from_unique_id"`
	FromNickname string         `json:"from_nickname"`
	Body         string         `json:"body"`
	SentAt       int64          `json:"sent_at"` // unix seconds
	EditedAt     int64          `json:"edited_at,omitempty"`
	Deleted      bool           `json:"deleted,omitempty"`
	Reactions    map[string]int `json:"reactions,omitempty"`
}

// ChatHistoryResponse carries one history page (newest first).
type ChatHistoryResponse struct {
	ChannelID int64              `json:"channel_id"`
	Messages  []ChatHistoryEntry `json:"messages"`
}

// ChatEdit edits the caller's own message. NewText is encrypted like a
// normal message (the server decrypts before storing).
type ChatEdit struct {
	MessageID int64  `json:"message_id"`
	NewText   string `json:"new_text"`
	Enc       bool   `json:"enc,omitempty"`
	KeyID     uint32 `json:"key_id,omitempty"`
}

// ChatDelete deletes a message (own, or any with b_chat_delete_any).
type ChatDelete struct {
	MessageID int64 `json:"message_id"`
}

// ChatPin pins or unpins a message in a channel.
type ChatPin struct {
	ChannelID int64 `json:"channel_id"`
	MessageID int64 `json:"message_id"`
	Pinned    bool  `json:"pinned"`
}

// ChatPins requests a channel's pins.
type ChatPins struct {
	ChannelID int64 `json:"channel_id"`
}

// ChatPinEntry describes one pinned message.
type ChatPinEntry struct {
	MessageID int64             `json:"message_id"`
	PinnedBy  string            `json:"pinned_by"`
	PinnedAt  int64             `json:"pinned_at"`
	Message   *ChatHistoryEntry `json:"message,omitempty"`
}

// ChatPinsResponse carries a channel's pins.
type ChatPinsResponse struct {
	ChannelID int64          `json:"channel_id"`
	Pins      []ChatPinEntry `json:"pins"`
}

// Typing is a typing indicator. Exactly one of ChannelID / ToUniqueID is
// set (channel scope or DM); neither set means global. Relayed, not stored.
type Typing struct {
	ChannelID  int64  `json:"channel_id,omitempty"`
	ToUniqueID string `json:"to_unique_id,omitempty"`
}

// ChatDelivered is the recipient's ack that a DM arrived (client-side ref).
type ChatDelivered struct {
	ToUniqueID  string `json:"to_unique_id"` // the DM sender to notify
	ClientMsgID string `json:"client_msg_id"`
}

// ChatRead is the recipient's read receipt for a DM.
type ChatRead struct {
	ToUniqueID  string `json:"to_unique_id"` // the DM sender to notify
	ClientMsgID string `json:"client_msg_id"`
}

// EmojiUpload uploads a custom emoji image (png/gif/webp, max 256 KiB after
// decoding). Gated by b_emoji_manage.
type EmojiUpload struct {
	Name       string `json:"name"`
	DataBase64 string `json:"data_base64"`
}

// EmojiList requests the custom emoji list.
type EmojiList struct{}

// EmojiEntry describes one custom emoji.
type EmojiEntry struct {
	Name     string `json:"name"`
	FileName string `json:"file_name"`
}

// EmojiListResponse carries the custom emoji list.
type EmojiListResponse struct {
	Emojis []EmojiEntry `json:"emojis"`
}

// ChatReact toggles a reaction on a message (add if absent, remove if
// present; one reaction per emoji per user).
type ChatReact struct {
	MessageID int64  `json:"message_id"`
	Emoji     string `json:"emoji"`
}

// EmojiGet requests a custom emoji's image data by name (96; the files live
// on the server, so clients fetch them over the control channel).
type EmojiGet struct {
	Name string `json:"name"`
}

// EmojiData is the server's reply to EmojiGet.
type EmojiData struct {
	Name        string `json:"name"`
	DataBase64  string `json:"data_base64"`
	ContentType string `json:"content_type"`
}

// --- Permission/group management (wave 6a) ----------------------------------

// GroupList requests the group list. Type is "server" or "channel".
type GroupList struct {
	Type string `json:"type"`
}

// GroupEntry describes one server/channel group.
type GroupEntry struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	SortID      int    `json:"sort_id"`
	MemberCount int    `json:"member_count"`
	Icon        string `json:"icon,omitempty"`
	Color       string `json:"color,omitempty"`
	Hoist       bool   `json:"hoist,omitempty"`
}

// GroupListResponse carries the group list.
type GroupListResponse struct {
	Type   string       `json:"type"`
	Groups []GroupEntry `json:"groups"`
}

// GroupCreate creates a group. Type is "server" or "channel".
type GroupCreate struct {
	Type   string `json:"type"`
	Name   string `json:"name"`
	SortID int    `json:"sort_id,omitempty"`
}

// GroupRename renames a group.
type GroupRename struct {
	Type    string `json:"type"`
	GroupID int64  `json:"group_id"`
	Name    string `json:"name"`
}

// GroupDelete deletes a group. Force is required when it has members.
type GroupDelete struct {
	Type    string `json:"type"`
	GroupID int64  `json:"group_id"`
	Force   bool   `json:"force,omitempty"`
}

// GroupAssign assigns a user to a group. ChannelID is required for channel
// groups. ExpiresInSeconds > 0 makes the membership expire (145).
type GroupAssign struct {
	Type             string `json:"type"`
	GroupID          int64  `json:"group_id"`
	UniqueID         string `json:"unique_id"`
	ChannelID        int64  `json:"channel_id,omitempty"`
	ExpiresInSeconds int64  `json:"expires_in_seconds,omitempty"`
}

// GroupUnassign removes a user from a group.
type GroupUnassign struct {
	Type      string `json:"type"`
	GroupID   int64  `json:"group_id"`
	UniqueID  string `json:"unique_id"`
	ChannelID int64  `json:"channel_id,omitempty"`
}

// PermSet writes a permission entry. Tier is server_group|client|
// channel_client|channel|channel_group. Targeting: GroupID for group tiers,
// UniqueID for client tiers, ChannelID for channel/channel_client tiers.
type PermSet struct {
	Tier      string `json:"tier"`
	GroupID   int64  `json:"group_id,omitempty"`
	UniqueID  string `json:"unique_id,omitempty"`
	ChannelID int64  `json:"channel_id,omitempty"`
	Key       string `json:"key"`
	Value     int    `json:"value"`
	Grant     int    `json:"grant,omitempty"`
	Skip      bool   `json:"skip,omitempty"`
	Negate    bool   `json:"negate,omitempty"`
}

// PermUnset removes a permission entry (same addressing as PermSet).
type PermUnset struct {
	Tier      string `json:"tier"`
	GroupID   int64  `json:"group_id,omitempty"`
	UniqueID  string `json:"unique_id,omitempty"`
	ChannelID int64  `json:"channel_id,omitempty"`
	Key       string `json:"key"`
}

// PermList requests the current permission entries of a target (same
// addressing as PermSet; wave 6b editor read path).
type PermList struct {
	Tier      string `json:"tier"`
	GroupID   int64  `json:"group_id,omitempty"`
	UniqueID  string `json:"unique_id,omitempty"`
	ChannelID int64  `json:"channel_id,omitempty"`
}

// PermListResponse carries a target's current permission entries.
type PermListResponse struct {
	Tier    string            `json:"tier"`
	Entries []PermissionEntry `json:"entries"`
}

// PermTemplateApply applies a built-in permission template to a target.
type PermTemplateApply struct {
	Template string `json:"template"` // guest|member|moderator|admin
	Tier     string `json:"tier"`
	GroupID  int64  `json:"group_id,omitempty"`
	UniqueID string `json:"unique_id,omitempty"`
}

// PermTrace requests the winning-tier trace for a permission.
type PermTrace struct {
	UniqueID  string `json:"unique_id"`
	Key       string `json:"key"`
	ChannelID int64  `json:"channel_id,omitempty"`
}

// PermTraceEntry describes one tier's contribution to a trace.
type PermTraceEntry struct {
	Tier    string `json:"tier"`
	Present bool   `json:"present"`
	Value   int    `json:"value"`
	Grant   int    `json:"grant"`
	Skip    bool   `json:"skip"`
	Negate  bool   `json:"negate"`
	Winning bool   `json:"winning,omitempty"`
}

// PermTraceResponse carries the trace.
type PermTraceResponse struct {
	Key           string           `json:"key"`
	Effective     int              `json:"effective"`
	EffectiveTier string           `json:"effective_tier"`
	Entries       []PermTraceEntry `json:"entries"`
}

// AuditLog requests an audit page (before_id = 0 for latest).
type AuditLog struct {
	BeforeID int64 `json:"before_id,omitempty"`
	Limit    int   `json:"limit,omitempty"`
}

// AuditEntry is one audit log row on the wire.
type AuditEntry struct {
	ID        int64  `json:"id"`
	Actor     string `json:"actor"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	Detail    string `json:"detail"`
	CreatedAt int64  `json:"created_at"`
}

// AuditLogResponse carries an audit page (newest first).
type AuditLogResponse struct {
	Entries []AuditEntry `json:"entries"`
}

// GroupIconSet uploads a server-group icon (same validation as avatars).
type GroupIconSet struct {
	GroupID    int64  `json:"group_id"`
	DataBase64 string `json:"data_base64"`
}

// GroupIconGet fetches a server-group icon (wave 6b; mirrors AvatarGet).
type GroupIconGet struct {
	GroupID int64 `json:"group_id"`
}

// GroupIconData carries a group icon (empty data_base64 = no icon set).
type GroupIconData struct {
	GroupID     int64  `json:"group_id"`
	DataBase64  string `json:"data_base64,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

// GroupMembers requests the member list of a group. ChannelID is required
// for channel groups (membership is channel-scoped).
type GroupMembers struct {
	Type      string `json:"type"`
	GroupID   int64  `json:"group_id"`
	ChannelID int64  `json:"channel_id,omitempty"`
}

// GroupMemberEntry describes one group member.
type GroupMemberEntry struct {
	UniqueID  string `json:"unique_id"`
	Nickname  string `json:"nickname,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"` // unix; 0 = permanent
}

// GroupMembersResponse carries the member list.
type GroupMembersResponse struct {
	Type    string             `json:"type"`
	GroupID int64              `json:"group_id"`
	Members []GroupMemberEntry `json:"members"`
}

// BanList requests the ban list (gated by ban power / admin).
type BanList struct{}

// BanEntry describes one ban.
type BanEntry struct {
	ID        int64  `json:"id"`
	Type      int    `json:"type"` // 0=IP, 1=unique_id, 2=nickname
	Value     string `json:"value"`
	Reason    string `json:"reason,omitempty"`
	BannedBy  string `json:"banned_by,omitempty"` // unique ID, when known
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at,omitempty"` // unix; 0 = permanent
}

// BanListResponse carries the ban list (newest first).
type BanListResponse struct {
	Bans []BanEntry `json:"bans"`
}

// BanRemove lifts one ban by ID.
type BanRemove struct {
	BanID int64 `json:"ban_id"`
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

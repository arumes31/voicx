package netproto

import (
	"testing"
)

// TestMessageTypeString covers the String names of all defined message types.
func TestMessageTypeString(t *testing.T) {
	cases := []struct {
		mt   MessageType
		want string
	}{
		{MsgAuthenticate, "Authenticate"},
		{MsgAuthResponse, "AuthResponse"},
		{MsgCreateChannel, "CreateChannel"},
		{MsgChannelList, "ChannelList"},
		{MsgChatSend, "ChatSend"},
		{MsgChatBroadcast, "ChatBroadcast"},
		{MsgError, "Error"},
		{MsgPing, "Ping"},
		{MsgPong, "Pong"},
		{MsgJoinChannel, "JoinChannel"},
		{MsgDeleteChannel, "DeleteChannel"},
		{MsgMoveClient, "MoveClient"},
		{MsgKickClient, "KickClient"},
		{MsgSnapshot, "Snapshot"},
		{MsgEvent, "Event"},
		{MsgAuthChallenge, "AuthChallenge"},
		{MsgAuthSignature, "AuthSignature"},
		{MsgWebRTCOffer, "WebRTCOffer"},
		{MsgWebRTCAnswer, "WebRTCAnswer"},
		{MsgICECandidate, "ICECandidate"},
		{MsgWhisperSet, "WhisperSet"},
		{MsgPositionUpdate, "PositionUpdate"},
		{MsgVideoQuality, "VideoQuality"},
		{MsgRecordingControl, "RecordingControl"},
		{MsgFileTransferInit, "FileTransferInit"},
		{MsgFileTransferInitResponse, "FileTransferInitResponse"},
		{MsgFileList, "FileList"},
		{MsgFileListResponse, "FileListResponse"},
		{MsgAvatarSet, "AvatarSet"},
		{MsgAvatarGet, "AvatarGet"},
		{MsgAvatarData, "AvatarData"},
		{MsgChannelIconSet, "ChannelIconSet"},
		{MsgTokenUse, "TokenUse"},
		{MsgComplaint, "Complaint"},
		{MsgScreenShare, "ScreenShare"},
		{MsgClientInfoQuery, "ClientInfoQuery"},
		{MsgClientInfoResponse, "ClientInfoResponse"},
		{MessageType(999), "Unknown(999)"},
	}
	for _, tc := range cases {
		if got := tc.mt.String(); got != tc.want {
			t.Errorf("MessageType(%d).String() = %q, want %q", uint16(tc.mt), got, tc.want)
		}
	}
}

// TestCodecRoundTrip verifies that every message struct survives an
// Encode/Decode round-trip with all fields intact.
func TestCodecRoundTrip(t *testing.T) {
	t.Run("Authenticate", func(t *testing.T) {
		in := Authenticate{Username: "uid-abc", Password: "secret", ServerPassword: "spw", Nickname: "guesty", Anonymous: true, Token: "tok"}
		var out Authenticate
		roundTrip(t, MsgAuthenticate, in, &out)
		if out != in {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("AuthResponse", func(t *testing.T) {
		in := AuthResponse{OK: true, ClientID: "c-1", UniqueID: "uid-abc", Nickname: "dan", Reason: "x"}
		var out AuthResponse
		roundTrip(t, MsgAuthResponse, in, &out)
		if out.OK != in.OK || out.ClientID != in.ClientID || out.UniqueID != in.UniqueID ||
			out.Nickname != in.Nickname || out.Reason != in.Reason || len(out.ICEServers) != 0 {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("AuthResponseWithICEServers", func(t *testing.T) {
		in := AuthResponse{OK: true, ClientID: "c-1", ICEServers: []ICEServer{
			{URLs: []string{"stun:stun.example.com:3478"}},
			{URLs: []string{"turn:turn.example.com:3478"}, Username: "123:uid", Credential: "cred"},
		}}
		var out AuthResponse
		roundTrip(t, MsgAuthResponse, in, &out)
		if len(out.ICEServers) != 2 ||
			out.ICEServers[0].URLs[0] != "stun:stun.example.com:3478" ||
			out.ICEServers[1].Username != "123:uid" || out.ICEServers[1].Credential != "cred" {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("CreateChannel", func(t *testing.T) {
		in := CreateChannel{Name: "Lobby", Topic: "t", ParentID: 3, Type: 2, MaxClients: 8, Password: "pw", NeededJoinPower: 25}
		var out CreateChannel
		roundTrip(t, MsgCreateChannel, in, &out)
		if out != in {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("AuthChallenge", func(t *testing.T) {
		in := AuthChallenge{Challenge: []byte("0123456789abcdef0123456789abcdef")}
		var out AuthChallenge
		roundTrip(t, MsgAuthChallenge, in, &out)
		if string(out.Challenge) != string(in.Challenge) {
			t.Errorf("got %q, want %q", out.Challenge, in.Challenge)
		}
	})

	t.Run("AuthSignature", func(t *testing.T) {
		in := AuthSignature{UniqueID: "uid-abc", PublicKey: "-----BEGIN PUBLIC KEY-----", Signature: []byte("sig-bytes")}
		var out AuthSignature
		roundTrip(t, MsgAuthSignature, in, &out)
		if out.UniqueID != in.UniqueID || out.PublicKey != in.PublicKey || string(out.Signature) != string(in.Signature) {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("ChatSend direct", func(t *testing.T) {
		in := ChatSend{ToClientID: "c-9", ToUniqueID: "uid-9", Text: "hi"}
		var out ChatSend
		roundTrip(t, MsgChatSend, in, &out)
		if out != in {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("ChatBroadcast", func(t *testing.T) {
		in := ChatBroadcast{ChannelID: "7", FromClientID: "c-1", FromUniqueID: "uid-1", From: "dan", Text: "hello", Offline: true, ID: 42, Mentions: []string{"uid-2"}, ClientMsgID: "ref-1"}
		var out ChatBroadcast
		roundTrip(t, MsgChatBroadcast, in, &out)
		if out.ChannelID != in.ChannelID || out.FromClientID != in.FromClientID || out.FromUniqueID != in.FromUniqueID ||
			out.From != in.From || out.Text != in.Text || out.Offline != in.Offline || out.ID != in.ID ||
			len(out.Mentions) != 1 || out.Mentions[0] != "uid-2" || out.ClientMsgID != in.ClientMsgID {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("JoinChannel", func(t *testing.T) {
		in := JoinChannel{ChannelID: 42, Password: "pw"}
		var out JoinChannel
		roundTrip(t, MsgJoinChannel, in, &out)
		if out != in {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("DeleteChannel", func(t *testing.T) {
		in := DeleteChannel{ChannelID: 42}
		var out DeleteChannel
		roundTrip(t, MsgDeleteChannel, in, &out)
		if out != in {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("MoveClient", func(t *testing.T) {
		in := MoveClient{ClientID: "c-2", ChannelID: 42}
		var out MoveClient
		roundTrip(t, MsgMoveClient, in, &out)
		if out != in {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("KickClient", func(t *testing.T) {
		in := KickClient{ClientID: "c-2", FromServer: true, Ban: true, Reason: "spam"}
		var out KickClient
		roundTrip(t, MsgKickClient, in, &out)
		if out != in {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("WebRTCOffer", func(t *testing.T) {
		in := WebRTCOffer{SDP: "v=0\r\no=- 1 1 IN IP4 0.0.0.0"}
		var out WebRTCOffer
		roundTrip(t, MsgWebRTCOffer, in, &out)
		if out != in {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("WebRTCAnswer", func(t *testing.T) {
		in := WebRTCAnswer{SDP: "v=0\r\no=- 2 2 IN IP4 0.0.0.0"}
		var out WebRTCAnswer
		roundTrip(t, MsgWebRTCAnswer, in, &out)
		if out != in {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("ICECandidate", func(t *testing.T) {
		in := ICECandidate{Candidate: "candidate:1 1 udp 2130706431 127.0.0.1 9987 typ host", SDPMid: "0", SDPMLineIndex: 0}
		var out ICECandidate
		roundTrip(t, MsgICECandidate, in, &out)
		if out != in {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("WhisperSet", func(t *testing.T) {
		in := WhisperSet{UniqueIDs: []string{"u1", "u2"}, ChannelIDs: []int64{3, 4}, Active: true}
		var out WhisperSet
		roundTrip(t, MsgWhisperSet, in, &out)
		if out.Active != in.Active || len(out.UniqueIDs) != 2 || len(out.ChannelIDs) != 2 || out.ChannelIDs[1] != 4 {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("PositionUpdate", func(t *testing.T) {
		in := PositionUpdate{X: 1.5, Y: -2.25, Z: 3}
		var out PositionUpdate
		roundTrip(t, MsgPositionUpdate, in, &out)
		if out != in {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("VideoQuality", func(t *testing.T) {
		in := VideoQuality{Quality: "mid"}
		var out VideoQuality
		roundTrip(t, MsgVideoQuality, in, &out)
		if out != in {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("RecordingControl", func(t *testing.T) {
		in := RecordingControl{ChannelID: 7, Action: "start"}
		var out RecordingControl
		roundTrip(t, MsgRecordingControl, in, &out)
		if out != in {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("FileTransferInit", func(t *testing.T) {
		in := FileTransferInit{ChannelID: 7, Direction: "upload", Name: "a.txt", Size: 42}
		var out FileTransferInit
		roundTrip(t, MsgFileTransferInit, in, &out)
		if out != in {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("FileTransferInitResponse", func(t *testing.T) {
		in := FileTransferInitResponse{TransferID: "tid", Token: "tok", Port: 30033}
		var out FileTransferInitResponse
		roundTrip(t, MsgFileTransferInitResponse, in, &out)
		if out != in {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("FileListResponse", func(t *testing.T) {
		in := FileListResponse{Entries: []FileEntry{{Name: "a.txt", Size: 42, SHA256: "abc"}}}
		var out FileListResponse
		roundTrip(t, MsgFileListResponse, in, &out)
		if len(out.Entries) != 1 || out.Entries[0] != in.Entries[0] {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("AvatarSet", func(t *testing.T) {
		in := AvatarSet{DataBase64: "aGVsbG8="}
		var out AvatarSet
		roundTrip(t, MsgAvatarSet, in, &out)
		if out != in {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("AvatarData", func(t *testing.T) {
		in := AvatarData{UniqueID: "u1", DataBase64: "aGVsbG8=", ContentType: "image/png"}
		var out AvatarData
		roundTrip(t, MsgAvatarData, in, &out)
		if out != in {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("ChannelIconSet", func(t *testing.T) {
		in := ChannelIconSet{ChannelID: 7, DataBase64: "aGVsbG8="}
		var out ChannelIconSet
		roundTrip(t, MsgChannelIconSet, in, &out)
		if out != in {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("TokenUse", func(t *testing.T) {
		in := TokenUse{Token: "abc123"}
		var out TokenUse
		roundTrip(t, MsgTokenUse, in, &out)
		if out != in {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("Complaint", func(t *testing.T) {
		in := Complaint{TargetUniqueID: "u1", Reason: "spam"}
		var out Complaint
		roundTrip(t, MsgComplaint, in, &out)
		if out != in {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("ScreenShare", func(t *testing.T) {
		in := ScreenShare{Active: true}
		var out ScreenShare
		roundTrip(t, MsgScreenShare, in, &out)
		if out != in {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("ClientInfoQuery", func(t *testing.T) {
		in := ClientInfoQuery{ClientID: "c-1"}
		var out ClientInfoQuery
		roundTrip(t, MsgClientInfoQuery, in, &out)
		if out != in {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})

	t.Run("ClientInfoResponse", func(t *testing.T) {
		in := ClientInfoResponse{
			ClientID: "c-1", UniqueID: "uid", Nickname: "nick", ChannelID: 7,
			ConnectedAt: 1700000000, IdleSeconds: 42, PingMs: 62, IP: "1.2.3.4",
			Port: 5000, BytesIn: 1000, BytesOut: 2000,
		}
		var out ClientInfoResponse
		roundTrip(t, MsgClientInfoResponse, in, &out)
		if out != in {
			t.Errorf("got %+v, want %+v", out, in)
		}
	})
}

// roundTrip encodes in into a frame of type mt and decodes it back into out.
func roundTrip(t *testing.T, mt MessageType, in any, out any) {
	t.Helper()
	f, err := Encode(mt, in)
	if err != nil {
		t.Fatalf("Encode(%s): %v", mt, err)
	}
	if MessageType(f.Type) != mt {
		t.Fatalf("frame type = %d, want %d", f.Type, mt)
	}
	if err := Decode(f, out); err != nil {
		t.Fatalf("Decode(%s): %v", mt, err)
	}
}

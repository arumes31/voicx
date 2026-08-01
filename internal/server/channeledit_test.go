// channeledit_test.go covers the ChannelEdit and PrioritySpeaker control
// messages: permission gates, state updates, and broadcast events.
package server

import (
	"encoding/json"
	"testing"

	"voicx/internal/netproto"
	"voicx/internal/permissions"
	"voicx/internal/state"
)

// TestChannelEditDenied verifies b_channel_modify is enforced (deny on
// unset).
func TestChannelEditDenied(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	env.state.AddChannel(&state.Channel{ChannelID: 1, Name: "Lobby", ChannelType: 2})

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	topic := "new topic"
	send(t, conn, netproto.MsgChannelEdit, netproto.ChannelEdit{ChannelID: 1, Topic: &topic})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("error code = %d, want %d (permission denied)", e.Code, errCodePermissionDenied)
	}
}

// TestChannelEditOK verifies an edit persists to state and broadcasts a
// channel_updated event carrying the new values.
func TestChannelEditOK(t *testing.T) {
	perms := tieredWith(boolPerm(permissions.PermissionKeyChannelModify, true))
	env := startTestEnv(t, &perms)
	defer env.stop()

	env.state.AddChannel(&state.Channel{ChannelID: 1, Name: "Lobby", ChannelType: 2})

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	topic := "music room"
	bitrate := 128000
	stereo := true
	send(t, conn, netproto.MsgChannelEdit, netproto.ChannelEdit{
		ChannelID:   1,
		Topic:       &topic,
		OpusBitrate: &bitrate,
		OpusStereo:  &stereo,
	})

	data := readEventOfType(t, conn, "channel_updated")
	var ev struct {
		ChannelID   int64  `json:"channel_id"`
		Topic       string `json:"topic"`
		OpusBitrate int    `json:"opus_bitrate"`
		OpusStereo  bool   `json:"opus_stereo"`
		OpusFEC     bool   `json:"opus_fec"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("decode channel_updated: %v", err)
	}
	if ev.ChannelID != 1 || ev.Topic != "music room" || ev.OpusBitrate != 128000 || !ev.OpusStereo || ev.OpusFEC {
		t.Fatalf("channel_updated = %+v, want topic/music values", ev)
	}

	ch, ok := env.state.GetChannel(1)
	if !ok {
		t.Fatal("channel missing from state")
	}
	if ch.Topic != "music room" || ch.OpusBitrate != 128000 || !ch.OpusStereo || ch.OpusFEC {
		t.Fatalf("state channel = %+v, want edited values", ch)
	}
}

// TestChannelEditUnknown verifies editing a missing channel returns
// not-found.
func TestChannelEditUnknown(t *testing.T) {
	perms := tieredWith(boolPerm(permissions.PermissionKeyChannelModify, true))
	env := startTestEnv(t, &perms)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	topic := "x"
	send(t, conn, netproto.MsgChannelEdit, netproto.ChannelEdit{ChannelID: 999, Topic: &topic})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodeNotFound {
		t.Fatalf("error code = %d, want %d (not found)", e.Code, errCodeNotFound)
	}
}

// TestPrioritySpeakerDenied verifies b_client_priority_speaker is enforced
// (deny on unset).
func TestPrioritySpeakerDenied(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgPrioritySpeaker, netproto.PrioritySpeaker{Active: true})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("error code = %d, want %d (permission denied)", e.Code, errCodePermissionDenied)
	}
	if sc, _ := env.state.GetClient(""); sc != nil {
		t.Fatal("no client should be flagged")
	}
}

// TestPrioritySpeakerOK verifies the flag is stored in state and broadcast as
// priority_speaker_changed.
func TestPrioritySpeakerOK(t *testing.T) {
	perms := tieredWith(boolPerm(permissions.PermissionKeyClientPrioritySpeaker, true))
	env := startTestEnv(t, &perms)
	defer env.stop()

	conn, clientID := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgPrioritySpeaker, netproto.PrioritySpeaker{Active: true})
	data := readEventOfType(t, conn, "priority_speaker_changed")
	var ev struct {
		ClientID string `json:"client_id"`
		Active   bool   `json:"active"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("decode priority_speaker_changed: %v", err)
	}
	if ev.ClientID != clientID || !ev.Active {
		t.Fatalf("priority_speaker_changed = %+v, want client %s active", ev, clientID)
	}

	sc, ok := env.state.GetClient(clientID)
	if !ok || !sc.PrioritySpeaker {
		t.Fatalf("state PrioritySpeaker = %v, want true", ok && sc.PrioritySpeaker)
	}

	// Toggling off clears the flag.
	send(t, conn, netproto.MsgPrioritySpeaker, netproto.PrioritySpeaker{Active: false})
	readEventOfType(t, conn, "priority_speaker_changed")
	if sc, _ := env.state.GetClient(clientID); sc == nil || sc.PrioritySpeaker {
		t.Fatal("state PrioritySpeaker still true after toggle off")
	}
}

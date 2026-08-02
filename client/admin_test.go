package main

import (
	"testing"

	"voicx/internal/netproto"
)

func TestAdministrativeBindings(t *testing.T) {
	frames := make(chan *netproto.Frame, 8)
	app, _ := newPipedApp(t, func(frame *netproto.Frame) (netproto.MessageType, any, bool) {
		frames <- frame
		switch netproto.MessageType(frame.Type) {
		case netproto.MsgComplaintList, netproto.MsgComplaintClear:
			return netproto.MsgComplaints, netproto.Complaints{
				Entries: []netproto.ComplaintEntry{{TargetUniqueID: "target", FromUniqueID: "sender", Reason: "spam"}},
			}, true
		case netproto.MsgTokenList, netproto.MsgTokenAdd, netproto.MsgTokenDelete:
			return netproto.MsgTokens, netproto.Tokens{
				Entries: []netproto.TokenEntry{{Token: "key", GroupID: 7, Description: "moderator"}},
			}, true
		default:
			return 0, nil, false
		}
	})

	complaints, err := app.ComplaintList()
	if err != nil || len(complaints.Entries) != 1 || complaints.Entries[0].Reason != "spam" {
		t.Fatalf("ComplaintList = %+v, %v", complaints, err)
	}
	nextFrame(t, frames, netproto.MsgComplaintList)

	complaints, err = app.ComplaintClear("target", "sender")
	if err != nil || len(complaints.Entries) != 1 {
		t.Fatalf("ComplaintClear = %+v, %v", complaints, err)
	}
	var clear netproto.ComplaintClear
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgComplaintClear), &clear); err != nil ||
		clear.TargetUniqueID != "target" || clear.FromUniqueID != "sender" {
		t.Fatalf("ComplaintClear payload = %+v, %v", clear, err)
	}

	tokens, err := app.TokenList()
	if err != nil || len(tokens.Entries) != 1 || tokens.Entries[0].Token != "key" {
		t.Fatalf("TokenList = %+v, %v", tokens, err)
	}
	nextFrame(t, frames, netproto.MsgTokenList)

	tokens, err = app.TokenAdd(7, 9, "moderator")
	if err != nil || len(tokens.Entries) != 1 {
		t.Fatalf("TokenAdd = %+v, %v", tokens, err)
	}
	var add netproto.TokenAdd
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgTokenAdd), &add); err != nil ||
		add.GroupID != 7 || add.ChannelID != 9 || add.Description != "moderator" {
		t.Fatalf("TokenAdd payload = %+v, %v", add, err)
	}

	tokens, err = app.TokenDelete("key")
	if err != nil || len(tokens.Entries) != 1 {
		t.Fatalf("TokenDelete = %+v, %v", tokens, err)
	}
	var deleteToken netproto.TokenDelete
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgTokenDelete), &deleteToken); err != nil || deleteToken.Token != "key" {
		t.Fatalf("TokenDelete payload = %+v, %v", deleteToken, err)
	}

	if got := app.TokenUse("key"); got != "" {
		t.Fatalf("TokenUse = %q", got)
	}
	var use netproto.TokenUse
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgTokenUse), &use); err != nil || use.Token != "key" {
		t.Fatalf("TokenUse payload = %+v, %v", use, err)
	}

	app.cmLoad().disconnect()
	if _, err := app.ComplaintList(); err == nil {
		t.Fatal("ComplaintList succeeded after disconnect")
	}
	if _, err := app.TokenList(); err == nil {
		t.Fatal("TokenList succeeded after disconnect")
	}
	if got := app.TokenUse("key"); got == "" {
		t.Fatal("TokenUse succeeded after disconnect")
	}
}

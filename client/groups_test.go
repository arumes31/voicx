package main

import (
	"testing"

	"voicx/internal/netproto"
)

func TestGroupAndPermissionBindings(t *testing.T) {
	frames := make(chan *netproto.Frame, 48)
	app, cm := newPipedApp(t, func(frame *netproto.Frame) (netproto.MessageType, any, bool) {
		frames <- frame
		switch netproto.MessageType(frame.Type) {
		case netproto.MsgGroupList, netproto.MsgGroupCreate, netproto.MsgGroupEdit:
			return netproto.MsgGroupListResponse, netproto.GroupListResponse{}, true
		case netproto.MsgGroupMembers:
			return netproto.MsgGroupMembersResponse, netproto.GroupMembersResponse{}, true
		case netproto.MsgGroupIconGet:
			return netproto.MsgGroupIconData, netproto.GroupIconData{GroupID: 7, DataBase64: "aWNvbg=="}, true
		case netproto.MsgPermList:
			return netproto.MsgPermListResponse, netproto.PermListResponse{}, true
		case netproto.MsgPermTrace:
			return netproto.MsgPermTraceResponse, netproto.PermTraceResponse{}, true
		case netproto.MsgAuditLog:
			return netproto.MsgAuditLogResponse, netproto.AuditLogResponse{}, true
		case netproto.MsgBanList:
			return netproto.MsgBanListResponse, netproto.BanListResponse{}, true
		default:
			return 0, nil, false
		}
	})

	cm.mu.Lock()
	cm.isAdmin = true
	cm.isGuest = true
	cm.mu.Unlock()
	if !app.IsAdmin() || !app.IsGuest() {
		t.Fatal("session role snapshots were not exposed")
	}
	if (&App{}).IsAdmin() || (&App{}).IsGuest() {
		t.Fatal("offline App reports an authenticated role")
	}

	if _, err := app.GroupList("server"); err != nil {
		t.Fatalf("GroupList: %v", err)
	}
	var groupList netproto.GroupList
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgGroupList), &groupList); err != nil || groupList.Type != "server" {
		t.Fatalf("GroupList payload = %+v, %v", groupList, err)
	}

	if _, err := app.GroupCreate("server", "Moderators", 3); err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	var create netproto.GroupCreate
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgGroupCreate), &create); err != nil ||
		create.Type != "server" || create.Name != "Moderators" || create.SortID != 3 {
		t.Fatalf("GroupCreate payload = %+v, %v", create, err)
	}

	if _, err := app.GroupEdit(7, "#123456", true, 4); err != nil {
		t.Fatalf("GroupEdit: %v", err)
	}
	var groupEdit netproto.GroupEdit
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgGroupEdit), &groupEdit); err != nil ||
		groupEdit.GroupID != 7 || groupEdit.Color == nil || *groupEdit.Color != "#123456" ||
		groupEdit.Hoist == nil || !*groupEdit.Hoist || groupEdit.SortID == nil || *groupEdit.SortID != 4 {
		t.Fatalf("GroupEdit payload = %+v, %v", groupEdit, err)
	}

	writes := []struct {
		name  string
		type_ netproto.MessageType
		call  func() string
	}{
		{name: "rename", type_: netproto.MsgGroupRename, call: func() string { return app.GroupRename("server", 7, "Ops") }},
		{name: "delete", type_: netproto.MsgGroupDelete, call: func() string { return app.GroupDelete("server", 7, true) }},
		{name: "assign", type_: netproto.MsgGroupAssign, call: func() string { return app.GroupAssign("channel", 7, "user", 9, 3600) }},
		{name: "unassign", type_: netproto.MsgGroupUnassign, call: func() string { return app.GroupUnassign("channel", 7, "user", 9) }},
		{name: "icon set", type_: netproto.MsgGroupIconSet, call: func() string { return app.GroupIconSet(7, "aWNvbg==") }},
		{name: "permission set", type_: netproto.MsgPermSet, call: func() string {
			return app.PermSet("servergroup", 7, "", 0, "i_client_talk_power", 42, 75, true, false)
		}},
		{name: "permission unset", type_: netproto.MsgPermUnset, call: func() string {
			return app.PermUnset("servergroup", 7, "", 0, "i_client_talk_power")
		}},
		{name: "template", type_: netproto.MsgPermTemplateApply, call: func() string {
			return app.PermTemplateApply("moderator", "servergroup", 7, "")
		}},
		{name: "copy", type_: netproto.MsgPermCopy, call: func() string {
			return app.PermCopy("servergroup", "7", "servergroup", "8", 0, true)
		}},
		{name: "ban remove", type_: netproto.MsgBanRemove, call: func() string { return app.BanRemove(12) }},
		{name: "kick", type_: netproto.MsgKickClient, call: func() string {
			return app.KickClient("c2", true, true, "reason", 60)
		}},
		{name: "create channel", type_: netproto.MsgCreateChannel, call: func() string {
			return app.CreateChannel("Room", "Topic", 1, 2, 20, "secret", 10, 64_000, true, false, true)
		}},
		{name: "delete channel", type_: netproto.MsgDeleteChannel, call: func() string { return app.DeleteChannel(11) }},
	}
	for _, tc := range writes {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.call(); got != "" {
				t.Fatalf("binding error = %q", got)
			}
			nextFrame(t, frames, tc.type_)
		})
	}

	if _, err := app.GroupMembers("channel", 7, 9); err != nil {
		t.Fatalf("GroupMembers: %v", err)
	}
	var members netproto.GroupMembers
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgGroupMembers), &members); err != nil ||
		members.Type != "channel" || members.GroupID != 7 || members.ChannelID != 9 {
		t.Fatalf("GroupMembers payload = %+v, %v", members, err)
	}

	icon, err := app.GroupIconGet(7)
	if err != nil || icon.GroupID != 7 || icon.DataBase64 != "aWNvbg==" {
		t.Fatalf("GroupIconGet = %+v, %v", icon, err)
	}
	nextFrame(t, frames, netproto.MsgGroupIconGet)

	if _, err := app.PermList("client", 0, "user", 9); err != nil {
		t.Fatalf("PermList: %v", err)
	}
	var permList netproto.PermList
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgPermList), &permList); err != nil ||
		permList.Tier != "client" || permList.UniqueID != "user" || permList.ChannelID != 9 {
		t.Fatalf("PermList payload = %+v, %v", permList, err)
	}

	if _, err := app.PermTrace("user", "i_client_talk_power", 9); err != nil {
		t.Fatalf("PermTrace: %v", err)
	}
	nextFrame(t, frames, netproto.MsgPermTrace)
	if _, err := app.AuditLog(100, 25); err != nil {
		t.Fatalf("AuditLog: %v", err)
	}
	nextFrame(t, frames, netproto.MsgAuditLog)
	if _, err := app.BanList(); err != nil {
		t.Fatalf("BanList: %v", err)
	}
	nextFrame(t, frames, netproto.MsgBanList)

	if got := app.ChannelEditTree(11, "unknown", 0, 0, 0, false); got != "" {
		t.Fatalf("ChannelEditTree with no fields = %q", got)
	}
	if got := app.ChannelEditTree(11, "join_power, order, parent, inherit", 10, 2, 1, true); got != "" {
		t.Fatalf("ChannelEditTree = %q", got)
	}
	var tree netproto.ChannelEdit
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgChannelEdit), &tree); err != nil ||
		tree.ChannelID != 11 || tree.NeededJoinPower == nil || *tree.NeededJoinPower != 10 ||
		tree.OrderIndex == nil || *tree.OrderIndex != 2 || tree.ParentID == nil || *tree.ParentID != 1 ||
		tree.InheritPermissions == nil || !*tree.InheritPermissions {
		t.Fatalf("ChannelEditTree payload = %+v, %v", tree, err)
	}
}

// perminvalidate_test.go covers the permission-cache hook on channel edits
// that change how the channel tier resolves (157).
package server

import (
	"testing"

	"voicx/internal/netproto"
	"voicx/internal/permissions"
	"voicx/internal/state"
)

// invalidatedAll reports whether a full cache drop was recorded.
func invalidatedAll(env *testEnv) bool {
	env.perms.mu.Lock()
	defer env.perms.mu.Unlock()
	for _, inv := range env.perms.invalidations {
		if inv == [2]int64{-1, -1} {
			return true
		}
	}
	return false
}

// editForInvalidation runs one ChannelEdit and reports whether it dropped the
// permission cache.
func editForInvalidation(t *testing.T, edit netproto.ChannelEdit) bool {
	t.Helper()
	perms := tieredWith(
		boolPerm(permissions.PermissionKeyChannelModify, true),
		intPerm(permissions.PermissionKeyChannelJoinPower, 75),
	)
	env, _ := startTreeEnv(t, &perms)
	defer env.stop()
	env.state.AddChannel(&state.Channel{ChannelID: 1, Name: "Lobby", ChannelType: 2})
	env.state.AddChannel(&state.Channel{ChannelID: 2, Name: "Parent", ChannelType: 2})

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()
	send(t, conn, netproto.MsgChannelEdit, edit)
	readEventOfType(t, conn, "channel_updated")
	return invalidatedAll(env)
}

// TestChannelEditInvalidatesPermCacheOnInherit verifies flipping the
// inheritance flag drops the cache: the loader caches per (user, channel) and
// the flip changes the resolved channel tier for a whole subtree.
func TestChannelEditInvalidatesPermCacheOnInherit(t *testing.T) {
	inherit := true
	if !editForInvalidation(t, netproto.ChannelEdit{ChannelID: 1, InheritPermissions: &inherit}) {
		t.Fatalf("inherit_permissions edit did not invalidate the permission cache")
	}
}

// TestChannelEditInvalidatesPermCacheOnReparent verifies a move does the same:
// a new parent means a different inheritance chain.
func TestChannelEditInvalidatesPermCacheOnReparent(t *testing.T) {
	parent := int64(2)
	if !editForInvalidation(t, netproto.ChannelEdit{ChannelID: 1, ParentID: &parent}) {
		t.Fatalf("parent_id edit did not invalidate the permission cache")
	}
}

// TestChannelEditKeepsPermCacheOtherwise verifies an unrelated edit does not
// pay the cost of a full cache drop.
func TestChannelEditKeepsPermCacheOtherwise(t *testing.T) {
	topic := "hello"
	if editForInvalidation(t, netproto.ChannelEdit{ChannelID: 1, Topic: &topic}) {
		t.Fatalf("a topic edit dropped the whole permission cache")
	}
}

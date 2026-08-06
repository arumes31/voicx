// movepower_test.go pins the move gate (169): moving another client compares
// the caller's i_client_move_power against the TARGET's
// i_client_needed_move_power, not merely "is the caller an admin".
package server

import (
	"testing"
	"time"

	"voicx/internal/auth"
	"voicx/internal/netproto"
	"voicx/internal/permissions"
	"voicx/internal/state"
)

// startMoveEnv starts a server with a second non-admin user, so a move can be
// exercised between two ordinary clients.
func startMoveEnv(t *testing.T, perms permissions.TieredPermissions) *testEnv {
	t.Helper()
	env := startTestEnvDeps(t, &perms, nil, func(d *Deps) {
		fa, ok := d.Auth.(*fakeAuth)
		if !ok {
			t.Fatalf("auth backend is %T, want *fakeAuth", d.Auth)
		}
		fa.passwords["user2-uid"] = "pw"
		fa.users["user2-uid"] = &auth.User{ID: 3, UniqueID: "user2-uid", Nickname: "user2"}
	})
	env.state.AddChannel(&state.Channel{ChannelID: 1, Name: "Lobby", ChannelType: 2})
	return env
}

// TestMoveClientBelowTargetNeededPowerDenied verifies a caller whose move
// power is below the target's needed move power is refused, even though the
// power permission itself is granted.
func TestMoveClientBelowTargetNeededPowerDenied(t *testing.T) {
	env := startMoveEnv(t, tieredWith(
		intPerm(permissions.PermissionKeyClientMovePower, 10),
		intPerm(permissions.PermissionKeyClientNeededMovePower, 50),
	))
	defer env.stop()

	callerConn, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = callerConn.Close() }()
	targetConn, targetID := dialAuthed(t, env.addr, "user2-uid")
	defer func() { _ = targetConn.Close() }()

	send(t, callerConn, netproto.MsgMoveClient, netproto.MoveClient{ClientID: targetID, ChannelID: 1})
	f := readOfType(t, callerConn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("error code = %d, want %d (permission denied)", e.Code, errCodePermissionDenied)
	}
	if sc, ok := env.state.GetClient(targetID); ok && sc.ChannelID == 1 {
		t.Fatal("target was moved despite insufficient move power")
	}
}

// TestMoveClientMeetsTargetNeededPower verifies the move succeeds once the
// caller's move power meets the target's needed move power.
func TestMoveClientMeetsTargetNeededPower(t *testing.T) {
	env := startMoveEnv(t, tieredWith(
		intPerm(permissions.PermissionKeyClientMovePower, 50),
		intPerm(permissions.PermissionKeyClientNeededMovePower, 10),
	))
	defer env.stop()

	callerConn, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = callerConn.Close() }()
	targetConn, targetID := dialAuthed(t, env.addr, "user2-uid")
	defer func() { _ = targetConn.Close() }()

	send(t, callerConn, netproto.MsgMoveClient, netproto.MoveClient{ClientID: targetID, ChannelID: 1})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sc, ok := env.state.GetClient(targetID); ok && sc.ChannelID == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("target was never moved despite sufficient move power")
}

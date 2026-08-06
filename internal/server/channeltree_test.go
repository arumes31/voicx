// channeltree_test.go covers the channel-edit fields that place a channel in
// the tree: needed join power (160), order index (163), parent (168) and the
// permission inheritance toggle (157), plus the join gate they feed.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"voicx/internal/channels"
	"voicx/internal/netproto"
	"voicx/internal/permissions"
	"voicx/internal/state"
)

// treeChannels is a ChannelBackend that mirrors every ChannelUpdate field into
// the state manager, so the handler path can be asserted end to end. updErr,
// when set, is returned instead of applying the update.
type treeChannels struct {
	state *state.Manager

	mu     sync.Mutex
	updErr error
}

func (f *treeChannels) setUpdateError(err error) {
	f.mu.Lock()
	f.updErr = err
	f.mu.Unlock()
}

func (f *treeChannels) CreateChannel(_ context.Context, spec channels.ChannelSpec) (int64, error) {
	f.state.AddChannel(&state.Channel{ChannelID: 1, Name: spec.Name, ChannelType: int(spec.Type)})
	return 1, nil
}

func (f *treeChannels) DeleteChannel(_ context.Context, channelID int64) error {
	f.state.RemoveChannel(channelID)
	return nil
}

func (f *treeChannels) OnClientJoinedChannel(int64) {}
func (f *treeChannels) OnClientLeftChannel(int64)   {}

func (f *treeChannels) UpdateChannel(_ context.Context, channelID int64, upd channels.ChannelUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updErr != nil {
		return f.updErr
	}
	ch, ok := f.state.GetChannel(channelID)
	if !ok {
		return channels.ErrChannelNotFound
	}
	if upd.NeededJoinPower != nil {
		ch.NeededJoinPower = *upd.NeededJoinPower
	}
	if upd.OrderIndex != nil {
		ch.OrderIndex = *upd.OrderIndex
	}
	if upd.ParentID != nil {
		ch.ParentID = *upd.ParentID
	}
	if upd.InheritPermissions != nil {
		ch.InheritPermissions = *upd.InheritPermissions
	}
	return nil
}

// channelUpdatedPayload is the subset of the channel_updated event the tree
// fields ride on.
type channelUpdatedPayload struct {
	ChannelID          int64 `json:"channel_id"`
	NeededJoinPower    int   `json:"needed_join_power"`
	OrderIndex         int   `json:"order_index"`
	ParentID           int64 `json:"parent_id"`
	InheritPermissions bool  `json:"inherit_permissions"`
}

// startTreeEnv starts a server whose channel backend mirrors the tree fields.
func startTreeEnv(t *testing.T, perms *permissions.TieredPermissions) (*testEnv, *treeChannels) {
	t.Helper()
	var fc *treeChannels
	env := startTestEnvDeps(t, perms, nil, func(d *Deps) {
		fc = &treeChannels{state: d.State}
		d.Channels = fc
	})
	return env, fc
}

// TestChannelEditJoinPower verifies a needed-join-power edit reaches state and
// the channel_updated event (160).
func TestChannelEditJoinPower(t *testing.T) {
	perms := tieredWith(
		boolPerm(permissions.PermissionKeyChannelModify, true),
		intPerm(permissions.PermissionKeyChannelJoinPower, 75),
	)
	env, _ := startTreeEnv(t, &perms)
	defer env.stop()

	env.state.AddChannel(&state.Channel{ChannelID: 1, Name: "Lobby", ChannelType: 2})

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = conn.Close() }()

	power := 50
	send(t, conn, netproto.MsgChannelEdit, netproto.ChannelEdit{ChannelID: 1, NeededJoinPower: &power})

	var ev channelUpdatedPayload
	if err := json.Unmarshal(readEventOfType(t, conn, "channel_updated"), &ev); err != nil {
		t.Fatalf("decode channel_updated: %v", err)
	}
	if ev.NeededJoinPower != 50 {
		t.Fatalf("event needed_join_power = %d, want 50", ev.NeededJoinPower)
	}
	if ch, _ := env.state.GetChannel(1); ch == nil || ch.NeededJoinPower != 50 {
		t.Fatalf("state needed join power = %+v, want 50", ch)
	}
}

// TestChannelEditJoinPowerAboveOwnDenied verifies the power cap: an editor may
// not set a needed join power above their own join power (160).
func TestChannelEditJoinPowerAboveOwnDenied(t *testing.T) {
	perms := tieredWith(
		boolPerm(permissions.PermissionKeyChannelModify, true),
		intPerm(permissions.PermissionKeyChannelJoinPower, 10),
	)
	env, _ := startTreeEnv(t, &perms)
	defer env.stop()

	env.state.AddChannel(&state.Channel{ChannelID: 1, Name: "Lobby", ChannelType: 2})

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = conn.Close() }()

	power := 99
	send(t, conn, netproto.MsgChannelEdit, netproto.ChannelEdit{ChannelID: 1, NeededJoinPower: &power})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("error code = %d, want %d (permission denied)", e.Code, errCodePermissionDenied)
	}
	if ch, _ := env.state.GetChannel(1); ch != nil && ch.NeededJoinPower != 0 {
		t.Fatalf("needed join power = %d, want unchanged 0", ch.NeededJoinPower)
	}
}

func TestChannelEditJoinPowerReductionDenied(t *testing.T) {
	perms := tieredWith(
		boolPerm(permissions.PermissionKeyChannelModify, true),
		intPerm(permissions.PermissionKeyChannelJoinPower, 75),
	)
	env, _ := startTreeEnv(t, &perms)
	defer env.stop()
	env.state.AddChannel(&state.Channel{ChannelID: 1, Name: "Lobby", ChannelType: 2, NeededJoinPower: 20})

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = conn.Close() }()
	power := 0
	send(t, conn, netproto.MsgChannelEdit, netproto.ChannelEdit{ChannelID: 1, NeededJoinPower: &power})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatal(err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("error code = %d, want %d", e.Code, errCodePermissionDenied)
	}
	if ch, _ := env.state.GetChannel(1); ch.NeededJoinPower != 20 {
		t.Fatalf("needed join power = %d, want unchanged 20", ch.NeededJoinPower)
	}
}

// TestChannelEditReorderAndReparent verifies order index, parent and the
// inheritance toggle round-trip through the edit path (163/168/157).
func TestChannelEditReorderAndReparent(t *testing.T) {
	perms := tieredWith(boolPerm(permissions.PermissionKeyChannelModify, true))
	env, _ := startTreeEnv(t, &perms)
	defer env.stop()

	env.state.AddChannel(&state.Channel{ChannelID: 1, Name: "Parent", ChannelType: 2})
	env.state.AddChannel(&state.Channel{ChannelID: 2, Name: "Child", ChannelType: 2})

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = conn.Close() }()

	order := 7
	parent := int64(1)
	inherit := true
	send(t, conn, netproto.MsgChannelEdit, netproto.ChannelEdit{
		ChannelID:          2,
		OrderIndex:         &order,
		ParentID:           &parent,
		InheritPermissions: &inherit,
	})

	var ev channelUpdatedPayload
	if err := json.Unmarshal(readEventOfType(t, conn, "channel_updated"), &ev); err != nil {
		t.Fatalf("decode channel_updated: %v", err)
	}
	if ev.OrderIndex != 7 || ev.ParentID != 1 || !ev.InheritPermissions {
		t.Fatalf("channel_updated = %+v, want order 7 parent 1 inherit true", ev)
	}
	ch, _ := env.state.GetChannel(2)
	if ch == nil || ch.OrderIndex != 7 || ch.ParentID != 1 || !ch.InheritPermissions {
		t.Fatalf("state channel = %+v, want order 7 parent 1 inherit true", ch)
	}
}

// TestChannelEditInvalidMoveRefused verifies a rejected re-parent surfaces as
// a malformed error carrying the reason instead of a silent success (168).
func TestChannelEditInvalidMoveRefused(t *testing.T) {
	perms := tieredWith(boolPerm(permissions.PermissionKeyChannelModify, true))
	env, fc := startTreeEnv(t, &perms)
	defer env.stop()

	fc.setUpdateError(fmt.Errorf("%w: channel 2 is an ancestor of 3", channels.ErrInvalidMove))
	env.state.AddChannel(&state.Channel{ChannelID: 2, Name: "Parent", ChannelType: 2})

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = conn.Close() }()

	parent := int64(3)
	send(t, conn, netproto.MsgChannelEdit, netproto.ChannelEdit{ChannelID: 2, ParentID: &parent})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodeMalformed {
		t.Fatalf("error code = %d, want %d (malformed)", e.Code, errCodeMalformed)
	}
	if !strings.Contains(e.Message, "invalid channel move") {
		t.Fatalf("error message = %q, want the move reason", e.Message)
	}
}

// TestJoinChannelInheritedPowerDenied verifies an inheriting sub-channel is
// gated by its parent's needed join power (157/168): joining the child must
// not bypass the parent's gate.
func TestJoinChannelInheritedPowerDenied(t *testing.T) {
	perms := tieredWith(intPerm(permissions.PermissionKeyChannelJoinPower, 10))
	env, _ := startTreeEnv(t, &perms)
	defer env.stop()

	env.state.AddChannel(&state.Channel{ChannelID: 1, Name: "Staff", ChannelType: 2, NeededJoinPower: 50})
	env.state.AddChannel(&state.Channel{ChannelID: 2, Name: "Staff sub", ChannelType: 2, ParentID: 1, InheritPermissions: true})

	conn, clientID := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = conn.Close() }()

	send(t, conn, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 2})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("error code = %d, want %d (permission denied)", e.Code, errCodePermissionDenied)
	}
	if sc, ok := env.state.GetClient(clientID); ok && sc.ChannelID == 2 {
		t.Fatal("client joined a channel gated by its parent")
	}
}

// TestJoinChannelWithoutInheritanceAllowed verifies the same sub-channel is
// joinable once inheritance is off: the gate must come from the chain, not
// from being a child (157).
func TestJoinChannelWithoutInheritanceAllowed(t *testing.T) {
	perms := tieredWith(intPerm(permissions.PermissionKeyChannelJoinPower, 10))
	env, _ := startTreeEnv(t, &perms)
	defer env.stop()

	env.state.AddChannel(&state.Channel{ChannelID: 1, Name: "Staff", ChannelType: 2, NeededJoinPower: 50})
	env.state.AddChannel(&state.Channel{ChannelID: 2, Name: "Staff sub", ChannelType: 2, ParentID: 1})

	conn, clientID := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = conn.Close() }()

	send(t, conn, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 2})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sc, ok := env.state.GetClient(clientID); ok && sc.ChannelID == 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("client never joined the non-inheriting sub-channel")
}

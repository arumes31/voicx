// channeledit_test.go covers the editable tree fields: needed join power
// (160), order index (163), re-parenting with its cycle guard (168), the
// permission inheritance toggle (157), the configurable temporary-channel
// lifetime (165) and the creator's channel-admin assignment (156).
package channels

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// channelRow reads the tree columns of a channel straight from the database.
func channelRow(t *testing.T, mgr *ChannelManager, id int64) (parentID int64, orderIndex, joinPower int, inherit bool) {
	t.Helper()
	err := mgr.store.DB().QueryRowContext(context.Background(),
		`SELECT COALESCE(parent_id, 0), order_index, needed_join_power, inherit_permissions
		 FROM channels WHERE id = $1`, id,
	).Scan(&parentID, &orderIndex, &joinPower, &inherit)
	if err != nil {
		t.Fatalf("channelRow(%d): %v", id, err)
	}
	return
}

// ptr returns a pointer to v, for building a ChannelUpdate.
func ptr[T any](v T) *T { return &v }

// TestUpdateChannelTreeFields verifies join power, order index and the
// inheritance toggle reach both the database and the in-memory state.
func TestUpdateChannelTreeFields(t *testing.T) {
	mgr, _, sm := testEnv(t)
	ctx := context.Background()

	id, err := mgr.CreateChannel(ctx, ChannelSpec{
		Name: fmt.Sprintf("edit-%d", time.Now().UnixNano()),
		Type: ChannelTypePermanent,
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	t.Cleanup(func() { _ = mgr.DeleteChannel(ctx, id) })

	if err := mgr.UpdateChannel(ctx, id, ChannelUpdate{
		NeededJoinPower:    ptr(42),
		OrderIndex:         ptr(7),
		InheritPermissions: ptr(true),
	}); err != nil {
		t.Fatalf("update channel: %v", err)
	}

	_, order, power, inherit := channelRow(t, mgr, id)
	if order != 7 || power != 42 || !inherit {
		t.Fatalf("db row = order %d power %d inherit %v, want 7/42/true", order, power, inherit)
	}
	ch, ok := sm.GetChannel(id)
	if !ok || ch.OrderIndex != 7 || ch.NeededJoinPower != 42 || !ch.InheritPermissions {
		t.Fatalf("state channel = %+v, want order 7 power 42 inherit true", ch)
	}
}

// TestUpdateChannelRejectsNegativeJoinPower verifies the validation guard.
func TestUpdateChannelRejectsNegativeJoinPower(t *testing.T) {
	mgr, _, _ := testEnv(t)
	if err := mgr.UpdateChannel(context.Background(), 1, ChannelUpdate{NeededJoinPower: ptr(-1)}); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("update error = %v, want ErrInvalidSpec", err)
	}
}

// TestUpdateChannelReparent verifies a legal move updates the parent in the
// database and state, and that moving back to the root works.
func TestUpdateChannelReparent(t *testing.T) {
	mgr, _, sm := testEnv(t)
	ctx := context.Background()

	parent, err := mgr.CreateChannel(ctx, ChannelSpec{Name: fmt.Sprintf("p-%d", time.Now().UnixNano()), Type: ChannelTypePermanent})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	t.Cleanup(func() { _ = mgr.DeleteChannel(ctx, parent) })
	child, err := mgr.CreateChannel(ctx, ChannelSpec{Name: fmt.Sprintf("c-%d", time.Now().UnixNano()), Type: ChannelTypePermanent})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	t.Cleanup(func() { _ = mgr.DeleteChannel(ctx, child) })

	if err := mgr.UpdateChannel(ctx, child, ChannelUpdate{ParentID: ptr(parent)}); err != nil {
		t.Fatalf("re-parent: %v", err)
	}
	if got, _, _, _ := channelRow(t, mgr, child); got != parent {
		t.Fatalf("db parent = %d, want %d", got, parent)
	}
	if ch, _ := sm.GetChannel(child); ch == nil || ch.ParentID != parent {
		t.Fatalf("state parent = %+v, want %d", ch, parent)
	}

	if err := mgr.UpdateChannel(ctx, child, ChannelUpdate{ParentID: ptr(int64(0))}); err != nil {
		t.Fatalf("move to root: %v", err)
	}
	if got, _, _, _ := channelRow(t, mgr, child); got != 0 {
		t.Fatalf("db parent = %d, want 0 (root)", got)
	}
	if ch, _ := sm.GetChannel(child); ch == nil || ch.ParentID != 0 {
		t.Fatalf("state parent = %+v, want root", ch)
	}
}

// TestUpdateChannelRejectsCycles verifies the tree cannot be corrupted: a
// channel may not become its own parent, descend from itself, or move under a
// channel that does not exist. Every refusal must leave the tree untouched.
func TestUpdateChannelRejectsCycles(t *testing.T) {
	mgr, _, _ := testEnv(t)
	ctx := context.Background()

	root, err := mgr.CreateChannel(ctx, ChannelSpec{Name: fmt.Sprintf("r-%d", time.Now().UnixNano()), Type: ChannelTypePermanent})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	t.Cleanup(func() { _ = mgr.DeleteChannel(ctx, root) })
	mid, err := mgr.CreateChannel(ctx, ChannelSpec{Name: fmt.Sprintf("m-%d", time.Now().UnixNano()), Type: ChannelTypePermanent, ParentID: root})
	if err != nil {
		t.Fatalf("create mid: %v", err)
	}
	leaf, err := mgr.CreateChannel(ctx, ChannelSpec{Name: fmt.Sprintf("l-%d", time.Now().UnixNano()), Type: ChannelTypePermanent, ParentID: mid})
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}

	cases := map[string]int64{
		"self":       root,
		"descendant": leaf,
		"missing":    999999999,
	}
	for name, target := range cases {
		err := mgr.UpdateChannel(ctx, root, ChannelUpdate{ParentID: ptr(target)})
		if !errors.Is(err, ErrInvalidMove) {
			t.Fatalf("%s move error = %v, want ErrInvalidMove", name, err)
		}
		if got, _, _, _ := channelRow(t, mgr, root); got != 0 {
			t.Fatalf("%s move changed the parent to %d", name, got)
		}
	}

	// The subtree is intact: refusing a move must not detach anything.
	if got, _, _, _ := channelRow(t, mgr, mid); got != root {
		t.Fatalf("mid parent = %d, want %d", got, root)
	}
	if got, _, _, _ := channelRow(t, mgr, leaf); got != mid {
		t.Fatalf("leaf parent = %d, want %d", got, mid)
	}
}

// TestLoadIntoStateReadsInheritance verifies the inheritance toggle survives a
// restart (157): it is read back into state at startup.
func TestLoadIntoStateReadsInheritance(t *testing.T) {
	mgr, _, sm := testEnv(t)
	ctx := context.Background()

	id, err := mgr.CreateChannel(ctx, ChannelSpec{Name: fmt.Sprintf("inh-%d", time.Now().UnixNano()), Type: ChannelTypePermanent})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	t.Cleanup(func() { _ = mgr.DeleteChannel(ctx, id) })
	if err := mgr.UpdateChannel(ctx, id, ChannelUpdate{InheritPermissions: ptr(true), NeededJoinPower: ptr(30)}); err != nil {
		t.Fatalf("update channel: %v", err)
	}

	sm.RemoveChannel(id)
	if _, err := mgr.LoadIntoState(ctx); err != nil {
		t.Fatalf("load into state: %v", err)
	}
	ch, ok := sm.GetChannel(id)
	if !ok || !ch.InheritPermissions || ch.NeededJoinPower != 30 {
		t.Fatalf("reloaded channel = %+v, want inherit true power 30", ch)
	}
}

// TestSetCleanupDelay verifies the temporary-channel lifetime is configurable
// (165): an empty temp channel survives a gap shorter than the lifetime.
func TestSetCleanupDelay(t *testing.T) {
	mgr, s, _ := testEnv(t)
	ctx := context.Background()

	mgr.SetCleanupDelay(2 * time.Second)
	id, err := mgr.CreateChannel(ctx, ChannelSpec{Name: fmt.Sprintf("tmp-%d", time.Now().UnixNano()), Type: ChannelTypeTemporary})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	t.Cleanup(func() { _ = mgr.DeleteChannel(ctx, id) })

	time.Sleep(500 * time.Millisecond)
	if !channelExistsInDB(t, s, id) {
		t.Fatal("temporary channel deleted before its configured lifetime elapsed")
	}

	mgr.SetCleanupDelay(0)
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if got := mgr.cleanupDelayLocked(); got != DefaultCleanupDelay {
		t.Fatalf("cleanup delay = %v, want the default %v", got, DefaultCleanupDelay)
	}
}

// TestCreateChannelAssignsChannelAdmin verifies the creator lands in the
// channel-admin group on the channel they created (156).
func TestCreateChannelAssignsChannelAdmin(t *testing.T) {
	mgr, s, _ := testEnv(t)
	ctx := context.Background()

	g, err := s.FindGroupByName(ctx, "channel", ChannelAdminGroupName)
	if err != nil {
		t.Fatalf("find channel admin group: %v", err)
	}
	if g == nil {
		gid, err := s.CreateGroup(ctx, "channel", ChannelAdminGroupName, 0)
		if err != nil {
			t.Fatalf("create channel admin group: %v", err)
		}
		t.Cleanup(func() { _ = s.DeleteGroup(ctx, "channel", gid, true) })
		g, err = s.FindGroupByName(ctx, "channel", ChannelAdminGroupName)
		if err != nil || g == nil {
			t.Fatalf("re-find channel admin group: %v", err)
		}
	}

	userID := createTestUser(t, s)
	id, err := mgr.CreateChannel(ctx, ChannelSpec{
		Name:      fmt.Sprintf("owned-%d", time.Now().UnixNano()),
		Type:      ChannelTypePermanent,
		CreatedBy: userID,
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	t.Cleanup(func() { _ = mgr.DeleteChannel(ctx, id) })

	var groupID int64
	err = s.DB().QueryRowContext(ctx,
		`SELECT channel_group_id FROM channel_group_members WHERE user_id = $1 AND channel_id = $2`,
		userID, id,
	).Scan(&groupID)
	if err != nil {
		t.Fatalf("creator has no channel group on the new channel: %v", err)
	}
	if groupID != g.ID {
		t.Fatalf("creator channel group = %d, want %d (%s)", groupID, g.ID, ChannelAdminGroupName)
	}
}

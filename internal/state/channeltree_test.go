// channeltree_test.go covers the channel-tree helpers: the total sibling
// order (163) and the permission/join-power inheritance chain (157/168).
package state

import (
	"testing"

	"go.uber.org/zap"
)

func treeManager(chans ...*Channel) *Manager {
	m := New(zap.NewNop())
	for _, ch := range chans {
		m.AddChannel(ch)
	}
	return m
}

func orderOf(m *Manager) []int64 {
	var ids []int64
	for _, ch := range m.ChannelTreeOrdered() {
		ids = append(ids, ch.ChannelID)
	}
	return ids
}

// TestChannelTreeOrderedIsTotal verifies siblings sharing an order index still
// have one fixed position: repeated calls must not reshuffle the tree.
func TestChannelTreeOrderedIsTotal(t *testing.T) {
	m := treeManager(
		&Channel{ChannelID: 5, ParentID: 1, OrderIndex: 0},
		&Channel{ChannelID: 3, ParentID: 1, OrderIndex: 0},
		&Channel{ChannelID: 9, ParentID: 1, OrderIndex: 0},
		&Channel{ChannelID: 1, OrderIndex: 2},
		&Channel{ChannelID: 2, OrderIndex: 1},
	)

	want := []int64{2, 1, 3, 5, 9}
	for i := 0; i < 20; i++ {
		got := orderOf(m)
		if len(got) != len(want) {
			t.Fatalf("order = %v, want %v", got, want)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("order = %v, want %v", got, want)
			}
		}
	}
}

// TestChannelTreeOrderedRespectsOrderIndex verifies the order index still wins
// over the id tiebreak.
func TestChannelTreeOrderedRespectsOrderIndex(t *testing.T) {
	m := treeManager(
		&Channel{ChannelID: 1, OrderIndex: 10},
		&Channel{ChannelID: 2, OrderIndex: 5},
	)
	got := orderOf(m)
	if len(got) != 2 || got[0] != 2 || got[1] != 1 {
		t.Fatalf("order = %v, want [2 1]", got)
	}
}

// TestChannelPermissionChain verifies the chain stops at the first channel
// that does not inherit (157).
func TestChannelPermissionChain(t *testing.T) {
	m := treeManager(
		&Channel{ChannelID: 1},
		&Channel{ChannelID: 2, ParentID: 1, InheritPermissions: true},
		&Channel{ChannelID: 3, ParentID: 2, InheritPermissions: true},
		&Channel{ChannelID: 4, ParentID: 1},
	)

	chain := m.ChannelPermissionChain(3)
	want := []int64{3, 2, 1}
	if len(chain) != len(want) {
		t.Fatalf("chain = %v, want %v", chain, want)
	}
	for i := range want {
		if chain[i] != want[i] {
			t.Fatalf("chain = %v, want %v", chain, want)
		}
	}

	if chain := m.ChannelPermissionChain(4); len(chain) != 1 || chain[0] != 4 {
		t.Fatalf("non-inheriting chain = %v, want [4]", chain)
	}
	if chain := m.ChannelPermissionChain(99); chain != nil {
		t.Fatalf("unknown channel chain = %v, want nil", chain)
	}
}

// TestEffectiveJoinPower verifies an inheriting sub-channel takes the highest
// needed power on its chain, and a non-inheriting one only its own (157/168).
func TestEffectiveJoinPower(t *testing.T) {
	m := treeManager(
		&Channel{ChannelID: 1, NeededJoinPower: 50},
		&Channel{ChannelID: 2, ParentID: 1, InheritPermissions: true, NeededJoinPower: 10},
		&Channel{ChannelID: 3, ParentID: 1, NeededJoinPower: 10},
		&Channel{ChannelID: 4, ParentID: 2, InheritPermissions: true, NeededJoinPower: 75},
	)

	cases := map[int64]int{1: 50, 2: 50, 3: 10, 4: 75, 99: 0}
	for id, want := range cases {
		if got := m.EffectiveJoinPower(id); got != want {
			t.Fatalf("EffectiveJoinPower(%d) = %d, want %d", id, got, want)
		}
	}
}

// TestChannelAncestors verifies the parent walk used as the move cycle guard
// (168), including a hand-corrupted cycle that must terminate.
func TestChannelAncestors(t *testing.T) {
	m := treeManager(
		&Channel{ChannelID: 1},
		&Channel{ChannelID: 2, ParentID: 1},
		&Channel{ChannelID: 3, ParentID: 2},
	)
	got := m.ChannelAncestors(3)
	if len(got) != 2 || got[0] != 2 || got[1] != 1 {
		t.Fatalf("ancestors = %v, want [2 1]", got)
	}
	if got := m.ChannelAncestors(1); len(got) != 0 {
		t.Fatalf("root ancestors = %v, want none", got)
	}

	cyclic := treeManager(
		&Channel{ChannelID: 1, ParentID: 2},
		&Channel{ChannelID: 2, ParentID: 1},
	)
	if got := cyclic.ChannelAncestors(1); len(got) != maxChannelDepth {
		t.Fatalf("cyclic walk length = %d, want the depth cap %d", len(got), maxChannelDepth)
	}
}

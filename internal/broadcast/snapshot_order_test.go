package broadcast

import (
	"encoding/json"
	"testing"

	"voicx/internal/state"
)

// TestBuildSnapshotOrderIsStable verifies siblings sharing an order index keep
// one fixed position across snapshots, and that the order index still wins
// over the id tiebreak (163).
func TestBuildSnapshotOrderIsStable(t *testing.T) {
	sm := newTestManager()
	sm.AddChannel(&state.Channel{ChannelID: 1, Name: "Root", OrderIndex: 5})
	sm.AddChannel(&state.Channel{ChannelID: 2, Name: "First", OrderIndex: 1})
	for _, id := range []int64{30, 10, 20} {
		sm.AddChannel(&state.Channel{ChannelID: id, ParentID: 1, Name: "Child", OrderIndex: 0})
	}

	want := []int64{10, 20, 30}
	for i := 0; i < 20; i++ {
		snap := BuildSnapshot(sm, true, "")
		if len(snap.RootChannels) != 2 || snap.RootChannels[0].ChannelID != 2 {
			t.Fatalf("root order = %+v, want channel 2 first", snap.RootChannels)
		}
		children := snap.RootChannels[1].Children
		if len(children) != len(want) {
			t.Fatalf("children = %d, want %d", len(children), len(want))
		}
		for j, id := range want {
			if children[j].ChannelID != id {
				t.Fatalf("children order = %v, want %v", children, want)
			}
		}
	}
}

// TestSnapshotChannelFieldNames pins the marshaling convention: state.Channel
// carries no json tags, so the client reads Go field names. A tag added to one
// field would silently rename it in every snapshot (304).
func TestSnapshotChannelFieldNames(t *testing.T) {
	sm := newTestManager()
	sm.AddChannel(&state.Channel{ChannelID: 1, Name: "Root", OrderIndex: 3, NeededJoinPower: 50, InheritPermissions: true})

	raw, err := json.Marshal(BuildSnapshot(sm, true, ""))
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var snap struct {
		RootChannels []map[string]json.RawMessage `json:"root_channels"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if len(snap.RootChannels) != 1 {
		t.Fatalf("root channels = %d, want 1", len(snap.RootChannels))
	}
	for _, key := range []string{"OrderIndex", "ParentID", "NeededJoinPower", "InheritPermissions"} {
		if _, ok := snap.RootChannels[0][key]; !ok {
			t.Fatalf("snapshot channel has no %q field: %s", key, raw)
		}
	}
}

// querylist_test.go covers the ServerQuery channel listing order (163).
package main

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"voicx/internal/query"
	"voicx/internal/state"
)

// TestListChannelsTotalOrder verifies channellist uses the total
// (parent, order index, id) order, so a ServerQuery bot and the connected
// clients never disagree about where a channel sits. Siblings sharing an
// order index are the case the old unstable sort reshuffled.
func TestListChannelsTotalOrder(t *testing.T) {
	sm := state.New(zap.NewNop())
	sm.AddChannel(&state.Channel{ChannelID: 1, Name: "Root", OrderIndex: 5})
	sm.AddChannel(&state.Channel{ChannelID: 2, Name: "First", OrderIndex: 1})
	for _, id := range []int64{30, 10, 20} {
		sm.AddChannel(&state.Channel{ChannelID: id, ParentID: 1, Name: "Child", OrderIndex: 0})
	}

	q := &queryBackend{stateMgr: sm}
	want := []int64{2, 1, 10, 20, 30}
	// Repeat: an unstable sort on a non-total key only misbehaves sometimes.
	for i := 0; i < 20; i++ {
		got := q.ListChannels(context.Background())
		if len(got) != len(want) {
			t.Fatalf("channellist returned %d rows, want %d", len(got), len(want))
		}
		for j, id := range want {
			if got[j].ChannelID != id {
				t.Fatalf("channellist order = %v, want %v", ids(got), want)
			}
		}
	}
}

func ids(rows []query.ChannelInfo) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ChannelID)
	}
	return out
}

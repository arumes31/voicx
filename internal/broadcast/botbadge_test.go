package broadcast

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"voicx/internal/state"
)

// TestSnapshotCarriesBotFlag verifies the bot flag survives the state ->
// ClientInfo conversion and reaches the wire under is_bot (180).
func TestSnapshotCarriesBotFlag(t *testing.T) {
	sm := newTestManager()
	sm.AddChannel(&state.Channel{ChannelID: 1, Name: "Lobby"})
	sm.AddClient(&state.Client{ClientID: "c-bot", UniqueID: "bot-uid", Nickname: "bot", IsBot: true, ConnectedAt: time.Now()})
	sm.AddClient(&state.Client{ClientID: "c-human", UniqueID: "human-uid", Nickname: "human", ConnectedAt: time.Now()})
	sm.MoveClient("c-bot", 1)
	sm.MoveClient("c-human", 1)

	raw, err := json.Marshal(BuildSnapshot(sm, true, ""))
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var snap TreeSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if len(snap.RootChannels) != 1 {
		t.Fatalf("root channels = %d, want 1", len(snap.RootChannels))
	}
	seen := map[string]bool{}
	for _, ci := range snap.RootChannels[0].Clients {
		seen[ci.UniqueID] = ci.IsBot
	}
	if !seen["bot-uid"] {
		t.Errorf("bot client lost its flag in the snapshot: %s", raw)
	}
	if seen["human-uid"] {
		t.Errorf("non-bot client gained a bot flag: %s", raw)
	}
	// omitempty keeps the key off every non-bot entry, so old clients and
	// snapshot size are unaffected by 180.
	if strings.Count(string(raw), `"is_bot"`) != 1 {
		t.Errorf("is_bot appears for non-bots too: %s", raw)
	}
}

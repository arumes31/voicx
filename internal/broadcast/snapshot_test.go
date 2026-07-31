package broadcast

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"voicx/internal/state"
)

func newTestManager() *state.Manager {
	return state.New(zap.NewNop())
}

func TestBuildSnapshot_Populated(t *testing.T) {
	sm := newTestManager()

	root := &state.Channel{ChannelID: 1, ParentID: 0, Name: "Root", OrderIndex: 0, ChannelType: 2, MaxClients: 100, CreatedAt: time.Now()}
	child := &state.Channel{ChannelID: 2, ParentID: 1, Name: "Child", OrderIndex: 0, ChannelType: 0, MaxClients: 10, CreatedAt: time.Now()}
	sm.AddChannel(root)
	sm.AddChannel(child)

	c1 := &state.Client{ClientID: "c1", UniqueID: "u1", Nickname: "Alice", ChannelID: 2, ConnectedAt: time.Now()}
	c2 := &state.Client{ClientID: "c2", UniqueID: "u2", Nickname: "Bob", ChannelID: 2, ConnectedAt: time.Now()}
	sm.AddClient(c1)
	sm.AddClient(c2)
	if err := sm.JoinChannel("c1", 2); err != nil {
		t.Fatalf("join c1: %v", err)
	}
	if err := sm.JoinChannel("c2", 2); err != nil {
		t.Fatalf("join c2: %v", err)
	}

	snap := BuildSnapshot(sm)
	if snap == nil {
		t.Fatal("snapshot is nil")
	}
	if len(snap.RootChannels) != 1 {
		t.Fatalf("expected 1 root channel, got %d", len(snap.RootChannels))
	}
	rootNode := snap.RootChannels[0]
	if rootNode.ChannelID != 1 {
		t.Fatalf("expected root id 1, got %d", rootNode.ChannelID)
	}
	if len(rootNode.Children) != 1 || rootNode.Children[0].ChannelID != 2 {
		t.Fatalf("expected root to have child id 2, got %+v", rootNode.Children)
	}
	childNode := rootNode.Children[0]
	if len(childNode.Clients) != 2 {
		t.Fatalf("expected 2 clients in child, got %d", len(childNode.Clients))
	}
	if snap.TotalChannels != 2 {
		t.Fatalf("expected total channels 2, got %d", snap.TotalChannels)
	}
	if snap.TotalClients != 2 {
		t.Fatalf("expected total clients 2, got %d", snap.TotalClients)
	}

	// JSON marshal must not contain a Conn field.
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(data)
	if strings.Contains(s, "Conn") {
		t.Fatalf("serialized snapshot must not contain Conn field: %s", s)
	}
	if !strings.Contains(s, "root_channels") {
		t.Fatalf("expected root_channels in JSON: %s", s)
	}
}

func TestBuildSnapshot_Empty(t *testing.T) {
	sm := newTestManager()
	snap := BuildSnapshot(sm)
	if snap == nil {
		t.Fatal("snapshot is nil")
	}
	if snap.TotalChannels != 0 {
		t.Fatalf("expected 0 channels, got %d", snap.TotalChannels)
	}
	if snap.TotalClients != 0 {
		t.Fatalf("expected 0 clients, got %d", snap.TotalClients)
	}
	if len(snap.RootChannels) != 0 {
		t.Fatalf("expected 0 root channels, got %d", len(snap.RootChannels))
	}
}

func TestBuildSnapshot_NilManager(t *testing.T) {
	snap := BuildSnapshot(nil)
	if snap == nil {
		t.Fatal("snapshot is nil")
	}
	if snap.TotalChannels != 0 || snap.TotalClients != 0 {
		t.Fatalf("expected zero totals for nil manager")
	}
}

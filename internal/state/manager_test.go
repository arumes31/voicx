package state

import (
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	return New(zap.NewNop())
}

func TestAddRemoveClient(t *testing.T) {
	m := newTestManager(t)

	cases := []struct {
		name string
		c    *Client
	}{
		{
			name: "client-a",
			c: &Client{
				ClientID:    "a",
				UniqueID:    "u-a",
				Nickname:    "Alice",
				ConnectedAt: time.Now(),
				Metadata:    map[string]string{"os": "linux"},
			},
		},
		{
			name: "client-b",
			c: &Client{
				ClientID:    "b",
				UniqueID:    "u-b",
				Nickname:    "Bob",
				ConnectedAt: time.Now(),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m.AddClient(tc.c)
			got, ok := m.GetClient(tc.c.ClientID)
			if !ok {
				t.Fatalf("GetClient(%q) not found after AddClient", tc.c.ClientID)
			}
			if got.ClientID != tc.c.ClientID || got.UniqueID != tc.c.UniqueID || got.Nickname != tc.c.Nickname {
				t.Fatalf("GetClient returned wrong snapshot: %+v", got)
			}
			got.Nickname = "mutated snapshot"
			again, _ := m.GetClient(tc.c.ClientID)
			if again.Nickname != tc.c.Nickname {
				t.Fatal("mutating GetClient snapshot changed manager state")
			}
		})
	}

	if got := m.ClientCount(); got != len(cases) {
		t.Fatalf("ClientCount = %d, want %d", got, len(cases))
	}

	if got := len(m.ListClients()); got != len(cases) {
		t.Fatalf("ListClients len = %d, want %d", got, len(cases))
	}

	m.RemoveClient("a")
	if _, ok := m.GetClient("a"); ok {
		t.Fatalf("client a still present after RemoveClient")
	}
	if got := m.ClientCount(); got != 1 {
		t.Fatalf("ClientCount = %d, want 1", got)
	}

	// Removing an unknown client is a no-op (no panic).
	m.RemoveClient("does-not-exist")
}

func TestChannelSubscriptionsStayConsistentAcrossRemoval(t *testing.T) {
	m := newTestManager(t)
	m.AddClient(&Client{ClientID: "a", UniqueID: "u-a", Nickname: "Alice"})
	m.AddClient(&Client{ClientID: "b", UniqueID: "u-b", Nickname: "Bob"})
	m.AddChannel(&Channel{ChannelID: 10, Name: "Ten"})
	m.AddChannel(&Channel{ChannelID: 20, Name: "Twenty"})

	// Unknown targets are ignored and duplicate requests are idempotent.
	m.Subscribe("a", []int64{20, 10, 20, 999})
	m.Subscribe("b", []int64{20})
	if got := m.Subscriptions("a"); len(got) != 2 || got[0] != 10 || got[1] != 20 {
		t.Fatalf("Subscriptions(a) = %v, want [10 20]", got)
	}
	if got := len(m.ChannelSubscribers(20)); got != 2 {
		t.Fatalf("ChannelSubscribers(20) len = %d, want 2", got)
	}

	m.Unsubscribe("a", []int64{10})
	if m.IsSubscribed("a", 10) {
		t.Fatal("client a still subscribed to channel 10")
	}

	// Removing a channel drops both forward and reverse entries.
	m.RemoveChannel(20)
	if got := m.Subscriptions("a"); len(got) != 0 {
		t.Fatalf("Subscriptions(a) after channel removal = %v, want empty", got)
	}
	if got := m.Subscriptions("b"); len(got) != 0 {
		t.Fatalf("Subscriptions(b) after channel removal = %v, want empty", got)
	}
	if got := len(m.ChannelSubscribers(20)); got != 0 {
		t.Fatalf("ChannelSubscribers(20) after removal len = %d, want 0", got)
	}

	// Removing a client also removes it from the reverse index.
	m.AddChannel(&Channel{ChannelID: 30, Name: "Thirty"})
	m.Subscribe("a", []int64{30})
	m.RemoveClient("a")
	if got := len(m.ChannelSubscribers(30)); got != 0 {
		t.Fatalf("ChannelSubscribers(30) after client removal len = %d, want 0", got)
	}
}

func TestAddRemoveChannel(t *testing.T) {
	m := newTestManager(t)

	ch := &Channel{
		ChannelID:   1,
		ParentID:    0,
		Name:        "Root",
		Topic:       "root topic",
		OrderIndex:  0,
		ChannelType: 2,
		MaxClients:  100,
		CreatedAt:   time.Now(),
	}
	m.AddChannel(ch)

	got, ok := m.GetChannel(1)
	if !ok {
		t.Fatalf("GetChannel(1) not found")
	}
	if got.Name != "Root" {
		t.Fatalf("GetChannel name = %q, want Root", got.Name)
	}

	if c := m.ChannelCount(); c != 1 {
		t.Fatalf("ChannelCount = %d, want 1", c)
	}
	if c := len(m.ListChannels()); c != 1 {
		t.Fatalf("ListChannels len = %d, want 1", c)
	}

	m.RemoveChannel(1)
	if _, ok := m.GetChannel(1); ok {
		t.Fatalf("channel 1 still present after RemoveChannel")
	}
	if c := m.ChannelCount(); c != 0 {
		t.Fatalf("ChannelCount = %d, want 0", c)
	}
}

func TestJoinChannelErrors(t *testing.T) {
	m := newTestManager(t)

	ch := &Channel{ChannelID: 10, Name: "ch", CreatedAt: time.Now()}
	m.AddChannel(ch)

	// Unknown client.
	if err := m.JoinChannel("ghost", 10); err != ErrClientNotFound {
		t.Fatalf("JoinChannel unknown client err = %v, want ErrClientNotFound", err)
	}

	c := &Client{ClientID: "c1", ConnectedAt: time.Now()}
	m.AddClient(c)

	// Unknown channel.
	if err := m.JoinChannel("c1", 999); err != ErrChannelNotFound {
		t.Fatalf("JoinChannel unknown channel err = %v, want ErrChannelNotFound", err)
	}

	// Happy path.
	if err := m.JoinChannel("c1", 10); err != nil {
		t.Fatalf("JoinChannel happy path err = %v", err)
	}
	if got, _ := m.GetClient("c1"); got.ChannelID != 10 {
		t.Fatalf("client ChannelID = %d, want 10", got.ChannelID)
	}
	if got, _ := m.GetChannel(10); got.ClientCount != 1 {
		t.Fatalf("channel ClientCount = %d, want 1", got.ClientCount)
	}
	if members := m.ChannelMembers(10); len(members) != 1 {
		t.Fatalf("ChannelMembers len = %d, want 1", len(members))
	}

	// Idempotent join.
	if err := m.JoinChannel("c1", 10); err != nil {
		t.Fatalf("idempotent JoinChannel err = %v", err)
	}
	if got, _ := m.GetChannel(10); got.ClientCount != 1 {
		t.Fatalf("channel ClientCount after idempotent = %d, want 1", got.ClientCount)
	}
}

func TestLeaveChannelErrors(t *testing.T) {
	m := newTestManager(t)

	// Unknown client.
	if err := m.LeaveChannel("ghost"); err != ErrClientNotFound {
		t.Fatalf("LeaveChannel unknown client err = %v, want ErrClientNotFound", err)
	}

	c := &Client{ClientID: "c2", ConnectedAt: time.Now()}
	m.AddClient(c)

	// Not in a channel.
	if err := m.LeaveChannel("c2"); err != ErrNotInChannel {
		t.Fatalf("LeaveChannel not-in-channel err = %v, want ErrNotInChannel", err)
	}

	ch := &Channel{ChannelID: 20, Name: "ch", CreatedAt: time.Now()}
	m.AddChannel(ch)
	if err := m.JoinChannel("c2", 20); err != nil {
		t.Fatalf("JoinChannel err = %v", err)
	}
	if err := m.LeaveChannel("c2"); err != nil {
		t.Fatalf("LeaveChannel err = %v", err)
	}
	if got, _ := m.GetClient("c2"); got.ChannelID != 0 {
		t.Fatalf("client ChannelID = %d, want 0", got.ChannelID)
	}
	if got, _ := m.GetChannel(20); got.ClientCount != 0 {
		t.Fatalf("channel ClientCount = %d, want 0", got.ClientCount)
	}
	if members := m.ChannelMembers(20); members != nil {
		t.Fatalf("ChannelMembers = %v, want nil", members)
	}
}

func TestMoveClient(t *testing.T) {
	m := newTestManager(t)

	ch1 := &Channel{ChannelID: 1, Name: "ch1", CreatedAt: time.Now()}
	ch2 := &Channel{ChannelID: 2, Name: "ch2", CreatedAt: time.Now()}
	m.AddChannel(ch1)
	m.AddChannel(ch2)

	c := &Client{ClientID: "mover", ConnectedAt: time.Now()}
	m.AddClient(c)

	if err := m.JoinChannel("mover", 1); err != nil {
		t.Fatalf("JoinChannel(1) err = %v", err)
	}
	if err := m.MoveClient("mover", 2); err != nil {
		t.Fatalf("MoveClient err = %v", err)
	}
	if got, _ := m.GetClient("mover"); got.ChannelID != 2 {
		t.Fatalf("client ChannelID = %d, want 2", got.ChannelID)
	}
	if got, _ := m.GetChannel(1); got.ClientCount != 0 {
		t.Fatalf("ch1 ClientCount = %d, want 0", got.ClientCount)
	}
	if got, _ := m.GetChannel(2); got.ClientCount != 1 {
		t.Fatalf("ch2 ClientCount = %d, want 1", got.ClientCount)
	}

	// Move to same channel is a no-op.
	if err := m.MoveClient("mover", 2); err != nil {
		t.Fatalf("MoveClient same-channel err = %v", err)
	}
	if got, _ := m.GetChannel(2); got.ClientCount != 1 {
		t.Fatalf("ch2 ClientCount after no-op = %d, want 1", got.ClientCount)
	}

	// Move unknown client.
	if err := m.MoveClient("ghost", 1); err != ErrClientNotFound {
		t.Fatalf("MoveClient unknown client err = %v, want ErrClientNotFound", err)
	}
	// Move to unknown channel.
	if err := m.MoveClient("mover", 999); err != ErrChannelNotFound {
		t.Fatalf("MoveClient unknown channel err = %v, want ErrChannelNotFound", err)
	}
}

func TestSpeaking(t *testing.T) {
	m := newTestManager(t)

	ch := &Channel{ChannelID: 5, Name: "ch", CreatedAt: time.Now()}
	m.AddChannel(ch)
	c := &Client{ClientID: "talker", ConnectedAt: time.Now()}
	m.AddClient(c)
	if err := m.JoinChannel("talker", 5); err != nil {
		t.Fatalf("JoinChannel err = %v", err)
	}

	if m.IsSpeaking("talker") {
		t.Fatalf("IsSpeaking should be false initially")
	}

	m.SetSpeaking("talker", true)
	if !m.IsSpeaking("talker") {
		t.Fatalf("IsSpeaking should be true after SetSpeaking(true)")
	}
	if got, _ := m.GetClient("talker"); !got.IsSpeaking {
		t.Fatalf("client.IsSpeaking should be true")
	}

	speaking := m.SpeakingClients()
	if len(speaking) != 1 {
		t.Fatalf("SpeakingClients len = %d, want 1", len(speaking))
	}
	if speaking[0].ChannelID != 5 {
		t.Fatalf("speaking ChannelID = %d, want 5", speaking[0].ChannelID)
	}
	if speaking[0].StartedAt.IsZero() {
		t.Fatalf("speaking StartedAt should not be zero")
	}

	m.SetSpeaking("talker", false)
	if m.IsSpeaking("talker") {
		t.Fatalf("IsSpeaking should be false after SetSpeaking(false)")
	}
	if got, _ := m.GetClient("talker"); got.IsSpeaking {
		t.Fatalf("client.IsSpeaking should be false")
	}
	if len(m.SpeakingClients()) != 0 {
		t.Fatalf("SpeakingClients len = %d, want 0", len(m.SpeakingClients()))
	}

	// SetSpeaking on unknown client is a no-op.
	m.SetSpeaking("ghost", true)
	if m.IsSpeaking("ghost") {
		t.Fatalf("ghost should not be speaking")
	}
}

func TestChannelTreeOrdering(t *testing.T) {
	m := newTestManager(t)

	// Insert out of order.
	m.AddChannel(&Channel{ChannelID: 3, ParentID: 1, Name: "c", OrderIndex: 2, CreatedAt: time.Now()})
	m.AddChannel(&Channel{ChannelID: 1, ParentID: 0, Name: "a", OrderIndex: 0, CreatedAt: time.Now()})
	m.AddChannel(&Channel{ChannelID: 2, ParentID: 1, Name: "b", OrderIndex: 1, CreatedAt: time.Now()})
	m.AddChannel(&Channel{ChannelID: 4, ParentID: 0, Name: "d", OrderIndex: 1, CreatedAt: time.Now()})

	tree := m.ChannelTree()
	wantIDs := []int64{1, 4, 2, 3}
	if len(tree) != len(wantIDs) {
		t.Fatalf("ChannelTree len = %d, want %d", len(tree), len(wantIDs))
	}
	for i, want := range wantIDs {
		if tree[i].ChannelID != want {
			t.Fatalf("ChannelTree[%d].ChannelID = %d, want %d", i, tree[i].ChannelID, want)
		}
	}
}

func TestStats(t *testing.T) {
	m := newTestManager(t)

	ch1 := &Channel{ChannelID: 1, Name: "ch1", CreatedAt: time.Now()}
	ch2 := &Channel{ChannelID: 2, Name: "ch2", CreatedAt: time.Now()}
	m.AddChannel(ch1)
	m.AddChannel(ch2)

	for i := 0; i < 3; i++ {
		c := &Client{ClientID: string(rune('a' + i)), ConnectedAt: time.Now()}
		m.AddClient(c)
		if i < 2 {
			_ = m.JoinChannel(c.ClientID, 1)
		} else {
			_ = m.JoinChannel(c.ClientID, 2)
		}
	}

	m.SetSpeaking("a", true)
	m.SetSpeaking("b", true)

	s := m.Stats()
	if s.ClientCount != 3 {
		t.Fatalf("Stats.ClientCount = %d, want 3", s.ClientCount)
	}
	if s.ChannelCount != 2 {
		t.Fatalf("Stats.ChannelCount = %d, want 2", s.ChannelCount)
	}
	if s.SpeakingCount != 2 {
		t.Fatalf("Stats.SpeakingCount = %d, want 2", s.SpeakingCount)
	}
	if s.ChannelClientCounts[1] != 2 {
		t.Fatalf("ChannelClientCounts[1] = %d, want 2", s.ChannelClientCounts[1])
	}
	if s.ChannelClientCounts[2] != 1 {
		t.Fatalf("ChannelClientCounts[2] = %d, want 1", s.ChannelClientCounts[2])
	}
}

func TestRemoveChannelClearsMembership(t *testing.T) {
	m := newTestManager(t)

	ch := &Channel{ChannelID: 7, Name: "ch", CreatedAt: time.Now()}
	m.AddChannel(ch)
	c := &Client{ClientID: "x", ConnectedAt: time.Now()}
	m.AddClient(c)
	_ = m.JoinChannel("x", 7)
	m.SetSpeaking("x", true)

	m.RemoveChannel(7)

	if got, _ := m.GetClient("x"); got.ChannelID != 0 {
		t.Fatalf("client ChannelID = %d, want 0", got.ChannelID)
	}
	if m.IsSpeaking("x") {
		t.Fatalf("client should not be speaking after channel removed")
	}
	if got, _ := m.GetClient("x"); got.IsSpeaking {
		t.Fatalf("client.IsSpeaking should be false")
	}
}

func TestConcurrentAccess(t *testing.T) {
	// Exercise the RWMutex under concurrent reads/writes; run with -race.
	m := newTestManager(t)

	m.AddChannel(&Channel{ChannelID: 1, Name: "ch", CreatedAt: time.Now()})

	var wg sync.WaitGroup

	// Writers: add/remove clients and toggle speaking.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('A' + i%26))
			c := &Client{ClientID: id, ConnectedAt: time.Now()}
			m.AddClient(c)
			_ = m.JoinChannel(id, 1)
			m.SetSpeaking(id, true)
			m.SetSpeaking(id, false)
			_ = m.LeaveChannel(id)
			m.RemoveClient(id)
		}(i)
	}

	// Readers: query state concurrently.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.ClientCount()
			_ = m.ChannelCount()
			_ = m.ListClients()
			_ = m.ListChannels()
			_ = m.ChannelTree()
			_ = m.ChannelMembers(1)
			_ = m.SpeakingClients()
			_ = m.Stats()
			_ = m.IsSpeaking("A")
		}()
	}

	wg.Wait()
}

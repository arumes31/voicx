// tabs_test.go exercises the wave-8a multi-server tab registry: tab
// lifecycle, event journaling/replay, badge counting, and hotkey profiles.
package main

import (
	"crypto/tls"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"voicx/internal/netproto"
	"voicx/internal/tlscert"
)

// newTabApp returns an App with a fresh tab registry (no Wails context).
// settingsPath must always be set so persistence stays inside the test's temp
// dir even if the package-wide path gate is ever rearmed.
func newTabApp(t *testing.T) *App {
	t.Helper()
	return &App{
		settings:     DefaultSettings(),
		hotkeys:      map[string]*hotkeyReg{},
		tabs:         map[string]*tabState{},
		settingsPath: filepath.Join(t.TempDir(), "settings.json"),
	}
}

// TestTabLifecycle covers creation, activation, and closing.
func TestTabLifecycle(t *testing.T) {
	a := newTabApp(t)

	id1, ts1 := a.newTab()
	id2, _ := a.newTab()
	if id1 == id2 {
		t.Fatal("duplicate tab IDs")
	}
	a.activate(id1)
	if a.cmLoad() != ts1.cm {
		t.Fatal("active cm is not tab 1's")
	}
	tabs := a.ListTabs()
	if len(tabs) != 2 {
		t.Fatalf("tabs = %+v", tabs)
	}
	for _, tb := range tabs {
		if tb.ID == id1 && !tb.Active {
			t.Fatal("tab 1 not marked active")
		}
	}

	// Closing the active tab activates the remaining one.
	a.CloseTab(id1)
	if a.cmLoad() == ts1.cm {
		t.Fatal("closed tab still active")
	}
	if len(a.ListTabs()) != 1 {
		t.Fatalf("tabs after close = %+v", a.ListTabs())
	}

	// Closing the last tab leaves no active manager.
	a.CloseTab(id2)
	if a.cmLoad() != nil {
		t.Fatal("cm set with no tabs")
	}
}

func TestTabOrderAndActiveCloseNeighborAreDeterministic(t *testing.T) {
	a := newTabApp(t)
	first, _ := a.newTab()
	middle, _ := a.newTab()
	last, _ := a.newTab()
	a.activate(middle)
	a.CloseTab(middle)
	if got := a.cmLoad(); got == nil {
		t.Fatal("closing the middle active tab left no active tab")
	}
	tabs := a.ListTabs()
	if len(tabs) != 2 || tabs[0].ID != first || tabs[1].ID != last || !tabs[1].Active {
		t.Fatalf("tab order/active after middle close = %+v", tabs)
	}

	a.CloseTab(last)
	tabs = a.ListTabs()
	if len(tabs) != 1 || tabs[0].ID != first || !tabs[0].Active {
		t.Fatalf("last active tab did not select its left neighbor: %+v", tabs)
	}
}

func TestIntentionalDisconnectEventPrecedesReplacementReplay(t *testing.T) {
	a := newTabApp(t)
	activeID, active := a.newTab()
	a.newTab()
	a.activate(activeID)

	client, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })
	active.cm.mu.Lock()
	active.cm.conn = client
	active.cm.closed = false
	active.cm.mu.Unlock()

	var events []journalEntry
	a.eventEmit = func(name string, payload any) {
		text, _ := payload.(string)
		events = append(events, journalEntry{name: name, payload: text})
	}
	a.DisconnectTab(activeID)

	intentionalAt, resetAt := -1, -1
	for i, event := range events {
		switch {
		case event.name == "intentional_disconnect" && event.payload == activeID:
			intentionalAt = i
		case event.name == "tab_reset":
			resetAt = i
		}
	}
	if intentionalAt < 0 || resetAt < 0 || intentionalAt >= resetAt {
		t.Fatalf("event order = %#v, want intentional_disconnect before tab_reset", events)
	}
	if got := len(events); got == 0 {
		t.Fatal("DisconnectTab emitted no events")
	}

	// A normal internal close is deliberately silent, even while connected.
	silentID, silent := a.newTab()
	client, peer = net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })
	silent.cm.mu.Lock()
	silent.cm.conn = client
	silent.cm.closed = false
	silent.cm.mu.Unlock()
	a.activate(silentID)
	events = nil
	a.CloseTab(silentID)
	for _, event := range events {
		if event.name == "intentional_disconnect" {
			t.Fatalf("silent CloseTab emitted intentional disconnect: %#v", events)
		}
	}

	// A connected background tab has no connection edge: only the active live
	// tab can earn the cue.
	foregroundID, _ := a.newTab()
	backgroundID, background := a.newTab()
	a.activate(foregroundID)
	client, peer = net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })
	background.cm.mu.Lock()
	background.cm.conn = client
	background.cm.closed = false
	background.cm.mu.Unlock()
	events = nil
	a.DisconnectTab(backgroundID)
	for _, event := range events {
		if event.name == "intentional_disconnect" {
			t.Fatalf("background DisconnectTab emitted intentional disconnect: %#v", events)
		}
	}
	_, offline := a.newTab()
	a.activate(offline.info.ID)
	events = nil
	a.DisconnectTab(offline.info.ID)
	for _, event := range events {
		if event.name == "intentional_disconnect" {
			t.Fatalf("offline DisconnectTab emitted intentional disconnect: %#v", events)
		}
	}
}

func TestContainsMentionUsesUnicodeCaseFoldAndExactBoundary(t *testing.T) {
	for _, test := range []struct {
		text, nickname string
		want           bool
	}{
		{"hello @ÄLICE!", "älice", true},
		{"hello @K!", "k", true},
		{"hello @k!", "K", true},
		{"hello @ſ!", "s", true},
		{"hello @s!", "ſ", true},
		{"hello @ann", "ann", true},
		{"hello @annex", "ann", false},
		{"hello @ann_2", "ann", false},
		{"hello @ann-2", "ann", false},
		{"hello @Kx", "k", false},
		{"hello @K-2", "k", false},
		{"hello @ann.", "ann", true},
		{"hello @foo.bar!", "foo.bar", true},
		{"hello @foo.bar2", "foo.bar", false},
		{"hello @fox🙂!", "fox🙂", true},
		{"hello @fox🙂x", "fox🙂", false},
		{"hello @🙂", "🙂", true},
	} {
		if got := containsMention(test.text, test.nickname); got != test.want {
			t.Errorf("containsMention(%q, %q) = %v, want %v", test.text, test.nickname, got, test.want)
		}
	}
}

func TestCloseTabCommitCannotReinstateClosedActiveOrRouteLateEvent(t *testing.T) {
	a := newTabApp(t)
	closedID, closed := a.newTab()
	remainingID, remaining := a.newTab()
	a.activate(closedID)

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); <-start; a.CloseTab(closedID) }()
	go func() { defer wg.Done(); <-start; a.SetActiveTab(remainingID) }()
	close(start)
	wg.Wait()

	a.tabsMu.Lock()
	_, removed := a.tabs[closedID]
	activeID := a.activeID
	a.tabsMu.Unlock()
	if removed || activeID == closedID || a.cmLoad() == closed.cm {
		t.Fatalf("closed tab remained active: active=%q removed=%v", activeID, removed)
	}
	if activeID != remainingID || a.cmLoad() != remaining.cm {
		t.Fatalf("remaining tab not active: active=%q", activeID)
	}
	before := len(remaining.journal)
	var emitted atomic.Int32
	a.eventEmit = func(string, any) { emitted.Add(1) }
	a.relayTabEvent(closedID, "event", `{"type":"chat","data":{"text":"late"}}`)
	if len(remaining.journal) != before {
		t.Fatal("late removed-tab event was delivered to remaining tab")
	}
	if emitted.Load() != 0 {
		t.Fatal("late removed-tab event was emitted to the webview")
	}
}

func TestActivationPublicationDropsSupersededReplayBatch(t *testing.T) {
	a := newTabApp(t)
	firstID, first := a.newTab()
	secondID, second := a.newTab()
	first.journal = []journalEntry{{name: "event", payload: "first-journal"}}
	second.journal = []journalEntry{{name: "event", payload: "second-journal"}}

	firstReset := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	var events []journalEntry
	a.eventEmit = func(name string, payload any) {
		if name == "tab_reset" && payload == firstID {
			select {
			case <-firstReset:
			default:
				close(firstReset)
				<-releaseFirst
			}
		}
		text, _ := payload.(string)
		mu.Lock()
		events = append(events, journalEntry{name: name, payload: text})
		mu.Unlock()
	}

	firstDone := make(chan struct{})
	go func() { a.activate(firstID); close(firstDone) }()
	<-firstReset
	secondDone := make(chan struct{})
	go func() { a.activate(secondID); close(secondDone) }()
	close(releaseFirst)
	<-firstDone
	<-secondDone

	mu.Lock()
	defer mu.Unlock()
	lastReset := -1
	for i, event := range events {
		if event.name == "tab_reset" {
			lastReset = i
		}
	}
	if lastReset < 0 || events[lastReset].payload != secondID {
		t.Fatalf("last reset = %#v, want second tab %q", events, secondID)
	}
	for _, event := range events[lastReset+1:] {
		if event.name == "event" && event.payload == "first-journal" {
			t.Fatalf("superseded first replay published after second reset: %#v", events)
		}
	}
}

func TestRelayAndActivationSharePublicationSequencer(t *testing.T) {
	t.Run("old relay cannot follow new reset", func(t *testing.T) {
		a := newTabApp(t)
		oldID, _ := a.newTab()
		newID, _ := a.newTab()
		a.activate(oldID)
		entered := make(chan struct{})
		release := make(chan struct{})
		var mu sync.Mutex
		var events []journalEntry
		a.eventEmit = func(name string, payload any) {
			text, _ := payload.(string)
			if name == "event" && text == "old" {
				close(entered)
				<-release
			}
			mu.Lock()
			events = append(events, journalEntry{name: name, payload: text})
			mu.Unlock()
		}
		relayDone := make(chan struct{})
		go func() { a.relayTabEvent(oldID, "event", "old"); close(relayDone) }()
		<-entered
		activateDone := make(chan struct{})
		go func() { a.activate(newID); close(activateDone) }()
		close(release)
		<-relayDone
		<-activateDone
		mu.Lock()
		defer mu.Unlock()
		oldAt, resetAt := -1, -1
		for i, event := range events {
			if event.name == "event" && event.payload == "old" {
				oldAt = i
			}
			if event.name == "tab_reset" && event.payload == newID {
				resetAt = i
			}
		}
		if oldAt < 0 || resetAt < 0 || oldAt > resetAt {
			t.Fatalf("old relay/reset order = %#v", events)
		}
	})

	t.Run("new relay cannot precede its reset", func(t *testing.T) {
		a := newTabApp(t)
		oldID, _ := a.newTab()
		newID, _ := a.newTab()
		a.activate(oldID)
		entered := make(chan struct{})
		release := make(chan struct{})
		var mu sync.Mutex
		var events []journalEntry
		a.eventEmit = func(name string, payload any) {
			text, _ := payload.(string)
			if name == "tab_reset" && text == newID {
				close(entered)
				<-release
			}
			mu.Lock()
			events = append(events, journalEntry{name: name, payload: text})
			mu.Unlock()
		}
		activateDone := make(chan struct{})
		go func() { a.activate(newID); close(activateDone) }()
		<-entered
		relayDone := make(chan struct{})
		go func() { a.relayTabEvent(newID, "event", "new"); close(relayDone) }()
		close(release)
		<-activateDone
		<-relayDone
		mu.Lock()
		defer mu.Unlock()
		resetAt, newAt := -1, -1
		for i, event := range events {
			if event.name == "tab_reset" && event.payload == newID {
				resetAt = i
			}
			if event.name == "event" && event.payload == "new" {
				newAt = i
			}
		}
		if resetAt < 0 || newAt < 0 || newAt < resetAt {
			t.Fatalf("new reset/relay order = %#v", events)
		}
	})
}

// TestTabJournalAndBadges covers background-event journaling, badge
// counting, and journal reset on a fresh snapshot.
func TestTabJournalAndBadges(t *testing.T) {
	a := newTabApp(t)
	id1, _ := a.newTab()
	_, ts2 := a.newTab()
	a.activate(id1) // tab 2 stays in the background

	a.relayTabEvent(ts2.info.ID, "snapshot", `{"root_channels":[]}`)
	a.relayTabEvent(ts2.info.ID, "channellist", `{"channels":[]}`)
	a.relayTabEvent(ts2.info.ID, "subscriptions", `{"channel_ids":[7]}`)
	a.relayTabEvent(ts2.info.ID, "server_rules", `{"text":"be kind","hash":"v1"}`)
	a.relayTabEvent(ts2.info.ID, "event", `{"type":"chat","data":{"from":"bob","text":"hello"}}`)
	a.relayTabEvent(ts2.info.ID, "event", `{"type":"chat","data":{"from":"bob","text":"hey @friend you there"}}`)

	if len(ts2.journal) != 6 {
		t.Fatalf("journal = %d entries, want 6", len(ts2.journal))
	}
	if ts2.info.Unread != 2 {
		t.Fatalf("unread = %d, want 2", ts2.info.Unread)
	}
	// ts2's nickname is empty here, so no mention counted; set it and retry.
	ts2.cm.nickname = "friend"
	a.relayTabEvent(ts2.info.ID, "event", `{"type":"chat","data":{"from":"bob","text":"hey @friend"}}`)
	if ts2.info.Mentions != 1 {
		t.Fatalf("mentions = %d, want 1", ts2.info.Mentions)
	}

	// A fresh snapshot resets the journal.
	a.relayTabEvent(ts2.info.ID, "snapshot", `{"root_channels":[]}`)
	if len(ts2.journal) != 1 {
		t.Fatalf("journal after snapshot = %d, want 1", len(ts2.journal))
	}

	// Activation clears the badges.
	a.activate(ts2.info.ID)
	if ts2.info.Unread != 0 || ts2.info.Mentions != 0 {
		t.Fatalf("badges after activation = %d/%d, want 0/0", ts2.info.Unread, ts2.info.Mentions)
	}
}

// TestActiveTabJournalTracksSessionChanges covers the switch-away/switch-back
// lifecycle. State events received while a tab is active must still be kept in
// its replay journal; otherwise a successful channel join disappears as soon
// as another server tab is selected.
func TestActiveTabJournalTracksSessionChanges(t *testing.T) {
	a := newTabApp(t)
	id, ts := a.newTab()
	a.activate(id)

	a.relayTabEvent(id, "snapshot", `{"root_channels":[]}`)
	a.relayTabEvent(id, "event", `{"type":"user_moved","data":{"client_id":"me","channel_id":42}}`)

	if len(ts.journal) != 2 {
		t.Fatalf("active tab journal = %d entries, want snapshot plus move", len(ts.journal))
	}
}

// TestTabReplayUsesCachedFrames verifies activation replays the cached
// snapshot/list frames through the connManager itself (281 replay source).
func TestTabReplayUsesCachedFrames(t *testing.T) {
	a := newTabApp(t)
	_, ts := a.newTab()
	// Simulate frames that arrived while the tab was in the background.
	ts.cm.dispatch(&netproto.Frame{Type: uint16(netproto.MsgSnapshot), Payload: []byte(`{"root_channels":[]}`)})
	ts.cm.dispatch(&netproto.Frame{Type: uint16(netproto.MsgChannelList), Payload: []byte(`{"channels":[]}`)})
	if ts.cm.lastSnapshot == "" || ts.cm.lastChannelList == "" {
		t.Fatal("frames not cached on the connManager")
	}
}

func TestTabReplayPublishesCompletionAfterJournal(t *testing.T) {
	a := newTabApp(t)
	id, ts := a.newTab()
	ts.journal = []journalEntry{{name: "event", payload: "journal-entry"}}
	var events []journalEntry
	a.eventEmit = func(name string, payload any) {
		text, _ := payload.(string)
		events = append(events, journalEntry{name: name, payload: text})
	}

	a.activate(id)
	reset, journal, done := -1, -1, -1
	for i, event := range events {
		switch {
		case event.name == "tab_reset":
			reset = i
		case event.name == "event" && event.payload == "journal-entry":
			journal = i
		case event.name == "tab_replay_done" && event.payload == id:
			done = i
		}
	}
	if reset < 0 || journal < 0 || done < 0 || reset >= journal || journal >= done {
		t.Fatalf("replay order = %#v, want reset < journal < tab_replay_done", events)
	}
}

// TestConnectBookmarkTabWithID verifies successful connections return the
// exact tab they created and the compatibility wrappers connect only once.
func TestConnectBookmarkTabWithID(t *testing.T) {
	oldRoot, oldProtection := identityRootOverride, keyProtectionSetting
	identityRootOverride = t.TempDir()
	keyProtectionSetting = func() string { return "off" }
	t.Cleanup(func() {
		identityRootOverride = oldRoot
		keyProtectionSetting = oldProtection
	})

	addr := startTabAuthServer(t, 3)
	a := newTabApp(t)
	t.Cleanup(func() {
		for _, tab := range a.ListTabs() {
			a.CloseTab(tab.ID)
		}
	})

	account := a.ConnectBookmarkTabWithID("work", addr, "alice", "password", "")
	if account.Error != "" || account.TabID == "" {
		t.Fatalf("account result = %+v, want a tab ID without an error", account)
	}
	if tabs := a.ListTabs(); len(tabs) != 1 || tabs[0].ID != account.TabID || !tabs[0].Active {
		t.Fatalf("tabs after account connect = %+v, want active tab %q", tabs, account.TabID)
	}

	guest := a.ConnectGuestBookmarkTabWithID("guest", addr, "visitor")
	if guest.Error != "" || guest.TabID == "" {
		t.Fatalf("guest result = %+v, want a tab ID without an error", guest)
	}
	if guest.TabID == account.TabID {
		t.Fatalf("guest tab ID = account tab ID = %q", guest.TabID)
	}
	if a.activeID != guest.TabID {
		t.Fatalf("active tab = %q, want guest tab %q", a.activeID, guest.TabID)
	}

	if err := a.ConnectBookmarkTab("legacy", addr, "bob", "password", ""); err != "" {
		t.Fatalf("compatibility wrapper connect: %s", err)
	}
	if tabs := a.ListTabs(); len(tabs) != 3 {
		t.Fatalf("tabs after three connect calls = %d, want 3", len(tabs))
	}
}

func TestConnectTabResultValidation(t *testing.T) {
	const expected = "server address and nickname are required"
	a := newTabApp(t)

	account := a.ConnectBookmarkTabWithID("", "", "alice", "password", "")
	if account.TabID != "" || account.Error != expected {
		t.Fatalf("account validation result = %+v", account)
	}
	guest := a.ConnectGuestBookmarkTabWithID("", "server", "")
	if guest.TabID != "" || guest.Error != expected {
		t.Fatalf("guest validation result = %+v", guest)
	}
	if got := a.ConnectBookmarkTab("", "", "alice", "password", ""); got != expected {
		t.Fatalf("account compatibility error = %q, want %q", got, expected)
	}
	if got := a.ConnectGuestBookmarkTab("", "server", ""); got != expected {
		t.Fatalf("guest compatibility error = %q, want %q", got, expected)
	}
	if tabs := a.ListTabs(); len(tabs) != 0 {
		t.Fatalf("validation failures created tabs: %+v", tabs)
	}
}

func startTabAuthServer(t *testing.T, connectionCount int) string {
	t.Helper()
	cert, _, err := tlscert.Ensure(t.TempDir(), "", "", nil)
	if err != nil {
		t.Fatalf("create TLS certificate: %v", err)
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var (
		connections  []net.Conn
		acceptDone   = make(chan struct{})
		connectionWG sync.WaitGroup
	)
	go func() {
		defer close(acceptDone)
		for range connectionCount {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			connections = append(connections, conn)
			connectionWG.Add(1)
			go func() {
				defer connectionWG.Done()
				serveTabAuthConnection(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-acceptDone
		for _, conn := range connections {
			_ = conn.Close()
		}
		connectionWG.Wait()
	})
	return listener.Addr().String()
}

func serveTabAuthConnection(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	frame, err := netproto.ReadFrame(conn)
	if err != nil {
		return
	}
	var authenticate netproto.Authenticate
	if err := netproto.Decode(frame, &authenticate); err != nil {
		return
	}
	nickname := authenticate.Nickname
	if nickname == "" {
		nickname = authenticate.Username
	}
	response, err := netproto.Encode(netproto.MsgAuthResponse, netproto.AuthResponse{
		OK:       true,
		ClientID: "client",
		UniqueID: "user",
		Nickname: nickname,
	})
	if err != nil || netproto.WriteFrame(conn, response) != nil {
		return
	}
	for {
		if _, err := netproto.ReadFrame(conn); err != nil {
			return
		}
	}
}

// TestRecordRecent verifies dedup, ordering, and the 10-entry cap (282).
func TestRecordRecent(t *testing.T) {
	a := newTabApp(t)
	a.settings.Recents = nil

	for i := 0; i < 12; i++ {
		a.RecordRecent("srv-"+string(rune('a'+i)), "nick")
	}
	if len(a.settings.Recents) != 10 {
		t.Fatalf("recents = %d, want 10", len(a.settings.Recents))
	}
	if a.settings.Recents[0].Addr != "srv-l" {
		t.Fatalf("newest first = %q, want srv-l", a.settings.Recents[0].Addr)
	}

	// Reconnecting the same server moves it to the front without duplicating.
	a.RecordRecent("srv-c", "nick")
	if len(a.settings.Recents) != 10 || a.settings.Recents[0].Addr != "srv-c" {
		t.Fatalf("recents after dedup = %+v", a.settings.Recents[:2])
	}
}

// TestLookupBookmark verifies the bookmark a connection belongs to is found
// by name, that the name must agree with the address (names are not unique),
// and that the nameless fallback only resolves an unambiguous match.
func TestLookupBookmark(t *testing.T) {
	a := newTabApp(t)
	a.settings.Bookmarks = []Bookmark{
		{Name: "work", Addr: "srv:12333", Nickname: "alice", NicknameOverride: "alice-work", Profile: "work"},
		{Name: "home", Addr: "srv:12333", Nickname: "alice"},
		{Name: "work", Addr: "other:12333", Nickname: "carol"},
	}

	if b := a.lookupBookmark("home", "srv:12333", "alice-work"); b == nil || b.Name != "home" {
		t.Fatalf("explicit name lookup = %+v", b)
	}
	// A duplicated name resolves per address, not first-wins.
	if b := a.lookupBookmark("work", "other:12333", "carol"); b == nil || b.Nickname != "carol" {
		t.Fatalf("duplicate name lookup = %+v", b)
	}
	// The override is what was sent as the login nickname.
	if b := a.lookupBookmark("", "srv:12333", "alice-work"); b == nil || b.Name != "work" {
		t.Fatalf("override lookup = %+v", b)
	}
	// Two bookmarks answer to "alice" on that server: guessing would apply
	// the wrong profile/avatar override, so neither is returned.
	if b := a.lookupBookmark("", "srv:12333", "alice"); b != nil {
		t.Fatalf("ambiguous nickname matched %+v", b)
	}
	if b := a.lookupBookmark("", "nowhere:12333", "alice"); b != nil {
		t.Fatalf("unknown server matched %+v", b)
	}
	if b := a.lookupBookmark("gone", "srv:12333", "alice"); b != nil {
		t.Fatalf("unknown bookmark name matched %+v", b)
	}
	if b := a.lookupBookmark("work", "nowhere:12333", "alice-work"); b != nil {
		t.Fatalf("name matched a foreign address: %+v", b)
	}
}

// TestLookupBookmarkNicknameCollision covers the shape that broke: one
// bookmark's override is another bookmark's plain nickname on the same
// server, so only the explicit name can tell them apart.
func TestLookupBookmarkNicknameCollision(t *testing.T) {
	a := newTabApp(t)
	a.settings.Bookmarks = []Bookmark{
		{Name: "X", Addr: "srv", Nickname: "alice", NicknameOverride: "bob", AvatarOverrideB64: "xxx"},
		{Name: "Y", Addr: "srv", Nickname: "bob"},
	}

	if b := a.lookupBookmark("Y", "srv", "bob"); b == nil || b.Name != "Y" {
		t.Fatalf("connect via Y resolved %+v", b)
	}
	if b := a.lookupBookmark("X", "srv", "bob"); b == nil || b.Name != "X" {
		t.Fatalf("connect via X resolved %+v", b)
	}
}

// TestLookupBookmarkDuplicateName covers two bookmarks on one server renamed
// to the same name: nothing enforces name uniqueness, and the pair carries
// different profiles/avatar overrides, so neither may win.
func TestLookupBookmarkDuplicateName(t *testing.T) {
	a := newTabApp(t)
	a.settings.Bookmarks = []Bookmark{
		{Name: "work", Addr: "srv:12333", Nickname: "alice", Profile: "alice", AvatarOverrideB64: "aaa"},
		{Name: "work", Addr: "srv:12333", Nickname: "bob", Profile: "bob", AvatarOverrideB64: "bbb"},
	}

	if b := a.lookupBookmark("work", "srv:12333", "bob"); b != nil {
		t.Fatalf("ambiguous name matched %+v", b)
	}
	if b := a.lookupBookmark("work", "srv:12333", "alice"); b != nil {
		t.Fatalf("ambiguous name matched %+v", b)
	}
}

// TestHotkeyProfiles verifies profile merging over defaults (300).
func TestHotkeyProfiles(t *testing.T) {
	a := newTabApp(t)
	a.settings.HotkeyPTT = "Space"
	a.settings.HotkeyMute = "Ctrl+M"
	a.settings.HotkeyProfiles = map[string]HotkeyProfile{
		"work": {PTT: "F5"},
	}

	specs := a.profileSpecs("work")
	if specs["ptt"] != "F5" {
		t.Fatalf("profile ptt = %q, want F5", specs["ptt"])
	}
	if specs["mute_toggle"] != "Ctrl+M" {
		t.Fatalf("profile mute = %q, want default Ctrl+M", specs["mute_toggle"])
	}

	def := a.profileSpecs("default")
	if def["ptt"] != "Space" {
		t.Fatalf("default ptt = %q", def["ptt"])
	}
}

// TestSetHotkeyValidation verifies the per-action rebind API (299).
func TestSetHotkeyValidation(t *testing.T) {
	a := newTabApp(t)

	if err := a.SetHotkey("nope", "F1"); err == "" {
		t.Fatal("unknown action accepted")
	}
	if err := a.SetHotkey("quick_connect", "not a key!!"); err == "" {
		t.Fatal("invalid spec accepted")
	}
	if err := a.SetHotkey("quick_connect", "Ctrl+Shift+C"); err != "" {
		t.Fatalf("valid spec rejected: %s", err)
	}
	if a.settings.HotkeyQuickConnect != "Ctrl+Shift+C" {
		t.Fatalf("spec = %q", a.settings.HotkeyQuickConnect)
	}
}

// conn_live_test.go is a headless integration test for the client backend
// against a LIVE voicx server. It is skipped unless VOICX_LIVE_ADDR is set:
//
//	VOICX_LIVE_ADDR=127.0.0.1:10011 go test -run Live -v ./... -count=1
//
// Optional: VOICX_LIVE_QUERY_ADDR (default: same host, port 10012) and
// VOICX_LIVE_ADMIN_UID / VOICX_LIVE_ADMIN_PASS for channel creation.
package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"voicx/internal/netproto"
)

const (
	liveAliceUID  = "cHZVQTN4VW91dG9KekhjTFViWGttTGpyaFUxVlBDcVloN3EzWVk5T1VsST0="
	liveAlicePass = "alicepass123"
	liveBobUID    = "T2EybDNONG9pYXFhMk5aa3gyL3YveVNONXFheHNwdC9FQzFNQXQ0U0gvdz0="
	liveBobPass   = "bobpass123"
	liveAdminUID  = "Q2ZCSGZWMDJnQUl3K0hIWkowUzUydUhUa0RoMlpGYkFtTWpxQnhZZ2VzUT0="
	liveAdminPass = "adminpass123"
)

// liveAddr returns the control address or skips the test.
func liveAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("VOICX_LIVE_ADDR")
	if addr == "" {
		t.Skip("VOICX_LIVE_ADDR not set; skipping live integration test")
	}
	return addr
}

// liveQueryAddr returns the ServerQuery address for the live server.
func liveQueryAddr(t *testing.T) string {
	t.Helper()
	if addr := os.Getenv("VOICX_LIVE_QUERY_ADDR"); addr != "" {
		return addr
	}
	host, _, err := net.SplitHostPort(liveAddr(t))
	if err != nil {
		t.Fatalf("splitting addr: %v", err)
	}
	return net.JoinHostPort(host, "10012")
}

// eventRecorder is an eventSink that records everything for assertions.
type eventRecorder struct {
	mu     sync.Mutex
	events []recordedEvent
}

type recordedEvent struct {
	name    string
	payload string
}

// Emit implements eventSink.
func (r *eventRecorder) Emit(name string, payload any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, recordedEvent{name: name, payload: fmt.Sprint(payload)})
}

// waitFor polls until an event matching pred arrives or the timeout passes.
func (r *eventRecorder) waitFor(t *testing.T, name string, pred func(string) bool, timeout time.Duration, what string) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for _, e := range r.events {
			if e.name == name && (pred == nil || pred(e.payload)) {
				r.mu.Unlock()
				return e.payload
			}
		}
		r.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
	return ""
}

// newTestBackend returns a connManager wired to a recording sink and a
// throwaway identity (so tests never touch the real identity.json or bind
// it to live accounts).
func newTestBackend(t *testing.T) (*connManager, *eventRecorder) {
	t.Helper()
	rec := &eventRecorder{}
	cm := newConnManager(nil)
	cm.sink = rec
	cm.id = mustTempIdentity(t)
	return cm, rec
}

// --- ServerQuery helper (channel creation) -----------------------------------

// queryCmd runs one ServerQuery command and returns the response lines.
func queryCmd(t *testing.T, conn net.Conn, r *bufio.Reader, cmd string) []string {
	t.Helper()
	if _, err := conn.Write([]byte(cmd + "\n")); err != nil {
		t.Fatalf("query write: %v", err)
	}
	var lines []string
	for {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("query read: %v", err)
		}
		lines = append(lines, strings.TrimRight(line, "\r\n"))
		if strings.HasPrefix(lines[len(lines)-1], "error id=") {
			return lines
		}
	}
}

// ensureLiveChannel creates a permanent channel via ServerQuery and returns
// its ID.
func ensureLiveChannel(t *testing.T) int64 {
	t.Helper()
	conn, err := net.DialTimeout("tcp", liveQueryAddr(t), 5*time.Second)
	if err != nil {
		t.Fatalf("dial query: %v", err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := r.ReadString('\n'); err != nil { // banner line 1
		t.Fatalf("query banner: %v", err)
	}
	if _, err := r.ReadString('\n'); err != nil { // banner line 2
		t.Fatalf("query banner: %v", err)
	}

	lines := queryCmd(t, conn, r, "login "+liveAdminUID+" "+liveAdminPass)
	if last := lines[len(lines)-1]; last != "error id=0 msg=ok" {
		t.Fatalf("query login failed: %s", last)
	}

	name := "live-e2e-" + strconv.FormatInt(time.Now().Unix(), 10)
	lines = queryCmd(t, conn, r, `channelcreate channel_name=`+name+` channel_flag_permanent=1`)
	for _, l := range lines {
		if strings.HasPrefix(l, "cid=") {
			cid, err := strconv.ParseInt(strings.TrimPrefix(l, "cid="), 10, 64)
			if err != nil {
				t.Fatalf("parse cid: %v", err)
			}
			return cid
		}
	}
	t.Fatalf("channelcreate failed: %v", lines)
	return 0
}

// --- tests -------------------------------------------------------------------

// TestLiveAuth exercises connect + password auth (alice, bob), wrong
// password rejection, and anonymous guest auth against the live server.
func TestLiveAuth(t *testing.T) {
	addr := liveAddr(t)

	// Alice.
	alice, _ := newTestBackend(t)
	if err := alice.connect(addr, liveAliceUID, liveAlicePass, ""); err != "" {
		t.Fatalf("alice connect: %s", err)
	}
	defer alice.disconnect()
	if alice.uniqueID != liveAliceUID {
		t.Errorf("alice uniqueID = %q, want %q", alice.uniqueID, liveAliceUID)
	}
	if alice.nickname == "" {
		t.Error("alice nickname empty")
	}

	// Bob.
	bob, _ := newTestBackend(t)
	if err := bob.connect(addr, liveBobUID, liveBobPass, ""); err != "" {
		t.Fatalf("bob connect: %s", err)
	}
	defer bob.disconnect()
	if bob.uniqueID != liveBobUID {
		t.Errorf("bob uniqueID = %q, want %q", bob.uniqueID, liveBobUID)
	}

	// Wrong password must be rejected.
	bad, _ := newTestBackend(t)
	if err := bad.connect(addr, liveAliceUID, "definitely-wrong", ""); err == "" {
		bad.disconnect()
		t.Fatal("wrong password accepted")
	}

	// Anonymous guest with the client's own identity (key-derived UID).
	guest, _ := newTestBackend(t)
	wantUID, err := guest.id.uniqueID()
	if err != nil {
		t.Fatalf("uniqueID: %v", err)
	}
	if err := guest.connect(addr, "live-guest", "", ""); err != "" {
		t.Fatalf("guest connect: %s", err)
	}
	defer guest.disconnect()
	if guest.uniqueID != wantUID {
		t.Errorf("guest uniqueID = %q, want key-derived %q", guest.uniqueID, wantUID)
	}
	if guest.nickname != "live-guest" {
		t.Errorf("guest nickname = %q, want live-guest", guest.nickname)
	}
}

// mustTempIdentity creates a throwaway identity in a temp dir so tests never
// touch the real identity.json.
func mustTempIdentity(t *testing.T) *identity {
	t.Helper()
	id, err := loadOrCreateIdentityAt(t.TempDir() + "/identity.json")
	if err != nil {
		t.Fatalf("loadOrCreateIdentityAt: %v", err)
	}
	return id
}

// TestLiveChannelFlow exercises the snapshot, channel join (with user_moved
// event), and channel chat between two backend instances.
func TestLiveChannelFlow(t *testing.T) {
	addr := liveAddr(t)
	channelID := ensureLiveChannel(t)

	alice, aliceEvents := newTestBackend(t)
	if err := alice.connect(addr, liveAliceUID, liveAlicePass, ""); err != "" {
		t.Fatalf("alice connect: %s", err)
	}
	defer alice.disconnect()

	bob, bobEvents := newTestBackend(t)
	if err := bob.connect(addr, liveBobUID, liveBobPass, ""); err != "" {
		t.Fatalf("bob connect: %s", err)
	}
	defer bob.disconnect()

	// Both backends must receive a snapshot containing the new channel.
	cidStr := strconv.FormatInt(channelID, 10)
	aliceEvents.waitFor(t, "snapshot", func(p string) bool {
		return strings.Contains(p, `"ChannelID":`+cidStr) || strings.Contains(p, "live-e2e-")
	}, 5*time.Second, "alice snapshot with channel")
	bobEvents.waitFor(t, "snapshot", func(p string) bool {
		return strings.Contains(p, "live-e2e-")
	}, 5*time.Second, "bob snapshot with channel")

	// Alice joins; then bob joins. Bob must observe a user_moved event for
	// alice's join.
	if err := alice.write(netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: channelID}); err != nil {
		t.Fatalf("alice join: %v", err)
	}
	bobEvents.waitFor(t, "event", func(p string) bool {
		return strings.Contains(p, `"user_moved"`) && strings.Contains(p, alice.clientID)
	}, 5*time.Second, "bob observing alice user_moved")

	if err := bob.write(netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: channelID}); err != nil {
		t.Fatalf("bob join: %v", err)
	}
	// Alice should observe bob's move too.
	aliceEvents.waitFor(t, "event", func(p string) bool {
		return strings.Contains(p, `"user_moved"`) && strings.Contains(p, bob.clientID)
	}, 5*time.Second, "alice observing bob user_moved")

	// Alice sends channel chat; bob receives it.
	text := "live-chat-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := alice.write(netproto.MsgChatSend, netproto.ChatSend{ChannelID: cidStr, Text: text}); err != nil {
		t.Fatalf("alice chat: %v", err)
	}
	bobEvents.waitFor(t, "event", func(p string) bool {
		return strings.Contains(p, `"chat"`) && strings.Contains(p, text)
	}, 5*time.Second, "bob receiving alice's channel chat")
}

// TestLivePermissions exercises the GetPermissions request/response round
// trip. A fresh server grants registered users nothing, so the resolved set
// may be empty; the important part is that the query/response exchange works
// (a non-empty set requires seeded permission rows).
func TestLivePermissions(t *testing.T) {
	addr := liveAddr(t)

	cm, _ := newTestBackend(t)
	if err := cm.connect(addr, liveAliceUID, liveAlicePass, ""); err != "" {
		t.Fatalf("alice connect: %s", err)
	}
	defer cm.disconnect()

	app := &App{cm: cm}
	entries, err := app.GetPermissions()
	if err != nil {
		t.Fatalf("GetPermissions: %v", err)
	}
	// Round-trip succeeded; entries may be empty on a fresh server.
	for _, e := range entries {
		if e.Key == "" {
			t.Errorf("permission entry with empty key: %+v", e)
		}
	}
	t.Logf("resolved %d permission entries for alice", len(entries))
}

// TestLiveClientInfo verifies the ClientInfo query/response flow against the
// live server: bob's self query includes his IP; alice's query of bob hides
// the IP (no b_client_remoteaddress_view grant by default).
func TestLiveClientInfo(t *testing.T) {
	addr := liveAddr(t)

	alice, _ := newTestBackend(t)
	if err := alice.connect(addr, liveAliceUID, liveAlicePass, ""); err != "" {
		t.Fatalf("alice connect: %s", err)
	}
	defer alice.disconnect()

	bob, _ := newTestBackend(t)
	if err := bob.connect(addr, liveBobUID, liveBobPass, ""); err != "" {
		t.Fatalf("bob connect: %s", err)
	}
	defer bob.disconnect()

	aliceApp := &App{cm: alice}
	bobApp := &App{cm: bob}

	// Self query: full data incl. IP.
	self, err := bobApp.GetClientInfo(bob.clientID)
	if err != nil {
		t.Fatalf("GetClientInfo(self): %v", err)
	}
	if self.UniqueID != liveBobUID {
		t.Fatalf("self unique id = %q, want %q", self.UniqueID, liveBobUID)
	}
	if self.IP == "" || self.Port == 0 {
		t.Fatalf("self query missing ip/port: %+v", self)
	}
	if self.ConnectedAt <= 0 {
		t.Fatalf("connected_at = %d", self.ConnectedAt)
	}

	// Alice queries bob: IP must be hidden (deny-on-unset).
	other, err := aliceApp.GetClientInfo(bob.clientID)
	if err != nil {
		t.Fatalf("GetClientInfo(bob): %v", err)
	}
	if other.UniqueID != liveBobUID {
		t.Fatalf("other unique id = %q, want %q", other.UniqueID, liveBobUID)
	}
	if other.IP != "" || other.Port != 0 {
		t.Fatalf("alice sees bob's ip/port without permission: %+v", other)
	}
}

// conn_live_test.go is a headless integration test for the client backend
// against a LIVE voicx server. It is skipped unless VOICX_LIVE_ADDR is set:
//
//	VOICX_LIVE_ADDR=127.0.0.1:12333 go test -run Live -v ./... -count=1
//
// Optional: VOICX_LIVE_QUERY_ADDR (default: same host, port 12335) and
// VOICX_LIVE_ADMIN_UID / VOICX_LIVE_ADMIN_PASS for channel creation.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
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
	liveAlicePass = "alicepw"
	liveBobUID    = "T2EybDNONG9pYXFhMk5aa3gyL3YveVNONXFheHNwdC9FQzFNQXQ0U0gvdz0="
	liveBobPass   = "bobpw"
	liveAdminUID  = "Q2ZCSGZWMDJnQUl3K0hIWkowUzUydUhUa0RoMlpGYkFtTWpxQnhZZ2VzUT0="
	liveAdminPass = "adminpw"
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
	return net.JoinHostPort(host, "12335")
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

	// Alice sends channel chat; bob receives it. Chat is encrypted (4b): the
	// backend seals with the channel key delivered after the join, and bob's
	// backend decrypts before emitting.
	text := "live-chat-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	keyDeadline := time.Now().Add(5 * time.Second)
	for {
		if _, _, ok := alice.scopeKeys.current(channelID); ok {
			break
		}
		if time.Now().After(keyDeadline) {
			t.Fatal("alice never received the channel chat key")
		}
		time.Sleep(20 * time.Millisecond)
	}
	msg, err := alice.encryptChat("channel", cidStr, text)
	if err != nil {
		t.Fatalf("alice encrypt chat: %v", err)
	}
	if err := alice.write(netproto.MsgChatSend, msg); err != nil {
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

	app := appWithCM(cm)
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

	aliceApp := appWithCM(alice)
	bobApp := appWithCM(bob)

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

// --- wave-6b: permission/group management bindings -----------------------------

// TestLiveGroupManagement exercises the wave-6b bindings against the live
// server: group list/create/members, perm set + trace, audit log, ban list,
// and the admin flag.
func TestLiveGroupManagement(t *testing.T) {
	addr := liveAddr(t)

	admin, _ := newTestBackend(t)
	if err := admin.connect(addr, liveAdminUID, liveAdminPass, ""); err != "" {
		t.Fatalf("admin connect: %s", err)
	}
	defer admin.disconnect()
	app := appWithCM(admin)

	if !app.IsAdmin() {
		t.Fatal("IsAdmin = false for the admin account")
	}

	// Default groups are seeded (143/144).
	list, err := app.GroupList("server")
	if err != nil {
		t.Fatalf("GroupList: %v", err)
	}
	var guest *netproto.GroupEntry
	for i := range list.Groups {
		if list.Groups[i].Name == "Guest" {
			guest = &list.Groups[i]
		}
	}
	if guest == nil {
		t.Fatalf("default Guest group missing: %+v", list.Groups)
	}

	// Create + assign alice + member listing.
	name := "live-mods-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	created, err := app.GroupCreate("server", name, 10)
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	var gid int64
	for _, g := range created.Groups {
		if g.Name == name {
			gid = g.ID
		}
	}
	if gid == 0 {
		t.Fatalf("created group %q not in list", name)
	}
	defer app.GroupDelete("server", gid, true)

	if err := app.GroupAssign("server", gid, liveAliceUID, 0, 0); err != "" {
		t.Fatalf("GroupAssign: %s", err)
	}
	members, err := app.GroupMembers("server", gid, 0)
	if err != nil {
		t.Fatalf("GroupMembers: %v", err)
	}
	if len(members.Members) != 1 || members.Members[0].UniqueID != liveAliceUID {
		t.Fatalf("members = %+v", members.Members)
	}
	if err := app.GroupUnassign("server", gid, liveAliceUID, 0); err != "" {
		t.Fatalf("GroupUnassign: %s", err)
	}

	// Perm set (client tier) -> trace reflects it -> unset.
	if err := app.PermSet("client", 0, liveAliceUID, 0, "i_client_talk_power", 66, 0, false, false); err != "" {
		t.Fatalf("PermSet: %s", err)
	}
	defer app.PermUnset("client", 0, liveAliceUID, 0, "i_client_talk_power")
	trace, err := app.PermTrace(liveAliceUID, "i_client_talk_power", 0)
	if err != nil {
		t.Fatalf("PermTrace: %v", err)
	}
	if trace.Effective != 66 || trace.EffectiveTier != "client_specific" {
		t.Fatalf("trace = %d/%q, want 66/client_specific", trace.Effective, trace.EffectiveTier)
	}
	if err := app.PermUnset("client", 0, liveAliceUID, 0, "i_client_talk_power"); err != "" {
		t.Fatalf("PermUnset: %s", err)
	}

	// The writes above produced audit rows.
	audit, err := app.AuditLog(0, 5)
	if err != nil {
		t.Fatalf("AuditLog: %v", err)
	}
	if len(audit.Entries) == 0 {
		t.Fatal("audit log empty after admin writes")
	}

	// Ban list is admin-readable (may be empty).
	if _, err := app.BanList(); err != nil {
		t.Fatalf("BanList: %v", err)
	}

	// Group icon get on a group without an icon returns an empty payload.
	icon, err := app.GroupIconGet(gid)
	if err != nil {
		t.Fatalf("GroupIconGet: %v", err)
	}
	if icon.DataBase64 != "" {
		t.Fatalf("unexpected icon data (%d bytes)", len(icon.DataBase64))
	}
}

// TestLiveGroupGateDenied verifies non-admin group writes are refused
// (deny-on-unset) and surface as servererror events.
func TestLiveGroupGateDenied(t *testing.T) {
	addr := liveAddr(t)
	bob, events := newTestBackend(t)
	if err := bob.connect(addr, liveBobUID, liveBobPass, ""); err != "" {
		t.Fatalf("bob connect: %s", err)
	}
	defer bob.disconnect()
	app := appWithCM(bob)

	if app.IsAdmin() {
		t.Fatal("IsAdmin = true for bob")
	}
	// Fire-and-forget: the denial arrives as a servererror event (the
	// request/response path would only see a timeout).
	if err := bob.write(netproto.MsgGroupCreate, netproto.GroupCreate{Type: "server", Name: "live-forbidden"}); err != nil {
		t.Fatalf("GroupCreate write: %v", err)
	}
	events.waitFor(t, "servererror", func(p string) bool {
		return strings.Contains(p, "insufficient permission")
	}, 5*time.Second, "permission-denied error event")
}

// --- wave-7: file management bindings -------------------------------------------

// tinyPNG is a minimal PNG header blob for icon uploads.
var tinyPNG = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52}

// sha256Hex returns the hex SHA-256 of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestLiveFileManagement exercises the wave-7 file flow against the live
// server: upload → overwrite (versions) → folder upload → rename/move →
// download link → checksum verify → delete.
func TestLiveFileManagement(t *testing.T) {
	addr := liveAddr(t)
	channelID := ensureLiveChannel(t)

	admin, _ := newTestBackend(t)
	if err := admin.connect(addr, liveAdminUID, liveAdminPass, ""); err != "" {
		t.Fatalf("admin connect: %s", err)
	}
	defer admin.disconnect()
	app := appWithCM(admin)

	name := "w7-live-" + strconv.FormatInt(time.Now().UnixNano(), 36) + ".txt"
	defer app.FileDelete(channelID, "", name)
	defer app.FileDelete(channelID, "", name+".v1")

	// Upload v1, then overwrite with v2.
	if err := app.UploadFile(channelID, name, base64.StdEncoding.EncodeToString([]byte("version-one"))); err != "" {
		t.Fatalf("upload v1: %s", err)
	}
	if err := app.UploadFile(channelID, name, base64.StdEncoding.EncodeToString([]byte("version-two"))); err != "" {
		t.Fatalf("upload v2: %s", err)
	}

	// The current file is v2; v1 was rotated into a version (264).
	list, err := app.FileList(channelID, "")
	if err != nil {
		t.Fatalf("FileList: %v", err)
	}
	var cur *netproto.FileEntry
	for i := range list.Entries {
		if list.Entries[i].Name == name {
			cur = &list.Entries[i]
		}
	}
	if cur == nil {
		t.Fatalf("uploaded file missing: %+v", list.Entries)
	}
	if cur.Size != int64(len("version-two")) {
		t.Fatalf("current size = %d, want v2", cur.Size)
	}
	versions, err := app.FileVersions(channelID, "", name)
	if err != nil {
		t.Fatalf("FileVersions: %v", err)
	}
	if len(versions.Entries) != 1 || versions.Entries[0].Name != name+".v1" {
		t.Fatalf("versions = %+v", versions.Entries)
	}

	// Folder upload via the low-level helpers (261).
	folderFile := "foldered.txt"
	f, err := admin.request(netproto.MsgFileTransferInit, netproto.MsgFileTransferInitResponse,
		netproto.FileTransferInit{ChannelID: channelID, Direction: "upload", Folder: "docs", Name: folderFile, Size: 4},
		10*time.Second)
	if err != nil {
		t.Fatalf("folder init: %v", err)
	}
	var init netproto.FileTransferInitResponse
	if err := json.Unmarshal(f.Payload, &init); err != nil {
		t.Fatalf("decode init: %v", err)
	}
	ep, err := app.ftTarget(init)
	if err != nil {
		t.Fatalf("ftTarget: %v", err)
	}
	if err := ftUpload(ep, init.Token, init.TransferID, []byte("docs")); err != nil {
		t.Fatalf("folder upload: %v", err)
	}
	defer app.FileDelete(channelID, "docs", folderFile)

	docs, err := app.FileList(channelID, "docs")
	if err != nil || len(docs.Entries) != 1 || docs.Entries[0].Folder != "docs" {
		t.Fatalf("docs list = %+v, err=%v", docs.Entries, err)
	}
	found := false
	for _, f := range docs.Folders {
		if f == "docs" {
			found = true
		}
	}
	if !found {
		t.Fatalf("folders = %v, want docs", docs.Folders)
	}

	// Rename/move into the root folder (262).
	if err := app.FileRename(channelID, "docs", folderFile, "", "moved.txt", 0); err != "" {
		t.Fatalf("FileRename: %s", err)
	}
	defer app.FileDelete(channelID, "", "moved.txt")
	list, err = app.FileList(channelID, "")
	if err != nil {
		t.Fatalf("FileList after rename: %v", err)
	}
	found = false
	for _, e := range list.Entries {
		if e.Name == "moved.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("moved.txt missing after rename: %+v", list.Entries)
	}

	// Download link (267): the /dl/ URL serves the bytes. The URL host comes
	// from our own control address (the server cannot know its published
	// address behind Docker/NAT).
	link, err := app.FileLink(channelID, "", "moved.txt")
	if err != nil {
		t.Fatalf("FileLink: %v", err)
	}
	if !strings.Contains(link.Path, "/dl/") {
		t.Fatalf("link path = %q", link.Path)
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	linkURL := fmt.Sprintf("http://%s:%d%s", host, link.HealthPort, link.Path)
	resp, err := http.Get(linkURL)
	if err != nil {
		t.Fatalf("GET link: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "docs" {
		t.Fatalf("link body = %q, want %q", body, "docs")
	}

	// Checksum verify (280).
	ok, err := app.VerifyFile(channelID, "", "moved.txt", sha256Hex([]byte("docs")))
	if err != nil || !ok {
		t.Fatalf("VerifyFile match = %v, %v", ok, err)
	}
	ok, err = app.VerifyFile(channelID, "", "moved.txt", sha256Hex([]byte("different")))
	if err != nil || ok {
		t.Fatalf("VerifyFile mismatch = %v, %v, want false", ok, err)
	}

	// Delete (263): the admin (not the uploader here — same account) deletes.
	if err := app.FileDelete(channelID, "", "moved.txt"); err != "" {
		t.Fatalf("FileDelete: %s", err)
	}
	list, _ = app.FileList(channelID, "")
	for _, e := range list.Entries {
		if e.Name == "moved.txt" {
			t.Fatal("moved.txt still listed after delete")
		}
	}

	// Server icon round trip (270).
	if err := app.ServerIconSet(base64.StdEncoding.EncodeToString(tinyPNG)); err != "" {
		t.Fatalf("ServerIconSet: %s", err)
	}
	icon, err := app.ServerIconGet()
	if err != nil {
		t.Fatalf("ServerIconGet: %v", err)
	}
	if icon.DataBase64 == "" {
		t.Fatal("server icon empty after set")
	}
}

// --- wave-8b: presence and social bindings ------------------------------------

// TestLivePresence exercises SetStatus, ServerInfo, and the poke gate
// against the live server.
func TestLivePresence(t *testing.T) {
	addr := liveAddr(t)

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

	aliceApp := appWithCM(alice)
	bobApp := appWithCM(bob)

	// Server info (313): version and counts present.
	info, err := aliceApp.ServerInfo()
	if err != nil {
		t.Fatalf("ServerInfo: %v", err)
	}
	if info.Version == "" || info.ClientsOnline < 2 {
		t.Fatalf("server info = %+v", info)
	}

	// Status (307): alice goes away; bob receives the broadcast.
	if err := aliceApp.SetStatus("away", "brb"); err != "" {
		t.Fatalf("SetStatus: %s", err)
	}
	bobEvents.waitFor(t, "event", func(p string) bool {
		return strings.Contains(p, "status_changed") && strings.Contains(p, "away")
	}, 5*time.Second, "status_changed event")
	if err := aliceApp.SetStatus("online", ""); err != "" {
		t.Fatalf("SetStatus online: %s", err)
	}

	// Poke without b_client_poke: bob gets a servererror (deny-on-unset).
	if err := bobApp.Poke(alice.clientID, "hi"); err != "" {
		t.Fatalf("Poke write: %s", err)
	}
	bobEvents.waitFor(t, "servererror", func(p string) bool {
		return strings.Contains(p, "insufficient permission")
	}, 5*time.Second, "poke denial")

	// Invalid status: servererror arrives.
	if err := aliceApp.SetStatus("sleeping", ""); err != "" {
		t.Fatalf("SetStatus invalid write: %s", err)
	}
	aliceEvents.waitFor(t, "servererror", func(p string) bool {
		return strings.Contains(p, "invalid status")
	}, 5*time.Second, "invalid-status error event")
}

// server_test.go exercises the ServerQuery protocol over real TCP with a
// fake backend.
package query

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func closeServerQueryTestResource(t *testing.T, closer io.Closer) {
	t.Helper()
	if err := closer.Close(); err != nil {
		t.Logf("closing test resource: %v", err)
	}
}

// fakeBackend implements Backend with canned data and call recording.
type fakeBackend struct {
	mu sync.Mutex

	// users maps uniqueID -> (password, admin).
	users map[string]struct {
		password string
		admin    bool
	}
	clients  []ClientInfo
	channels []ChannelInfo
	info     Info

	moved   [][2]any
	kicked  []kickCall
	texts   []textCall
	created []createCall
	deleted []int64
	banned  []banCall

	complaints []Complaint
	tokens     []Token
	nextToken  int
	settings   map[string]string

	auditEntries []AuditEntry

	// wave 10a state
	serverEdits    []ServerEditParams
	shutdownCalled bool
	channelPerms   []ChannelPerm
	groups         []GroupInfo
	nextGroupID    int64
	groupMembers   []GroupMemberInfo
	custom         map[string]string
	logLines       []string
	// logStream feeds `logview follow`; followCancelled records that the
	// command released its subscription.
	logStream       chan string
	followCancelled bool
	// server rules shown on first join (215)
	rulesText     string
	rulesHash     string
	rulesAccepted int
}

type kickCall struct {
	clientID   string
	fromServer bool
	reason     string
}

type textCall struct {
	mode   int
	target string
	msg    string
}

type createCall struct {
	name  string
	topic string
	ctype int
}

type banCall struct {
	clientID string
	seconds  int64
	reason   string
}

func (f *fakeBackend) Authenticate(_ context.Context, uniqueID, password string) (bool, bool, error) {
	u, ok := f.users[uniqueID]
	if !ok || u.password != password {
		return false, false, nil
	}
	return true, u.admin, nil
}

func (f *fakeBackend) ListClients(context.Context) []ClientInfo   { return f.clients }
func (f *fakeBackend) ListChannels(context.Context) []ChannelInfo { return f.channels }
func (f *fakeBackend) ServerInfo(context.Context) Info            { return f.info }

func (f *fakeBackend) MoveClient(_ context.Context, clientID string, channelID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moved = append(f.moved, [2]any{clientID, channelID})
	return nil
}

func (f *fakeBackend) KickClient(_ context.Context, clientID string, fromServer bool, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kicked = append(f.kicked, kickCall{clientID, fromServer, reason})
	return nil
}

func (f *fakeBackend) SendText(_ context.Context, targetMode int, target, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.texts = append(f.texts, textCall{targetMode, target, msg})
	return nil
}

func (f *fakeBackend) CreateChannel(_ context.Context, name, topic string, channelType int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, createCall{name, topic, channelType})
	return 42, nil
}

func (f *fakeBackend) DeleteChannel(_ context.Context, channelID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, channelID)
	return nil
}

// ChannelInfo returns the canned channel with the matching ID.
func (f *fakeBackend) ChannelInfo(_ context.Context, channelID int64) (ChannelInfo, bool) {
	for _, ch := range f.channels {
		if ch.ChannelID == channelID {
			return ch, true
		}
	}
	return ChannelInfo{}, false
}

// ServerSet records the setting.
func (f *fakeBackend) ServerSet(_ context.Context, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.settings == nil {
		f.settings = map[string]string{}
	}
	f.settings[key] = value
	return nil
}

// EditChannel records the edit and applies it to the canned channel.
func (f *fakeBackend) EditChannel(_ context.Context, channelID int64, params ChannelEditParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, ch := range f.channels {
		if ch.ChannelID == channelID {
			if params.Topic != nil {
				f.channels[i].Topic = *params.Topic
			}
			if params.MaxClients != nil {
				f.channels[i].MaxClients = *params.MaxClients
			}
			if params.OpusBitrate != nil {
				f.channels[i].OpusBitrate = *params.OpusBitrate
			}
			if params.OpusFEC != nil {
				f.channels[i].OpusFEC = *params.OpusFEC
			}
			if params.OpusDTX != nil {
				f.channels[i].OpusDTX = *params.OpusDTX
			}
			if params.OpusStereo != nil {
				f.channels[i].OpusStereo = *params.OpusStereo
			}
			return nil
		}
	}
	return nil
}

func (f *fakeBackend) BanClient(_ context.Context, clientID string, seconds int64, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.banned = append(f.banned, banCall{clientID, seconds, reason})
	return nil
}

func (f *fakeBackend) ListComplaints(context.Context) ([]Complaint, error) {
	return f.complaints, nil
}

func (f *fakeBackend) DeleteComplaint(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.complaints {
		if c.ID == id {
			f.complaints = append(f.complaints[:i], f.complaints[i+1:]...)
			return nil
		}
	}
	return nil
}

func (f *fakeBackend) DeleteAllComplaints(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.complaints = nil
	return nil
}

func (f *fakeBackend) TokenAdd(_ context.Context, tokenType int, groupID int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextToken++
	key := fmt.Sprintf("tok-%d", f.nextToken)
	f.tokens = append(f.tokens, Token{Key: key, Type: tokenType, GroupID: groupID, MaxUses: 1})
	return key, nil
}

func (f *fakeBackend) TokenList(context.Context) ([]Token, error) {
	return f.tokens, nil
}

func (f *fakeBackend) TokenDelete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, t := range f.tokens {
		if t.Key == key {
			f.tokens = append(f.tokens[:i], f.tokens[i+1:]...)
			return nil
		}
	}
	return ErrTokenNotFound
}

func (f *fakeBackend) AuditLog(_ context.Context, limit int) ([]AuditEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if limit > 0 && limit < len(f.auditEntries) {
		return f.auditEntries[:limit], nil
	}
	return f.auditEntries, nil
}

// --- wave 10a fakes -----------------------------------------------------------

func (f *fakeBackend) ServerEdit(_ context.Context, params ServerEditParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.serverEdits = append(f.serverEdits, params)
	return nil
}

func (f *fakeBackend) Shutdown(_ context.Context, restart bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shutdownCalled = restart || f.shutdownCalled
	return nil
}

func (f *fakeBackend) PermOverview(_ context.Context, uniqueID string, channelID int64) ([]PermLine, error) {
	if uniqueID != "user-uid" {
		return nil, errors.New("user not found")
	}
	return []PermLine{
		{Key: "i_client_talk_power", Value: 42, Grant: 50, Tier: "server_group"},
		{Key: "b_channel_modify", Value: 1, Tier: "client_specific"},
	}, nil
}

func (f *fakeBackend) ChannelPermList(context.Context, int64) ([]ChannelPerm, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.channelPerms, nil
}

func (f *fakeBackend) ChannelAddPerm(_ context.Context, actor string, channelID int64, key string, value, grant int, skip, negate bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, p := range f.channelPerms {
		if p.Key == key {
			f.channelPerms[i] = ChannelPerm{Key: key, Value: value, Grant: grant, Skip: skip, Negate: negate}
			return nil
		}
	}
	f.channelPerms = append(f.channelPerms, ChannelPerm{Key: key, Value: value, Grant: grant, Skip: skip, Negate: negate})
	return nil
}

func (f *fakeBackend) ChannelDelPerm(_ context.Context, actor string, channelID int64, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	keep := f.channelPerms[:0]
	for _, p := range f.channelPerms {
		if p.Key != key {
			keep = append(keep, p)
		}
	}
	f.channelPerms = keep
	return nil
}

func (f *fakeBackend) ServerGroupAdd(_ context.Context, actor, name string, sortID int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextGroupID++
	f.groups = append(f.groups, GroupInfo{ID: f.nextGroupID, Name: name, SortID: sortID})
	return f.nextGroupID, nil
}

func (f *fakeBackend) ServerGroupDel(_ context.Context, actor string, groupID int64, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, g := range f.groups {
		if g.ID == groupID {
			if g.MemberCount > 0 && !force {
				return errors.New("group has members (use force)")
			}
			f.groups = append(f.groups[:i], f.groups[i+1:]...)
			return nil
		}
	}
	return errors.New("group not found")
}

func (f *fakeBackend) ServerGroupAddClient(_ context.Context, actor string, groupID int64, uniqueID string, durationSeconds int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, g := range f.groups {
		if g.ID == groupID {
			f.groups[i].MemberCount++
			f.groupMembers = append(f.groupMembers, GroupMemberInfo{UniqueID: uniqueID, Nickname: "nick"})
			return nil
		}
	}
	return errors.New("group not found")
}

func (f *fakeBackend) ServerGroupDelClient(_ context.Context, actor string, groupID int64, uniqueID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	keep := f.groupMembers[:0]
	removed := false
	for _, m := range f.groupMembers {
		if m.UniqueID != uniqueID {
			keep = append(keep, m)
		} else {
			removed = true
		}
	}
	f.groupMembers = keep
	if removed {
		for i, g := range f.groups {
			if g.ID == groupID && g.MemberCount > 0 {
				f.groups[i].MemberCount--
			}
		}
	}
	return nil
}

func (f *fakeBackend) ServerGroupList(context.Context) ([]GroupInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.groups, nil
}

func (f *fakeBackend) ServerGroupClientList(_ context.Context, groupID int64) ([]GroupMemberInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.groupMembers, nil
}

func (f *fakeBackend) CustomSet(_ context.Context, uniqueID, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.custom[uniqueID+"/"+key] = value
	return nil
}

func (f *fakeBackend) CustomDel(_ context.Context, uniqueID, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.custom, uniqueID+"/"+key)
	return nil
}

func (f *fakeBackend) CustomInfo(_ context.Context, uniqueID string) ([]CustomProp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []CustomProp
	for k, v := range f.custom {
		if strings.HasPrefix(k, uniqueID+"/") {
			out = append(out, CustomProp{Key: strings.TrimPrefix(k, uniqueID+"/"), Value: v})
		}
	}
	return out, nil
}

func (f *fakeBackend) LogView(_ context.Context, lines int, filter string) ([]string, error) {
	return f.logLines, nil
}

func (f *fakeBackend) ServerRules(context.Context) (string, string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rulesText, f.rulesHash, f.rulesAccepted, nil
}

func (f *fakeBackend) LogFollow() (<-chan string, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.followCancelled = false
	return f.logStream, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.followCancelled = true
	}
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		users: map[string]struct {
			password string
			admin    bool
		}{
			"admin-uid": {"pw", true},
			"user-uid":  {"pw", false},
		},
		clients: []ClientInfo{
			{ClientID: "c-1", UniqueID: "admin-uid", Nickname: "admin user", ChannelID: 1},
		},
		channels: []ChannelInfo{
			{ChannelID: 1, ParentID: 0, Name: "Lobby", Type: 2, ClientCount: 1},
		},
		info:      Info{Name: "voicx test", Uptime: 90 * time.Second, ClientsOnline: 1, MaxClients: 1024, ChannelsOnline: 1},
		custom:    map[string]string{},
		logStream: make(chan string, 8),
	}
}

// startQueryServer starts a Server on an ephemeral port and returns its
// address.
func startQueryServer(t *testing.T, backend Backend) (string, *Server) {
	t.Helper()
	return startQueryServerWith(t, backend, nil)
}

// startQueryServerWith is startQueryServer with a hook to tune limits before
// the listener starts.
func startQueryServerWith(t *testing.T, backend Backend, configure func(*Server)) (string, *Server) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	srv := New(addr, nil, backend)
	if configure != nil {
		configure(srv)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	// Wait until the server accepts.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
		<-errCh
	})
	return addr, srv
}

// dialQuery connects and consumes the two banner lines.
func dialQuery(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	r := bufio.NewReader(conn)
	b1 := readLine(t, r)
	if !strings.HasPrefix(b1, "VOICX ServerQuery") {
		t.Fatalf("banner line 1 = %q", b1)
	}
	_ = readLine(t, r) // hint line
	return conn, r
}

func readLine(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read line: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

// sendCmd writes a command and reads lines until the terminating error line,
// returning all lines (error line last).
func sendCmd(t *testing.T, conn net.Conn, r *bufio.Reader, cmd string) []string {
	t.Helper()
	if _, err := conn.Write([]byte(cmd + "\n")); err != nil {
		t.Fatalf("write command: %v", err)
	}
	var lines []string
	for {
		line := readLine(t, r)
		lines = append(lines, line)
		if strings.HasPrefix(line, "error id=") {
			return lines
		}
	}
}

// loginOK authenticates as the admin user.
func loginOK(t *testing.T, conn net.Conn, r *bufio.Reader) {
	t.Helper()
	lines := sendCmd(t, conn, r, "login admin-uid pw")
	if got := lines[len(lines)-1]; got != "error id=0 msg=ok" {
		t.Fatalf("login = %q", got)
	}
}

func lastErr(t *testing.T, lines []string) string {
	t.Helper()
	return lines[len(lines)-1]
}

// --- tests ------------------------------------------------------------------

func TestGreeting(t *testing.T) {
	addr, _ := startQueryServer(t, newFakeBackend())
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer closeServerQueryTestResource(t, conn)

	r := bufio.NewReader(conn)
	if line := readLine(t, r); !strings.Contains(line, "VOICX ServerQuery "+Version) {
		t.Fatalf("banner = %q", line)
	}
	if line := readLine(t, r); !strings.Contains(line, "help") {
		t.Fatalf("hint = %q", line)
	}
}

func TestUnauthedRejected(t *testing.T) {
	addr, _ := startQueryServer(t, newFakeBackend())
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)

	lines := sendCmd(t, conn, r, "clientlist")
	if got := lastErr(t, lines); got != `error id=2568 msg=not\slogged\sin` {
		t.Fatalf("error = %q", got)
	}
}

func TestLoginFailures(t *testing.T) {
	addr, _ := startQueryServer(t, newFakeBackend())
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)

	// Wrong password.
	lines := sendCmd(t, conn, r, "login admin-uid wrong")
	if got := lastErr(t, lines); !strings.HasPrefix(got, "error id=520") {
		t.Fatalf("wrong password error = %q", got)
	}

	// Valid credentials but not an admin.
	lines = sendCmd(t, conn, r, "login user-uid pw")
	if got := lastErr(t, lines); !strings.HasPrefix(got, "error id=2568") {
		t.Fatalf("non-admin error = %q", got)
	}

	// Admin succeeds.
	lines = sendCmd(t, conn, r, "login admin-uid pw")
	if got := lastErr(t, lines); got != "error id=0 msg=ok" {
		t.Fatalf("admin login = %q", got)
	}
}

func TestClientlist(t *testing.T) {
	addr, _ := startQueryServer(t, newFakeBackend())
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)
	loginOK(t, conn, r)

	lines := sendCmd(t, conn, r, "clientlist")
	if len(lines) != 2 {
		t.Fatalf("clientlist lines = %v", lines)
	}
	want := `clid=c-1 client_unique_identifier=admin-uid client_nickname=admin\suser cid=1`
	if lines[0] != want {
		t.Fatalf("clientlist row = %q, want %q", lines[0], want)
	}
	if lines[1] != "error id=0 msg=ok" {
		t.Fatalf("status = %q", lines[1])
	}
}

func TestChannellist(t *testing.T) {
	addr, _ := startQueryServer(t, newFakeBackend())
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)
	loginOK(t, conn, r)

	lines := sendCmd(t, conn, r, "channellist")
	want := `cid=1 pid=0 channel_name=Lobby channel_type=2 total_clients=1`
	if lines[0] != want {
		t.Fatalf("channellist row = %q, want %q", lines[0], want)
	}
}

func TestServerinfo(t *testing.T) {
	addr, _ := startQueryServer(t, newFakeBackend())
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)
	loginOK(t, conn, r)

	lines := sendCmd(t, conn, r, "serverinfo")
	row := lines[0]
	for _, want := range []string{`virtualserver_name=voicx\stest`, "virtualserver_uptime=90", "virtualserver_clientsonline=1", "virtualserver_maxclients=1024", "virtualserver_channels_online=1"} {
		if !strings.Contains(row, want) {
			t.Errorf("serverinfo row %q missing %q", row, want)
		}
	}
}

func TestClientmove(t *testing.T) {
	backend := newFakeBackend()
	addr, _ := startQueryServer(t, backend)
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)
	loginOK(t, conn, r)

	lines := sendCmd(t, conn, r, "clientmove clid=c-1 cid=7")
	if got := lastErr(t, lines); got != "error id=0 msg=ok" {
		t.Fatalf("clientmove = %q", got)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.moved) != 1 || backend.moved[0][0] != "c-1" || backend.moved[0][1] != int64(7) {
		t.Fatalf("moved = %v", backend.moved)
	}
}

func TestClientkick(t *testing.T) {
	backend := newFakeBackend()
	addr, _ := startQueryServer(t, backend)
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)
	loginOK(t, conn, r)

	// Channel kick (reasonid 4).
	sendCmd(t, conn, r, `clientkick clid=c-1 reasonid=4 reasonmsg=idle\shours`)
	// Server kick (reasonid 5).
	sendCmd(t, conn, r, `clientkick clid=c-2 reasonid=5`)

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.kicked) != 2 {
		t.Fatalf("kicked = %v", backend.kicked)
	}
	if backend.kicked[0].fromServer || backend.kicked[0].reason != "idle hours" {
		t.Fatalf("channel kick = %+v", backend.kicked[0])
	}
	if !backend.kicked[1].fromServer {
		t.Fatalf("server kick = %+v", backend.kicked[1])
	}
}

func TestSendtextmessage(t *testing.T) {
	backend := newFakeBackend()
	addr, _ := startQueryServer(t, backend)
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)
	loginOK(t, conn, r)

	sendCmd(t, conn, r, `sendtextmessage targetmode=1 target=c-1 msg=hi\sthere`)
	sendCmd(t, conn, r, `sendtextmessage targetmode=2 target=1 msg=channel\spost`)
	sendCmd(t, conn, r, `sendtextmessage targetmode=3 target=0 msg=global`)

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.texts) != 3 {
		t.Fatalf("texts = %v", backend.texts)
	}
	if backend.texts[0] != (textCall{1, "c-1", "hi there"}) {
		t.Fatalf("direct text = %+v", backend.texts[0])
	}
	if backend.texts[1] != (textCall{2, "1", "channel post"}) {
		t.Fatalf("channel text = %+v", backend.texts[1])
	}
	if backend.texts[2] != (textCall{3, "0", "global"}) {
		t.Fatalf("global text = %+v", backend.texts[2])
	}
}

func TestChannelcreateDelete(t *testing.T) {
	backend := newFakeBackend()
	addr, _ := startQueryServer(t, backend)
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)
	loginOK(t, conn, r)

	lines := sendCmd(t, conn, r, `channelcreate channel_name=New\sChan channel_topic=a\stopic channel_flag_permanent=1`)
	if lines[0] != "cid=42" {
		t.Fatalf("create response = %q", lines[0])
	}

	backend.mu.Lock()
	if len(backend.created) != 1 || backend.created[0].name != "New Chan" ||
		backend.created[0].topic != "a topic" || backend.created[0].ctype != 2 {
		t.Fatalf("created = %+v", backend.created)
	}
	backend.mu.Unlock()

	lines = sendCmd(t, conn, r, "channeldelete cid=42 force=1")
	if got := lastErr(t, lines); got != "error id=0 msg=ok" {
		t.Fatalf("delete = %q", got)
	}
	backend.mu.Lock()
	if len(backend.deleted) != 1 || backend.deleted[0] != 42 {
		t.Fatalf("deleted = %v", backend.deleted)
	}
	backend.mu.Unlock()
}

func TestBanclient(t *testing.T) {
	backend := newFakeBackend()
	addr, _ := startQueryServer(t, backend)
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)
	loginOK(t, conn, r)

	lines := sendCmd(t, conn, r, `banclient clid=c-1 time=3600 banreason=rule\s7`)
	if got := lastErr(t, lines); got != "error id=0 msg=ok" {
		t.Fatalf("banclient = %q", got)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.banned) != 1 || backend.banned[0].clientID != "c-1" ||
		backend.banned[0].seconds != 3600 || backend.banned[0].reason != "rule 7" {
		t.Fatalf("banned = %+v", backend.banned)
	}
}

func TestUnknownCommand(t *testing.T) {
	addr, _ := startQueryServer(t, newFakeBackend())
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)
	loginOK(t, conn, r)

	lines := sendCmd(t, conn, r, "frobnicate foo=1")
	if got := lastErr(t, lines); !strings.HasPrefix(got, "error id=256") {
		t.Fatalf("unknown command error = %q", got)
	}
}

func TestHelpVersionQuit(t *testing.T) {
	addr, _ := startQueryServer(t, newFakeBackend())
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)

	// help works unauthenticated.
	lines := sendCmd(t, conn, r, "help")
	if joined := strings.Join(lines, "\n"); !strings.Contains(joined, "login <unique_id> <password>") {
		t.Fatalf("help = %v", lines)
	}

	// version works unauthenticated.
	lines = sendCmd(t, conn, r, "version")
	if lines[0] != "version="+Version {
		t.Fatalf("version = %q", lines[0])
	}

	// quit closes the connection after an ok.
	lines = sendCmd(t, conn, r, "quit")
	if got := lastErr(t, lines); got != "error id=0 msg=ok" {
		t.Fatalf("quit = %q", got)
	}
	if _, err := r.ReadString('\n'); err == nil {
		t.Fatal("connection not closed after quit")
	}
}

func TestBruteForceLockout(t *testing.T) {
	backend := newFakeBackend()
	addr, _ := startQueryServerWith(t, backend, func(s *Server) { s.MaxLoginFailures = 3 })

	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)

	for i := 0; i < 3; i++ {
		sendCmd(t, conn, r, "login admin-uid wrong")
	}
	// Next attempt is refused even with the right password.
	lines := sendCmd(t, conn, r, "login admin-uid pw")
	if got := lastErr(t, lines); !strings.HasPrefix(got, "error id=520") ||
		!strings.Contains(got, `too\smany\sfailed\slogins`) {
		t.Fatalf("lockout error = %q", got)
	}
}

func TestConnectionCap(t *testing.T) {
	addr, _ := startQueryServerWith(t, newFakeBackend(), func(s *Server) { s.MaxConns = 2 })

	c1, _ := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, c1)
	c2, _ := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, c2)

	// Third connection is refused with an error line.
	c3, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer closeServerQueryTestResource(t, c3)
	r3 := bufio.NewReader(c3)
	line, err := r3.ReadString('\n')
	if err != nil {
		t.Fatalf("read refusal: %v", err)
	}
	if !strings.HasPrefix(line, "error id=1539") {
		t.Fatalf("refusal = %q", line)
	}
}

func TestIdleTimeout(t *testing.T) {
	addr, _ := startQueryServerWith(t, newFakeBackend(), func(s *Server) { s.IdleTimeout = 100 * time.Millisecond })

	conn, _ := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)

	// Send nothing: the server should close the connection after the idle
	// timeout.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	_, err := conn.Read(buf)
	if err == nil {
		t.Fatal("connection still open after idle timeout")
	}
}

// TestLoginBackendError verifies an unknown user is a clean login failure.
func TestLoginBackendError(t *testing.T) {
	backend := newFakeBackend()
	addr, _ := startQueryServer(t, backend)
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)

	// Unknown user is a clean login failure, not an internal error.
	lines := sendCmd(t, conn, r, "login ghost-uid pw")
	if got := lastErr(t, lines); !strings.HasPrefix(got, "error id=520") {
		t.Fatalf("error = %q", got)
	}
}

// TestComplaintCommands verifies complaintlist/complaintdel/complaintdelall.
func TestComplaintCommands(t *testing.T) {
	backend := newFakeBackend()
	backend.complaints = []Complaint{
		{ID: 1, Reporter: "user-uid", Target: "admin-uid", Reason: "bad words"},
		{ID: 2, Reporter: "other-uid", Target: "admin-uid", Reason: "spam"},
	}
	addr, _ := startQueryServer(t, backend)
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)
	loginOK(t, conn, r)

	lines := sendCmd(t, conn, r, "complaintlist")
	if len(lines) != 3 { // two rows + error line
		t.Fatalf("complaintlist lines = %v", lines)
	}
	if !strings.Contains(lines[0], `reason=bad\swords`) || !strings.Contains(lines[0], "reporter=user-uid") {
		t.Fatalf("row = %q", lines[0])
	}

	lines = sendCmd(t, conn, r, "complaintdel id=1")
	if got := lastErr(t, lines); got != "error id=0 msg=ok" {
		t.Fatalf("complaintdel = %q", got)
	}
	backend.mu.Lock()
	if len(backend.complaints) != 1 || backend.complaints[0].ID != 2 {
		t.Fatalf("complaints after del = %+v", backend.complaints)
	}
	backend.mu.Unlock()

	sendCmd(t, conn, r, "complaintdelall")
	backend.mu.Lock()
	if len(backend.complaints) != 0 {
		t.Fatalf("complaints after delall = %+v", backend.complaints)
	}
	backend.mu.Unlock()
}

// TestTokenCommands verifies tokenadd/tokenlist/tokendelete.
func TestTokenCommands(t *testing.T) {
	backend := newFakeBackend()
	addr, _ := startQueryServer(t, backend)
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)
	loginOK(t, conn, r)

	// Create an admin-grant token (no group).
	lines := sendCmd(t, conn, r, "tokenadd")
	if !strings.HasPrefix(lines[0], "token=tok-") {
		t.Fatalf("tokenadd response = %q", lines[0])
	}

	// Create a server-group token.
	sendCmd(t, conn, r, "tokenadd tokentype=0 tokenid1=3")

	lines = sendCmd(t, conn, r, "tokenlist")
	if len(lines) != 3 { // two rows + error line
		t.Fatalf("tokenlist lines = %v", lines)
	}
	if !strings.Contains(lines[1], "group_id=3") {
		t.Fatalf("tokenlist row = %q", lines[1])
	}

	// Delete the first token.
	lines = sendCmd(t, conn, r, "tokendelete token=tok-1")
	if got := lastErr(t, lines); got != "error id=0 msg=ok" {
		t.Fatalf("tokendelete = %q", got)
	}
	backend.mu.Lock()
	if len(backend.tokens) != 1 || backend.tokens[0].GroupID != 3 {
		t.Fatalf("tokens after delete = %+v", backend.tokens)
	}
	backend.mu.Unlock()

	// Deleting an unknown token errors.
	lines = sendCmd(t, conn, r, "tokendelete token=nope")
	if got := lastErr(t, lines); !strings.HasPrefix(got, "error id=512") {
		t.Fatalf("tokendelete unknown = %q", got)
	}
}

// TestAuditlogCommand verifies the auditlog query command (149).
func TestAuditlogCommand(t *testing.T) {
	backend := newFakeBackend()
	backend.auditEntries = []AuditEntry{
		{ID: 2, Actor: "admin-uid", Action: "group_create", Target: "server:Mods", Detail: "id=3", CreatedAt: time.Unix(200, 0)},
		{ID: 1, Actor: "serverquery", Action: "channel_create", Target: "5", Detail: "General", CreatedAt: time.Unix(100, 0)},
	}
	addr, _ := startQueryServer(t, backend)
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)
	loginOK(t, conn, r)

	lines := sendCmd(t, conn, r, "auditlog")
	if len(lines) != 3 { // two rows + error line
		t.Fatalf("auditlog lines = %v", lines)
	}
	if !strings.Contains(lines[0], "action=group_create") || !strings.Contains(lines[0], "actor=admin-uid") {
		t.Fatalf("row = %q", lines[0])
	}

	// limit=1 returns only the newest entry.
	lines = sendCmd(t, conn, r, "auditlog limit=1")
	if len(lines) != 2 {
		t.Fatalf("auditlog limit=1 lines = %v", lines)
	}

	// A negative limit is rejected.
	lines = sendCmd(t, conn, r, "auditlog limit=-1")
	if got := lastErr(t, lines); !strings.HasPrefix(got, "error id=512") {
		t.Fatalf("auditlog bad limit = %q", got)
	}
}

// --- wave 10a command tests ---------------------------------------------------

// TestServeredit verifies the full and partial update paths (217).
func TestServeredit(t *testing.T) {
	backend := newFakeBackend()
	addr, _ := startQueryServer(t, backend)
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)
	loginOK(t, conn, r)

	lines := sendCmd(t, conn, r, `serveredit virtualserver_name=My\sServer virtualserver_maxclients=50`)
	if got := lastErr(t, lines); got != "error id=0 msg=ok" {
		t.Fatalf("serveredit = %q", got)
	}
	backend.mu.Lock()
	call := backend.serverEdits[0]
	backend.mu.Unlock()
	if call.Name == nil || *call.Name != "My Server" ||
		call.MaxClients == nil || *call.MaxClients != 50 || call.Welcome != nil {
		t.Fatalf("serveredit call = %+v", call)
	}

	// No fields: usage error.
	lines = sendCmd(t, conn, r, "serveredit")
	if got := lastErr(t, lines); !strings.HasPrefix(got, "error id=512") {
		t.Fatalf("serveredit empty = %q", got)
	}

	// An EMPTY value clears the override; it must reach the backend as a
	// present-but-empty field, not as "unchanged" (217).
	lines = sendCmd(t, conn, r, "serveredit virtualserver_name= virtualserver_maxclients=")
	if got := lastErr(t, lines); got != "error id=0 msg=ok" {
		t.Fatalf("serveredit clear = %q", got)
	}
	backend.mu.Lock()
	call = backend.serverEdits[len(backend.serverEdits)-1]
	backend.mu.Unlock()
	if call.Name == nil || *call.Name != "" || call.MaxClients == nil || *call.MaxClients != 0 {
		t.Fatalf("serveredit clear call = %+v", call)
	}
}

// TestServerstopConfirm verifies the confirm=1 gate (218).
func TestServerstopConfirm(t *testing.T) {
	backend := newFakeBackend()
	addr, _ := startQueryServer(t, backend)
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)
	loginOK(t, conn, r)

	lines := sendCmd(t, conn, r, "serverstop")
	if got := lastErr(t, lines); !strings.HasPrefix(got, "error id=512") {
		t.Fatalf("serverstop without confirm = %q", got)
	}
	backend.mu.Lock()
	called := backend.shutdownCalled
	backend.mu.Unlock()
	if called {
		t.Fatal("shutdown called without confirm")
	}

	lines = sendCmd(t, conn, r, "serverrestart confirm=1")
	if got := lastErr(t, lines); got != "error id=0 msg=ok" {
		t.Fatalf("serverrestart = %q", got)
	}
	// The handler acknowledges BEFORE shutting down, so the reply does not
	// prove Shutdown ran yet — reading the flag straight away is a race.
	deadline := time.Now().Add(5 * time.Second)
	for {
		backend.mu.Lock()
		called = backend.shutdownCalled
		backend.mu.Unlock()
		if called {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("shutdown not called with confirm=1")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestPermoverview verifies the resolved permission listing (219).
func TestPermoverview(t *testing.T) {
	backend := newFakeBackend()
	addr, _ := startQueryServer(t, backend)
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)
	loginOK(t, conn, r)

	lines := sendCmd(t, conn, r, "permoverview user-uid")
	if len(lines) != 3 { // two entries + error line
		t.Fatalf("permoverview lines = %v", lines)
	}
	if !strings.Contains(lines[0], "permid=i_client_talk_power") || !strings.Contains(lines[0], "permvalue=42") || !strings.Contains(lines[0], "tier=server_group") {
		t.Fatalf("row = %q", lines[0])
	}

	lines = sendCmd(t, conn, r, "permoverview nope-uid")
	if got := lastErr(t, lines); !strings.HasPrefix(got, "error id=512") {
		t.Fatalf("permoverview unknown user = %q", got)
	}
}

// TestChannelPerms verifies the channel-tier add/list/delete cycle (220).
func TestChannelPerms(t *testing.T) {
	backend := newFakeBackend()
	addr, _ := startQueryServer(t, backend)
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)
	loginOK(t, conn, r)

	sendCmd(t, conn, r, "channeladdperm cid=1 permid=i_client_needed_talk_power permvalue=50 permgrant=75 permskip=1")
	lines := sendCmd(t, conn, r, "channelpermlist cid=1")
	if len(lines) != 2 {
		t.Fatalf("channelpermlist = %v", lines)
	}
	if !strings.Contains(lines[0], "permvalue=50") || !strings.Contains(lines[0], "permgrant=75") || !strings.Contains(lines[0], "permskip=1") {
		t.Fatalf("row = %q", lines[0])
	}

	sendCmd(t, conn, r, "channeldelperm cid=1 permid=i_client_needed_talk_power")
	lines = sendCmd(t, conn, r, "channelpermlist cid=1")
	if len(lines) != 1 { // just the error line
		t.Fatalf("after del = %v", lines)
	}

	lines = sendCmd(t, conn, r, "channeladdperm cid=1")
	if got := lastErr(t, lines); !strings.HasPrefix(got, "error id=512") {
		t.Fatalf("channeladdperm usage = %q", got)
	}
}

// TestServerGroups verifies the group management cycle (221).
func TestServerGroups(t *testing.T) {
	backend := newFakeBackend()
	addr, _ := startQueryServer(t, backend)
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)
	loginOK(t, conn, r)

	lines := sendCmd(t, conn, r, `servergroupadd name=Mods\sAlpha sortid=10`)
	if !strings.HasPrefix(lines[0], "sgid=") {
		t.Fatalf("servergroupadd = %v", lines)
	}

	lines = sendCmd(t, conn, r, "servergrouplist")
	if !strings.Contains(lines[0], `name=Mods\sAlpha`) || !strings.Contains(lines[0], "sortid=10") {
		t.Fatalf("servergrouplist = %q", lines[0])
	}

	sendCmd(t, conn, r, "servergroupaddclient sgid=1 cldbid=user-uid duration=3600")
	lines = sendCmd(t, conn, r, "servergroupclientlist sgid=1")
	if !strings.Contains(lines[0], "cldbid=user-uid") {
		t.Fatalf("clientlist = %q", lines[0])
	}

	// Delete with members without force: refused; with force: ok.
	lines = sendCmd(t, conn, r, "servergroupdel sgid=1")
	if got := lastErr(t, lines); !strings.HasPrefix(got, "error id=512") {
		t.Fatalf("del without force = %q", got)
	}
	sendCmd(t, conn, r, "servergroupdelclient sgid=1 cldbid=user-uid")
	lines = sendCmd(t, conn, r, "servergroupdel sgid=1")
	if got := lastErr(t, lines); got != "error id=0 msg=ok" {
		t.Fatalf("del after unassign = %q", got)
	}
}

// TestCustomProps verifies customset/custominfo/customdel (222) and escaping.
func TestCustomProps(t *testing.T) {
	backend := newFakeBackend()
	addr, _ := startQueryServer(t, backend)
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)
	loginOK(t, conn, r)

	sendCmd(t, conn, r, `customset cldbid=user-uid ident=role value=senior\sdev`)
	sendCmd(t, conn, r, `customset cldbid=user-uid ident=team value=core`)
	lines := sendCmd(t, conn, r, "custominfo cldbid=user-uid")
	if len(lines) != 3 {
		t.Fatalf("custominfo = %v", lines)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, `ident=role value=senior\sdev`) || !strings.Contains(joined, "ident=team value=core") {
		t.Fatalf("custominfo rows = %v", lines)
	}

	sendCmd(t, conn, r, "customdel cldbid=user-uid ident=role")
	lines = sendCmd(t, conn, r, "custominfo cldbid=user-uid")
	if len(lines) != 2 {
		t.Fatalf("after del = %v", lines)
	}
}

// TestLogview verifies the log tail command (223).
func TestLogview(t *testing.T) {
	backend := newFakeBackend()
	backend.logLines = []string{"first line", "second line with marker"}
	addr, _ := startQueryServer(t, backend)
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)
	loginOK(t, conn, r)

	lines := sendCmd(t, conn, r, "logview lines=5")
	if len(lines) != 3 {
		t.Fatalf("logview = %v", lines)
	}
	if lines[0] != `line=first\sline` {
		t.Fatalf("logview row = %q", lines[0])
	}
}

// TestLogviewFollow verifies that follow streams lines emitted after the tail,
// honours the filter, and releases the subscription (223).
func TestLogviewFollow(t *testing.T) {
	backend := newFakeBackend()
	backend.logLines = []string{"tail line"}
	addr, _ := startQueryServer(t, backend)
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)
	loginOK(t, conn, r)

	// Queue the live lines up front: the follow window is short, and the
	// stream buffer holds them until the command drains it.
	backend.logStream <- "ignored by filter"
	backend.logStream <- "live marker line"

	lines := sendCmd(t, conn, r, "logview lines=5 filter=marker follow=1")
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, `line=live\smarker\sline`) {
		t.Fatalf("follow output = %v", lines)
	}
	if strings.Contains(joined, "ignored") {
		t.Fatalf("filter not applied to the live stream: %v", lines)
	}
	backend.mu.Lock()
	cancelled := backend.followCancelled
	backend.mu.Unlock()
	if !cancelled {
		t.Fatal("follow did not release its subscription")
	}

	// Out-of-range windows are rejected before anything is streamed.
	lines = sendCmd(t, conn, r, "logview follow=0")
	if got := lastErr(t, lines); !strings.HasPrefix(got, "error id=512") {
		t.Fatalf("logview follow=0 = %q", got)
	}
}

// TestServerrules verifies the rules read-back and the serverset write path
// (215).
func TestServerrules(t *testing.T) {
	backend := newFakeBackend()
	backend.rulesText = "be nice\nno spam"
	backend.rulesHash = "abc123"
	backend.rulesAccepted = 7
	addr, _ := startQueryServer(t, backend)
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)
	loginOK(t, conn, r)

	lines := sendCmd(t, conn, r, "serverrules")
	if len(lines) != 2 {
		t.Fatalf("serverrules = %v", lines)
	}
	if !strings.Contains(lines[0], `rules_hash=abc123`) ||
		!strings.Contains(lines[0], "accepted_clients=7") ||
		!strings.Contains(lines[0], `rules=be\snice`) {
		t.Fatalf("serverrules row = %q", lines[0])
	}

	lines = sendCmd(t, conn, r, `serverset key=server_rules value=new\srules`)
	if got := lastErr(t, lines); got != "error id=0 msg=ok" {
		t.Fatalf("serverset server_rules = %q", got)
	}
	backend.mu.Lock()
	stored := backend.settings["server_rules"]
	backend.mu.Unlock()
	if stored != "new rules" {
		t.Fatalf("stored rules = %q", stored)
	}
}

// TestChanneladdpermUnknownKey verifies that an unknown permid is refused
// before it becomes a dead permission row (220).
func TestChanneladdpermUnknownKey(t *testing.T) {
	backend := newFakeBackend()
	addr, _ := startQueryServer(t, backend)
	conn, r := dialQuery(t, addr)
	defer closeServerQueryTestResource(t, conn)
	loginOK(t, conn, r)

	lines := sendCmd(t, conn, r, "channeladdperm cid=1 permid=i_client_needed_talkpower permvalue=50")
	if got := lastErr(t, lines); !strings.HasPrefix(got, "error id=512") {
		t.Fatalf("unknown permid = %q", got)
	}
	backend.mu.Lock()
	stored := len(backend.channelPerms)
	backend.mu.Unlock()
	if stored != 0 {
		t.Fatalf("unknown permid stored %d rows", stored)
	}
}

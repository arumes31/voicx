// extras_test.go covers the Phase 9 control handlers: server password,
// avatars, channel icons, token use, complaints, and screen share.
package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"voicx/internal/auth"
	"voicx/internal/netproto"
	"voicx/internal/permissions"
	"voicx/internal/store"
)

// --- fakes ------------------------------------------------------------------

// fakeTokens implements TokenBackend with canned outcomes.
type fakeTokens struct {
	mu        sync.Mutex
	used      []string
	grants    map[string]int64 // token -> groupID (0 = admin grant)
	exhausted map[string]bool
	rows      []store.Token
	nextID    int64
}

func (f *fakeTokens) UseTokenForIdentity(_ context.Context, key string, userID int64, _ string, _ string) (store.TokenGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.used = append(f.used, key)
	if f.exhausted[key] {
		return store.TokenGrant{}, store.ErrTokenExhausted
	}
	g, ok := f.grants[key]
	if !ok {
		return store.TokenGrant{}, store.ErrTokenNotFound
	}
	promoted := userID == 0
	if promoted {
		userID = 99
	}
	return store.TokenGrant{UserID: userID, GroupID: g, Admin: g == 0, Promoted: promoted}, nil
}

func (f *fakeTokens) ListTokens(context.Context) ([]store.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Token(nil), f.rows...), nil
}

func (f *fakeTokens) CreateTokenWithMeta(_ context.Context, tokenType int, groupID, channelID int64, description string, maxUses int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	key := fmt.Sprintf("generated-%d", f.nextID)
	f.rows = append(f.rows, store.Token{
		ID: f.nextID, Key: key, Type: tokenType, GroupID: groupID, ChannelID: channelID,
		MaxUses: maxUses, CreatedAt: time.Unix(1700000000, 0), Description: description,
	})
	return key, nil
}

func (f *fakeTokens) DeleteToken(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, t := range f.rows {
		if t.Key == key {
			f.rows = append(f.rows[:i], f.rows[i+1:]...)
			return nil
		}
	}
	return store.ErrTokenNotFound
}

// fakeComplaints implements ComplaintBackend, enforcing the open-complaint
// limit per reporter like the store does.
type fakeComplaints struct {
	mu     sync.Mutex
	rows   []store.Complaint
	nextID int64
}

func (f *fakeComplaints) AddComplaint(_ context.Context, reporter, target, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	open := 0
	for _, c := range f.rows {
		if c.Reporter == reporter {
			open++
		}
	}
	if open >= store.MaxOpenComplaints {
		return store.ErrComplaintLimit
	}
	f.nextID++
	f.rows = append(f.rows, store.Complaint{
		ID: f.nextID, Reporter: reporter, Target: target, Reason: reason,
		CreatedAt: time.Unix(1700000000, 0),
	})
	return nil
}

func (f *fakeComplaints) ListComplaints(context.Context) ([]store.Complaint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Complaint(nil), f.rows...), nil
}

func (f *fakeComplaints) DeleteComplaintsAgainst(_ context.Context, target, reporter string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.rows[:0:0]
	var n int64
	for _, c := range f.rows {
		if c.Target == target && (reporter == "" || c.Reporter == reporter) {
			n++
			continue
		}
		kept = append(kept, c)
	}
	f.rows = kept
	return n, nil
}

func (f *fakeComplaints) count(reporter string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.rows {
		if c.Reporter == reporter {
			n++
		}
	}
	return n
}

// tinyPNG is the smallest byte sequence http.DetectContentType recognizes as
// image/png.
var tinyPNG = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
	0, 0, 0, 0x0D, 0x49, 0x48, 0x44, 0x52, 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 0x1F, 0x15, 0xC4, 0x89}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// --- server password ----------------------------------------------------------

// TestServerPassword verifies the global server password gate: open server
// (no hash) accepts, wrong password rejects, correct accepts.
func TestServerPassword(t *testing.T) {
	hash, err := auth.HashPassword("hunter2")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	env := startTestEnv(t, nil)
	env.deps.ServerPasswordHash = hash
	defer env.stop()

	// Missing server password: rejected.
	conn := dialRetry(t, env.addr)
	defer conn.Close()
	send(t, conn, netproto.MsgAuthenticate, netproto.Authenticate{Username: "user-uid", Password: "pw"})
	f := readOfType(t, conn, netproto.MsgAuthResponse)
	var resp netproto.AuthResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Fatal("auth without server password succeeded on a protected server")
	}

	// Wrong server password: rejected.
	send(t, conn, netproto.MsgAuthenticate, netproto.Authenticate{Username: "user-uid", Password: "pw", ServerPassword: "wrong"})
	f = readOfType(t, conn, netproto.MsgAuthResponse)
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Fatal("auth with wrong server password succeeded")
	}

	// Correct server password: accepted.
	send(t, conn, netproto.MsgAuthenticate, netproto.Authenticate{Username: "user-uid", Password: "pw", ServerPassword: "hunter2"})
	f = readOfType(t, conn, netproto.MsgAuthResponse)
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Fatalf("auth with correct server password failed: %s", resp.Reason)
	}
}

// --- avatar -----------------------------------------------------------------

// TestAvatarSetGet verifies avatar validation and the set/get round-trip.
func TestAvatarSetGet(t *testing.T) {
	perms := tieredWith(boolPerm(permissions.PermissionKeyClientAvatarUpload, true))
	env := startTestEnv(t, &perms)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	// Wrong type rejected.
	send(t, conn, netproto.MsgAvatarSet, netproto.AvatarSet{DataBase64: b64([]byte("not an image"))})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodeMalformed {
		t.Fatalf("error code = %d, want %d", e.Code, errCodeMalformed)
	}

	// Valid PNG accepted.
	send(t, conn, netproto.MsgAvatarSet, netproto.AvatarSet{DataBase64: b64(tinyPNG)})
	// Get it back.
	send(t, conn, netproto.MsgAvatarGet, netproto.AvatarGet{UniqueID: "user-uid"})
	f = readOfType(t, conn, netproto.MsgAvatarData)
	var data netproto.AvatarData
	if err := netproto.Decode(f, &data); err != nil {
		t.Fatalf("decode avatar data: %v", err)
	}
	if data.ContentType != "image/png" {
		t.Fatalf("content type = %q, want image/png", data.ContentType)
	}
	raw, err := base64.StdEncoding.DecodeString(data.DataBase64)
	if err != nil || string(raw) != string(tinyPNG) {
		t.Fatalf("avatar round-trip mismatch: %v", err)
	}
}

// TestAvatarOversize verifies oversized images are rejected.
func TestAvatarOversize(t *testing.T) {
	perms := tieredWith(boolPerm(permissions.PermissionKeyClientAvatarUpload, true))
	env := startTestEnv(t, &perms)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	big := make([]byte, maxImageBytes+1)
	copy(big, tinyPNG)
	send(t, conn, netproto.MsgAvatarSet, netproto.AvatarSet{DataBase64: b64(big)})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodeMalformed {
		t.Fatalf("error code = %d, want %d", e.Code, errCodeMalformed)
	}
}

// TestAvatarChangedEvent verifies other clients get avatar_changed on set.
func TestAvatarChangedEvent(t *testing.T) {
	perms := tieredWith(boolPerm(permissions.PermissionKeyClientAvatarUpload, true))
	env := startTestEnv(t, &perms)
	defer env.stop()

	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer adminConn.Close()
	userConn, userID := dialAuthed(t, env.addr, "user-uid")
	defer userConn.Close()

	send(t, userConn, netproto.MsgAvatarSet, netproto.AvatarSet{DataBase64: b64(tinyPNG)})
	data := readEventOfType(t, adminConn, eventAvatarChanged)
	var ue userEvent
	if err := json.Unmarshal(data, &ue); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ue.ClientID != userID {
		t.Fatalf("avatar_changed client = %q, want %q", ue.ClientID, userID)
	}
}

func TestAvatarSetRequiresPermission(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgAvatarSet, netproto.AvatarSet{DataBase64: b64(tinyPNG)})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("error code = %d, want %d", e.Code, errCodePermissionDenied)
	}
}

// --- channel icon -------------------------------------------------------------

// TestChannelIconSet verifies the permission gate and storage flag.
func TestChannelIconSet(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer adminConn.Close()
	userConn, _ := dialAuthed(t, env.addr, "user-uid")
	defer userConn.Close()

	send(t, adminConn, netproto.MsgCreateChannel, netproto.CreateChannel{Name: "Lobby", Type: 2})
	readOfType(t, adminConn, netproto.MsgChannelList)

	// Non-admin without b_channel_modify: denied.
	send(t, userConn, netproto.MsgChannelIconSet, netproto.ChannelIconSet{ChannelID: 1, DataBase64: b64(tinyPNG)})
	f := readOfType(t, userConn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("error code = %d, want %d", e.Code, errCodePermissionDenied)
	}

	// Admin: allowed; channel flagged.
	send(t, adminConn, netproto.MsgChannelIconSet, netproto.ChannelIconSet{ChannelID: 1, DataBase64: b64(tinyPNG)})
	readEventOfType(t, userConn, eventChannelIconChanged)
	ch, ok := env.state.GetChannel(1)
	if !ok || !ch.HasIcon {
		t.Fatal("channel not flagged HasIcon")
	}
}

// --- token use ---------------------------------------------------------------

// TestTokenUse verifies redemption applies the grant, invalidates the
// permission cache, and notifies the client.
func TestTokenUse(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	env.tokens.grants = map[string]int64{"tok-abc": 5}

	conn, userID := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgTokenUse, netproto.TokenUse{Token: "tok-abc"})
	data := readEventOfType(t, conn, eventTokenUsed)
	var ev struct {
		ClientID string `json:"client_id"`
		GroupID  int64  `json:"group_id"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.ClientID != userID || ev.GroupID != 5 {
		t.Fatalf("token_used = %+v", ev)
	}

	env.perms.mu.Lock()
	defer env.perms.mu.Unlock()
	if len(env.perms.invalidations) != 1 || env.perms.invalidations[0][0] != 2 { // user-uid has user ID 2
		t.Fatalf("invalidations = %v", env.perms.invalidations)
	}
}

// TestTokenUseUnknown verifies unknown and exhausted tokens are rejected.
func TestTokenUseUnknown(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	env.tokens.exhausted = map[string]bool{"tok-old": true}

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgTokenUse, netproto.TokenUse{Token: "nope"})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodeNotFound {
		t.Fatalf("unknown token error = %d, want %d", e.Code, errCodeNotFound)
	}

	send(t, conn, netproto.MsgTokenUse, netproto.TokenUse{Token: "tok-old"})
	f = readOfType(t, conn, netproto.MsgError)
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodeMalformed {
		t.Fatalf("exhausted token error = %d, want %d", e.Code, errCodeMalformed)
	}
}

// --- complaints ---------------------------------------------------------------

// TestComplaint verifies filing works up to the per-reporter limit.
func TestComplaint(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	for i := 0; i < store.MaxOpenComplaints; i++ {
		send(t, conn, netproto.MsgComplaint, netproto.Complaint{TargetUniqueID: "admin-uid", Reason: "spam"})
	}
	// The limit+1-th complaint is rejected.
	send(t, conn, netproto.MsgComplaint, netproto.Complaint{TargetUniqueID: "admin-uid", Reason: "spam"})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodeMalformed {
		t.Fatalf("error code = %d, want %d", e.Code, errCodeMalformed)
	}
	if got := env.complaints.count("user-uid"); got != store.MaxOpenComplaints {
		t.Fatalf("complaints = %d, want %d", got, store.MaxOpenComplaints)
	}
}

// TestComplaintUnknownTarget verifies complaints against unknown users fail.
func TestComplaintUnknownTarget(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgComplaint, netproto.Complaint{TargetUniqueID: "ghost", Reason: "x"})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodeNotFound {
		t.Fatalf("error code = %d, want %d", e.Code, errCodeNotFound)
	}
}

// --- screen share ---------------------------------------------------------------

// TestScreenShare verifies the event relay to channel members.
func TestScreenShare(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer adminConn.Close()
	userConn, userID := dialAuthed(t, env.addr, "user-uid")
	defer userConn.Close()

	send(t, adminConn, netproto.MsgCreateChannel, netproto.CreateChannel{Name: "Lobby", Type: 2})
	readOfType(t, adminConn, netproto.MsgChannelList)
	send(t, adminConn, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 1})
	send(t, userConn, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 1})
	waitFor(t, "both in channel", func() bool {
		return len(env.state.ChannelMembers(1)) == 2
	})

	send(t, userConn, netproto.MsgScreenShare, netproto.ScreenShare{Active: true})
	data := readEventOfType(t, adminConn, eventScreenshareChanged)
	var ev struct {
		ClientID string `json:"client_id"`
		Active   bool   `json:"active"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.ClientID != userID || !ev.Active {
		t.Fatalf("screenshare_changed = %+v", ev)
	}
}

// TestScreenShareDenied verifies a negated video publish permission denies
// screen sharing.
func TestScreenShareDenied(t *testing.T) {
	perms := tieredWith(&permissions.Permission{
		Key:    permissions.PermissionKeyClientVideoPublish,
		Type:   permissions.PermissionTypeBoolean,
		Value:  0,
		Negate: true,
	})
	env := startTestEnv(t, &perms)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgScreenShare, netproto.ScreenShare{Active: true})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("error code = %d, want %d", e.Code, errCodePermissionDenied)
	}
}

// --- permissions query -----------------------------------------------------

// TestPermissionsQuery verifies the resolved permission set is returned.
func TestPermissionsQuery(t *testing.T) {
	perms := tieredWith(
		boolPerm(permissions.PermissionKeyChannelCreateTemporary, true),
		intPerm(permissions.PermissionKeyChannelJoinPower, 75),
	)
	env := startTestEnv(t, &perms)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgPermissionsQuery, netproto.PermissionsQuery{})
	f := readOfType(t, conn, netproto.MsgPermissionsResponse)
	var resp netproto.PermissionsResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Entries) != 2 {
		t.Fatalf("entries = %+v, want 2", resp.Entries)
	}
	byKey := make(map[string]netproto.PermissionEntry, len(resp.Entries))
	for _, e := range resp.Entries {
		byKey[e.Key] = e
	}
	if e := byKey["b_channel_create_temporary"]; e.Value != 1 {
		t.Errorf("create_temporary = %+v, want value 1", e)
	}
	if e := byKey["i_channel_join_power"]; e.Value != 75 {
		t.Errorf("join_power = %+v, want value 75", e)
	}
}

// TestPermissionsQueryEmpty verifies an empty set returns no entries.
func TestPermissionsQueryEmpty(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgPermissionsQuery, netproto.PermissionsQuery{})
	f := readOfType(t, conn, netproto.MsgPermissionsResponse)
	var resp netproto.PermissionsResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Entries) != 0 {
		t.Fatalf("entries = %+v, want empty", resp.Entries)
	}
}

// --- anonymous / guest login -------------------------------------------------

// dialGuest connects and authenticates as an anonymous guest, returning the
// connection and the auth response.
func dialGuest(t *testing.T, addr, nickname, serverPassword string) (net.Conn, netproto.AuthResponse) {
	t.Helper()
	conn := dialRetry(t, addr)
	send(t, conn, netproto.MsgAuthenticate, netproto.Authenticate{
		Anonymous:      true,
		Nickname:       nickname,
		ServerPassword: serverPassword,
	})
	f := readOfType(t, conn, netproto.MsgAuthResponse)
	var resp netproto.AuthResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode auth response: %v", err)
	}
	if resp.OK {
		readOfType(t, conn, netproto.MsgSnapshot)
	}
	return conn, resp
}

// TestAnonymousAuthHappy verifies an ephemeral guest login: guest: unique ID,
// nickname kept, registered in state, never admin.
func TestAnonymousAuthHappy(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, resp := dialGuest(t, env.addr, "guesty", "")
	defer conn.Close()

	if !resp.OK {
		t.Fatalf("guest auth failed: %s", resp.Reason)
	}
	if !strings.HasPrefix(resp.UniqueID, "guest:") {
		t.Fatalf("unique id = %q, want guest: prefix", resp.UniqueID)
	}
	if resp.Nickname != "guesty" {
		t.Fatalf("nickname = %q, want guesty", resp.Nickname)
	}

	sc, ok := env.state.GetClient(resp.ClientID)
	if !ok {
		t.Fatal("guest not registered in state")
	}
	if sc.Nickname != "guesty" || !strings.HasPrefix(sc.UniqueID, "guest:") {
		t.Fatalf("state client = %+v", sc)
	}
}

// TestAnonymousNicknameDedup verifies duplicate guest nicknames get a suffix.
func TestAnonymousNicknameDedup(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn1, resp1 := dialGuest(t, env.addr, "guesty", "")
	defer conn1.Close()
	conn2, resp2 := dialGuest(t, env.addr, "guesty", "")
	defer conn2.Close()

	if resp1.Nickname != "guesty" {
		t.Fatalf("first nickname = %q, want guesty", resp1.Nickname)
	}
	if resp2.Nickname != "guesty#2" {
		t.Fatalf("second nickname = %q, want guesty#2", resp2.Nickname)
	}
}

// TestAnonymousServerPassword verifies the global server password applies to
// guest logins too.
func TestAnonymousServerPassword(t *testing.T) {
	hash, err := auth.HashPassword("hunter2")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	env := startTestEnv(t, nil)
	env.deps.ServerPasswordHash = hash
	defer env.stop()

	// Without server password: rejected.
	conn := dialRetry(t, env.addr)
	defer conn.Close()
	send(t, conn, netproto.MsgAuthenticate, netproto.Authenticate{Anonymous: true, Nickname: "g"})
	f := readOfType(t, conn, netproto.MsgAuthResponse)
	var resp netproto.AuthResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Fatal("guest auth without server password succeeded on a protected server")
	}

	// With server password: accepted.
	send(t, conn, netproto.MsgAuthenticate, netproto.Authenticate{Anonymous: true, Nickname: "g", ServerPassword: "hunter2"})
	f = readOfType(t, conn, netproto.MsgAuthResponse)
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Fatalf("guest auth with server password failed: %s", resp.Reason)
	}
}

// TestAnonymousBannedIP verifies IP bans apply to guests.
func TestAnonymousBannedIP(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	env.auth.bans["127.0.0.1"] = &auth.Ban{ID: 9, Type: 0, Value: "127.0.0.1"}

	conn := dialRetry(t, env.addr)
	defer conn.Close()
	send(t, conn, netproto.MsgAuthenticate, netproto.Authenticate{Anonymous: true, Nickname: "g"})
	f := readOfType(t, conn, netproto.MsgAuthResponse)
	var resp netproto.AuthResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Fatal("IP-banned guest auth succeeded")
	}
}

// TestAnonymousCreateDenied verifies guests cannot create channels (default
// deny-on-unset semantics).
func TestAnonymousCreateDenied(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, _ := dialGuest(t, env.addr, "guesty", "")
	defer conn.Close()

	send(t, conn, netproto.MsgCreateChannel, netproto.CreateChannel{Name: "nope", Type: 0})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("error code = %d, want %d", e.Code, errCodePermissionDenied)
	}
}

// TestGuestWithIdentity verifies the key-derived guest path: challenge +
// signature with a presented public key yields a stable unique ID and the
// requested nickname, without any users row.
func TestGuestWithIdentity(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	pubPEM, privPEM, err := auth.GenerateIdentityKeyPair()
	if err != nil {
		t.Fatalf("GenerateIdentityKeyPair: %v", err)
	}
	uid, err := auth.UniqueIDFromPublicKey(pubPEM)
	if err != nil {
		t.Fatalf("UniqueIDFromPublicKey: %v", err)
	}

	conn := dialRetry(t, env.addr)
	defer conn.Close()
	send(t, conn, netproto.MsgAuthenticate, netproto.Authenticate{
		Username:  uid,
		Anonymous: true,
		Nickname:  "keyguest",
	})
	f := readOfType(t, conn, netproto.MsgAuthChallenge)
	var ch netproto.AuthChallenge
	if err := netproto.Decode(f, &ch); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}

	sig, err := auth.SignChallenge(privPEM, ch.Challenge)
	if err != nil {
		t.Fatalf("SignChallenge: %v", err)
	}
	send(t, conn, netproto.MsgAuthSignature, netproto.AuthSignature{
		PublicKey: pubPEM,
		Signature: sig,
	})

	f = readOfType(t, conn, netproto.MsgAuthResponse)
	var resp netproto.AuthResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode auth response: %v", err)
	}
	if !resp.OK {
		t.Fatalf("guest-with-identity auth failed: %s", resp.Reason)
	}
	if resp.UniqueID != uid {
		t.Fatalf("unique id = %q, want key-derived %q", resp.UniqueID, uid)
	}
	if resp.Nickname != "keyguest" {
		t.Fatalf("nickname = %q, want keyguest", resp.Nickname)
	}
}

// --- nickname login + identity binding ---------------------------------------

// TestNicknamePasswordLogin verifies password auth by nickname: the account's
// unique ID is returned, and the client's presented public key is bound.
func TestNicknamePasswordLogin(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	env.auth.nicknames["user"] = env.auth.users["user-uid"]

	conn := dialRetry(t, env.addr)
	defer conn.Close()
	send(t, conn, netproto.MsgAuthenticate, netproto.Authenticate{
		Username:  "user", // nickname, not the unique ID
		Password:  "pw",
		PublicKey: "CLIENT-PUB-PEM",
	})
	f := readOfType(t, conn, netproto.MsgAuthResponse)
	var resp netproto.AuthResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode auth response: %v", err)
	}
	if !resp.OK {
		t.Fatalf("nickname login failed: %s", resp.Reason)
	}
	if resp.UniqueID != "user-uid" {
		t.Fatalf("unique id = %q, want canonical user-uid", resp.UniqueID)
	}
	if resp.Nickname != "user" {
		t.Fatalf("nickname = %q, want user", resp.Nickname)
	}

	env.auth.mu.Lock()
	defer env.auth.mu.Unlock()
	if len(env.auth.bindings) != 1 || env.auth.bindings[0][0] != int64(2) || env.auth.bindings[0][1] != "CLIENT-PUB-PEM" {
		t.Fatalf("bindings = %v, want [(2, CLIENT-PUB-PEM)]", env.auth.bindings)
	}
}

// TestNicknameLoginWrongPassword verifies nickname login with a bad password
// is rejected.
func TestNicknameLoginWrongPassword(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	env.auth.nicknames["user"] = env.auth.users["user-uid"]

	conn := dialRetry(t, env.addr)
	defer conn.Close()
	send(t, conn, netproto.MsgAuthenticate, netproto.Authenticate{Username: "user", Password: "wrong"})
	f := readOfType(t, conn, netproto.MsgAuthResponse)
	var resp netproto.AuthResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode auth response: %v", err)
	}
	if resp.OK {
		t.Fatal("nickname login with wrong password succeeded")
	}
}

// TestChallengeAuthWithBoundKey verifies a client whose key was bound to an
// account (via nickname login) can subsequently log in with challenge auth
// and gets the account's canonical unique ID.
func TestChallengeAuthWithBoundKey(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	pubPEM, privPEM, err := auth.GenerateIdentityKeyPair()
	if err != nil {
		t.Fatalf("GenerateIdentityKeyPair: %v", err)
	}
	// The client key is NOT the account's registration key: it was bound to
	// user-uid via a nickname login.
	env.auth.pubkeyIndex[pubPEM] = env.auth.users["user-uid"]
	uid, err := auth.UniqueIDFromPublicKey(pubPEM)
	if err != nil {
		t.Fatalf("UniqueIDFromPublicKey: %v", err)
	}

	conn := dialRetry(t, env.addr)
	defer conn.Close()
	send(t, conn, netproto.MsgAuthenticate, netproto.Authenticate{Username: uid, Nickname: "user", Anonymous: true})
	f := readOfType(t, conn, netproto.MsgAuthChallenge)
	var ch netproto.AuthChallenge
	if err := netproto.Decode(f, &ch); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}

	sig, err := auth.SignChallenge(privPEM, ch.Challenge)
	if err != nil {
		t.Fatalf("SignChallenge: %v", err)
	}
	send(t, conn, netproto.MsgAuthSignature, netproto.AuthSignature{PublicKey: pubPEM, Signature: sig})

	f = readOfType(t, conn, netproto.MsgAuthResponse)
	var resp netproto.AuthResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode auth response: %v", err)
	}
	if !resp.OK {
		t.Fatalf("bound-key challenge login failed: %s", resp.Reason)
	}
	if resp.UniqueID != "user-uid" {
		t.Fatalf("unique id = %q, want canonical user-uid", resp.UniqueID)
	}
}

// --- client info -------------------------------------------------------------

// queryClientInfo sends a ClientInfoQuery and decodes the response.
func queryClientInfo(t *testing.T, conn net.Conn, clientID string) netproto.ClientInfoResponse {
	t.Helper()
	send(t, conn, netproto.MsgClientInfoQuery, netproto.ClientInfoQuery{ClientID: clientID})
	f := readOfType(t, conn, netproto.MsgClientInfoResponse)
	var resp netproto.ClientInfoResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode client info: %v", err)
	}
	return resp
}

// TestClientInfoSelf verifies a self query returns full data incl. own IP.
func TestClientInfoSelf(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, clientID := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	resp := queryClientInfo(t, conn, clientID)
	if resp.ClientID != clientID || resp.UniqueID != "user-uid" || resp.Nickname != "user" {
		t.Fatalf("info = %+v", resp)
	}
	if resp.IP == "" || resp.Port == 0 {
		t.Fatalf("self query missing ip/port: %+v", resp)
	}
	if resp.ConnectedAt <= 0 {
		t.Fatalf("connected_at = %d, want > 0", resp.ConnectedAt)
	}
	if resp.BytesIn <= 0 {
		t.Fatalf("bytes_in = %d, want > 0 after auth", resp.BytesIn)
	}
	// Ping unknown on loopback without a server ping cycle: -1 or measured.
	if resp.PingMs < -1 {
		t.Fatalf("ping_ms = %d", resp.PingMs)
	}
}

// TestClientInfoOtherGuestDenied verifies a guest querying another client
// gets no IP (deny-on-unset for the remote address permission).
func TestClientInfoOtherGuestDenied(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	bobConn, bobID := dialAuthed(t, env.addr, "user-uid")
	defer bobConn.Close()
	guestConn, _ := dialGuest(t, env.addr, "snoopy", "")
	defer guestConn.Close()

	resp := queryClientInfo(t, guestConn, bobID)
	if resp.ClientID != bobID {
		t.Fatalf("info = %+v", resp)
	}
	if resp.IP != "" || resp.Port != 0 {
		t.Fatalf("guest query leaked ip/port: %+v", resp)
	}
}

// TestClientInfoOtherGranted verifies the remote-address permission grants
// IP visibility.
func TestClientInfoOtherGranted(t *testing.T) {
	perms := tieredWith(boolPerm(permissions.PermissionKeyClientRemoteAddressView, true))
	env := startTestEnv(t, &perms)
	defer env.stop()

	bobConn, bobID := dialAuthed(t, env.addr, "user-uid")
	defer bobConn.Close()
	aliceConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer aliceConn.Close()

	resp := queryClientInfo(t, aliceConn, bobID)
	if resp.IP == "" || resp.Port == 0 {
		t.Fatalf("granted query missing ip/port: %+v", resp)
	}
}

// TestClientInfoAdminBypass verifies admins see the remote address without
// an explicit grant.
func TestClientInfoAdminBypass(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	userConn, userID := dialAuthed(t, env.addr, "user-uid")
	defer userConn.Close()
	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer adminConn.Close()

	resp := queryClientInfo(t, adminConn, userID)
	if resp.IP == "" || resp.Port == 0 {
		t.Fatalf("admin query missing ip/port: %+v", resp)
	}
}

// TestClientInfoUnknown verifies querying a missing client errors.
func TestClientInfoUnknown(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgClientInfoQuery, netproto.ClientInfoQuery{ClientID: "c-nope"})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodeNotFound {
		t.Fatalf("error code = %d, want %d", e.Code, errCodeNotFound)
	}
}

// TestClientInfoActivityTracking verifies received frames bump the
// last-active/bytes counters.
func TestClientInfoActivityTracking(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, clientID := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	before := queryClientInfo(t, conn, clientID)
	// Send a chat message to bump activity.
	send(t, conn, netproto.MsgChatSend, netproto.ChatSend{Text: "bump"})
	after := queryClientInfo(t, conn, clientID)
	if after.BytesIn <= before.BytesIn {
		t.Fatalf("bytes_in did not increase: before=%d after=%d", before.BytesIn, after.BytesIn)
	}
	if after.IdleSeconds > before.IdleSeconds+2 {
		t.Fatalf("idle not refreshed: before=%d after=%d", before.IdleSeconds, after.IdleSeconds)
	}
}

// TestEWMARTT verifies the smoothed RTT math.
func TestEWMARTT(t *testing.T) {
	if got := ewmaRTT(0, 80, false); got != 80 {
		t.Fatalf("first sample = %d, want 80", got)
	}
	prev := int64(80)
	got := ewmaRTT(prev, 40, true)
	want := prev*7/8 + 40/8
	if got != want {
		t.Fatalf("ewma = %d, want %d", got, want)
	}
}

// --- complaint review (173) --------------------------------------------------

// TestComplaintList verifies an admin sees every complaint with nicknames
// resolved from the users table.
func TestComplaintList(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	userConn, _ := dialAuthed(t, env.addr, "user-uid")
	defer userConn.Close()
	send(t, userConn, netproto.MsgComplaint, netproto.Complaint{TargetUniqueID: "admin-uid", Reason: "spam"})

	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer adminConn.Close()
	send(t, adminConn, netproto.MsgComplaintList, netproto.ComplaintList{})

	f := readOfType(t, adminConn, netproto.MsgComplaints)
	var resp netproto.Complaints
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode complaints: %v", err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(resp.Entries))
	}
	e := resp.Entries[0]
	if e.TargetUniqueID != "admin-uid" || e.FromUniqueID != "user-uid" || e.Reason != "spam" {
		t.Fatalf("entry = %+v", e)
	}
	if e.TargetNickname != "admin" || e.FromNickname != "user" {
		t.Fatalf("nicknames unresolved: %+v", e)
	}
	if e.CreatedAt == 0 {
		t.Fatal("created_at not set")
	}
}

// TestComplaintClear verifies clearing one reporter's complaint, then all of
// them, and that each clear is audited and returns the refreshed list.
func TestComplaintClear(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	userConn, _ := dialAuthed(t, env.addr, "user-uid")
	defer userConn.Close()
	send(t, userConn, netproto.MsgComplaint, netproto.Complaint{TargetUniqueID: "admin-uid", Reason: "spam"})

	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer adminConn.Close()
	send(t, adminConn, netproto.MsgComplaint, netproto.Complaint{TargetUniqueID: "user-uid", Reason: "flood"})

	// Targeted clear: only the complaint from user-uid against admin-uid goes.
	send(t, adminConn, netproto.MsgComplaintClear, netproto.ComplaintClear{
		TargetUniqueID: "admin-uid", FromUniqueID: "user-uid",
	})
	f := readOfType(t, adminConn, netproto.MsgComplaints)
	var resp netproto.Complaints
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode complaints: %v", err)
	}
	if len(resp.Entries) != 1 || resp.Entries[0].TargetUniqueID != "user-uid" {
		t.Fatalf("after targeted clear entries = %+v", resp.Entries)
	}

	// Blanket clear against the remaining target.
	send(t, adminConn, netproto.MsgComplaintClear, netproto.ComplaintClear{TargetUniqueID: "user-uid"})
	f = readOfType(t, adminConn, netproto.MsgComplaints)
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode complaints: %v", err)
	}
	if len(resp.Entries) != 0 {
		t.Fatalf("after blanket clear entries = %+v", resp.Entries)
	}

	// Clearing nothing is a not-found, not a silent success.
	send(t, adminConn, netproto.MsgComplaintClear, netproto.ComplaintClear{TargetUniqueID: "user-uid"})
	if e := readError(t, adminConn); e.Code != errCodeNotFound {
		t.Fatalf("empty clear error = %d, want %d", e.Code, errCodeNotFound)
	}

	var clears int
	for _, a := range env.groups.auditActions() {
		if a == "complaint_clear" {
			clears++
		}
	}
	if clears != 2 {
		t.Fatalf("audited clears = %d, want 2", clears)
	}
}

// TestComplaintReviewDenied verifies a user without the ban gate can neither
// read nor clear complaints.
func TestComplaintReviewDenied(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgComplaintList, netproto.ComplaintList{})
	if e := readError(t, conn); e.Code != errCodePermissionDenied {
		t.Fatalf("list error = %d, want %d", e.Code, errCodePermissionDenied)
	}
	send(t, conn, netproto.MsgComplaintClear, netproto.ComplaintClear{TargetUniqueID: "admin-uid"})
	if e := readError(t, conn); e.Code != errCodePermissionDenied {
		t.Fatalf("clear error = %d, want %d", e.Code, errCodePermissionDenied)
	}
}

// TestComplaintReviewBanPermission verifies b_client_ban, not admin status,
// is enough to review complaints.
func TestComplaintReviewBanPermission(t *testing.T) {
	tp := tieredWith(boolPerm(permissions.PermissionKeyClientBan, true))
	env := startTestEnv(t, &tp)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()
	send(t, conn, netproto.MsgComplaintList, netproto.ComplaintList{})

	f := readOfType(t, conn, netproto.MsgComplaints)
	var resp netproto.Complaints
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode complaints: %v", err)
	}
	if len(resp.Entries) != 0 {
		t.Fatalf("entries = %+v", resp.Entries)
	}
}

// --- token management (174) --------------------------------------------------

// TestTokenManagement verifies the list/add/delete round-trip: the server
// mints the key, resolves the group name, audits, and replies with the
// refreshed list every time.
func TestTokenManagement(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	gid, err := env.groups.CreateGroup(context.Background(), "server", "Moderator", 0)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	conn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgTokenList, netproto.TokenList{})
	f := readOfType(t, conn, netproto.MsgTokens)
	var resp netproto.Tokens
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode tokens: %v", err)
	}
	if len(resp.Entries) != 0 {
		t.Fatalf("initial entries = %+v", resp.Entries)
	}

	send(t, conn, netproto.MsgTokenAdd, netproto.TokenAdd{
		GroupID: gid, ChannelID: 7, Description: "for the new mod",
	})
	f = readOfType(t, conn, netproto.MsgTokens)
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode tokens: %v", err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("after add entries = %+v", resp.Entries)
	}
	e := resp.Entries[0]
	if e.Token == "" {
		t.Fatal("server did not generate a token key")
	}
	if e.GroupID != gid || e.GroupName != "Moderator" {
		t.Fatalf("group not resolved: %+v", e)
	}
	if e.ChannelID != 7 || e.Description != "for the new mod" {
		t.Fatalf("metadata lost: %+v", e)
	}

	send(t, conn, netproto.MsgTokenDelete, netproto.TokenDelete{Token: e.Token})
	f = readOfType(t, conn, netproto.MsgTokens)
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode tokens: %v", err)
	}
	if len(resp.Entries) != 0 {
		t.Fatalf("after delete entries = %+v", resp.Entries)
	}

	send(t, conn, netproto.MsgTokenDelete, netproto.TokenDelete{Token: "nope"})
	if ferr := readError(t, conn); ferr.Code != errCodeNotFound {
		t.Fatalf("unknown token delete error = %d, want %d", ferr.Code, errCodeNotFound)
	}

	actions := env.groups.auditActions()
	var add, del int
	for _, a := range actions {
		switch a {
		case "token_add":
			add++
		case "token_delete":
			del++
		}
	}
	if add != 1 || del != 1 {
		t.Fatalf("audit actions = %v", actions)
	}
}

// TestTokenManagementDenied verifies each operation is gated by its own
// b_virtualserver_token_* key.
func TestTokenManagementDenied(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	for _, tc := range []struct {
		name string
		mt   netproto.MessageType
		msg  any
	}{
		{"list", netproto.MsgTokenList, netproto.TokenList{}},
		{"add", netproto.MsgTokenAdd, netproto.TokenAdd{GroupID: 3}},
		{"delete", netproto.MsgTokenDelete, netproto.TokenDelete{Token: "x"}},
	} {
		send(t, conn, tc.mt, tc.msg)
		if e := readError(t, conn); e.Code != errCodePermissionDenied {
			t.Fatalf("%s error = %d, want %d", tc.name, e.Code, errCodePermissionDenied)
		}
	}
}

// TestTokenAddGranted verifies b_virtualserver_token_add alone lets a
// non-admin mint a group token, but not a group-less admin token.
func TestTokenAddGranted(t *testing.T) {
	tp := tieredWith(boolPerm(permissions.PermissionKeyVirtualserverTokenAdd, true))
	env := startTestEnv(t, &tp)
	defer env.stop()

	gid, err := env.groups.CreateGroup(context.Background(), "server", "Moderator", 0)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgTokenAdd, netproto.TokenAdd{GroupID: gid})
	f := readOfType(t, conn, netproto.MsgTokens)
	var resp netproto.Tokens
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode tokens: %v", err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("entries = %+v", resp.Entries)
	}

	// An admin-granting (group-less) token needs more than the add key.
	send(t, conn, netproto.MsgTokenAdd, netproto.TokenAdd{})
	if e := readError(t, conn); e.Code != errCodePermissionDenied {
		t.Fatalf("admin token error = %d, want %d", e.Code, errCodePermissionDenied)
	}

	// A token for a group that does not exist is refused.
	send(t, conn, netproto.MsgTokenAdd, netproto.TokenAdd{GroupID: 4242})
	if e := readError(t, conn); e.Code != errCodeNotFound {
		t.Fatalf("unknown group error = %d, want %d", e.Code, errCodeNotFound)
	}
}

// TestTokenUseGuestPromotes verifies every connected identity can redeem a
// token and a guest becomes a durable user before the grant is applied.
func TestTokenUseGuestPromotes(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	env.tokens.grants = map[string]int64{"tok-abc": 5}

	conn := dialRetry(t, env.addr)
	defer conn.Close()
	send(t, conn, netproto.MsgAuthenticate, netproto.Authenticate{Anonymous: true, Nickname: "guest"})
	f := readOfType(t, conn, netproto.MsgAuthResponse)
	var ar netproto.AuthResponse
	if err := netproto.Decode(f, &ar); err != nil {
		t.Fatalf("decode auth: %v", err)
	}
	if !ar.OK {
		t.Fatalf("guest auth failed: %s", ar.Reason)
	}

	send(t, conn, netproto.MsgTokenUse, netproto.TokenUse{Token: "tok-abc"})
	data := readEventOfType(t, conn, eventTokenUsed)
	var event struct {
		ClientID string `json:"client_id"`
		GroupID  int64  `json:"group_id"`
		Promoted bool   `json:"promoted"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("decode token event: %v", err)
	}
	if event.ClientID != ar.ClientID || event.GroupID != 5 || !event.Promoted {
		t.Fatalf("guest token event = %+v", event)
	}
	env.tokens.mu.Lock()
	defer env.tokens.mu.Unlock()
	if len(env.tokens.used) != 1 || env.tokens.used[0] != "tok-abc" {
		t.Fatalf("guest redemption did not reach the store: %v", env.tokens.used)
	}
}

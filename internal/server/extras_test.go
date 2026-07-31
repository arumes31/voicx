// extras_test.go covers the Phase 9 control handlers: server password,
// avatars, channel icons, token use, complaints, and screen share.
package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"

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
}

func (f *fakeTokens) UseToken(_ context.Context, key string, _ int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.used = append(f.used, key)
	if f.exhausted[key] {
		return 0, store.ErrTokenExhausted
	}
	g, ok := f.grants[key]
	if !ok {
		return 0, store.ErrTokenNotFound
	}
	return g, nil
}

// fakeComplaints implements ComplaintBackend, enforcing the open-complaint
// limit per reporter like the store does.
type fakeComplaints struct {
	mu     sync.Mutex
	byUser map[string][]string // reporter -> reasons
}

func (f *fakeComplaints) AddComplaint(_ context.Context, reporter, target, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byUser == nil {
		f.byUser = make(map[string][]string)
	}
	if len(f.byUser[reporter]) >= store.MaxOpenComplaints {
		return store.ErrComplaintLimit
	}
	f.byUser[reporter] = append(f.byUser[reporter], target+":"+reason)
	return nil
}

func (f *fakeComplaints) count(reporter string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.byUser[reporter])
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
	env := startTestEnv(t, nil)
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
	env := startTestEnv(t, nil)
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
	env := startTestEnv(t, nil)
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

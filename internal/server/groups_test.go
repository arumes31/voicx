// groups_test.go exercises the wave-6a permission/group management handlers
// over real TCP connections with a fake group store: gates, writes, cache
// invalidation, grant-cap semantics, the resolver trace, the audit log,
// default groups, and the timed-membership reaper.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"voicx/internal/config"
	"voicx/internal/netproto"
	"voicx/internal/permissions"
	"voicx/internal/store"
)

// --- fake group store --------------------------------------------------------

// fakeGroupPerm records one written permission entry.
type fakeGroupPerm struct {
	value  int
	grant  int
	skip   bool
	negate bool
}

// fakeGroups implements GroupStore in memory.
type fakeGroups struct {
	mu     sync.Mutex
	nextID int64
	groups map[string]map[int64]*store.Group // type -> id -> group

	// serverMembers: groupID -> userID -> expiry (nil = permanent).
	serverMembers map[int64]map[int64]*time.Time
	// channelMembers: (userID, channelID) -> groupID.
	channelMembers map[[2]int64]int64

	// perms: "tier|groupID|userID|channelID|key" -> entry.
	perms map[string]fakeGroupPerm

	audit     []store.AuditEntry
	uniqueIDs map[int64]string
}

func newFakeGroups() *fakeGroups {
	return &fakeGroups{
		nextID:         10,
		groups:         map[string]map[int64]*store.Group{"server": {}, "channel": {}},
		serverMembers:  map[int64]map[int64]*time.Time{},
		channelMembers: map[[2]int64]int64{},
		perms:          map[string]fakeGroupPerm{},
		uniqueIDs:      map[int64]string{1: "admin-uid", 2: "user-uid"},
	}
}

func (f *fakeGroups) memberCount(groupType string, id int64) int {
	if groupType == "channel" {
		n := 0
		for _, gid := range f.channelMembers {
			if gid == id {
				n++
			}
		}
		return n
	}
	return len(f.serverMembers[id])
}

func (f *fakeGroups) ListGroups(_ context.Context, groupType string) ([]store.Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Group
	for _, g := range f.groups[groupType] {
		cp := *g
		cp.MemberCount = f.memberCount(groupType, cp.ID)
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SortID != out[j].SortID {
			return out[i].SortID < out[j].SortID
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (f *fakeGroups) GetGroup(_ context.Context, groupType string, id int64) (*store.Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if g, ok := f.groups[groupType][id]; ok {
		cp := *g
		return &cp, nil
	}
	return nil, nil
}

func (f *fakeGroups) CreateGroup(_ context.Context, groupType, name string, sortID int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.groups[groupType][f.nextID] = &store.Group{ID: f.nextID, Name: name, SortID: sortID}
	return f.nextID, nil
}

func (f *fakeGroups) RenameGroup(_ context.Context, groupType string, id int64, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.groups[groupType][id]
	if !ok {
		return fmt.Errorf("group not found")
	}
	g.Name = name
	return nil
}

func (f *fakeGroups) DeleteGroup(_ context.Context, groupType string, id int64, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.groups[groupType][id]; !ok {
		return fmt.Errorf("group not found")
	}
	if n := f.memberCount(groupType, id); n > 0 && !force {
		return fmt.Errorf("group has %d members (use force)", n)
	}
	delete(f.groups[groupType], id)
	delete(f.serverMembers, id)
	for k, gid := range f.channelMembers {
		if gid == id {
			delete(f.channelMembers, k)
		}
	}
	return nil
}

func (f *fakeGroups) AssignServerGroup(_ context.Context, groupID, userID int64, expiresIn time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.groups["server"][groupID]; !ok {
		return fmt.Errorf("group not found")
	}
	if f.serverMembers[groupID] == nil {
		f.serverMembers[groupID] = map[int64]*time.Time{}
	}
	var expires *time.Time
	if expiresIn > 0 {
		t := time.Now().Add(expiresIn)
		expires = &t
	}
	f.serverMembers[groupID][userID] = expires
	return nil
}

func (f *fakeGroups) UnassignServerGroup(_ context.Context, groupID, userID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.serverMembers[groupID], userID)
	return nil
}

func (f *fakeGroups) AssignChannelGroup(_ context.Context, groupID, userID, channelID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.groups["channel"][groupID]; !ok {
		return fmt.Errorf("group not found")
	}
	f.channelMembers[[2]int64{userID, channelID}] = groupID
	return nil
}

func (f *fakeGroups) UnassignChannelGroup(_ context.Context, userID, channelID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.channelMembers, [2]int64{userID, channelID})
	return nil
}

func (f *fakeGroups) UserGroupIDs(_ context.Context, userID int64) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int64
	for gid, members := range f.serverMembers {
		if _, ok := members[userID]; ok {
			out = append(out, gid)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func (f *fakeGroups) FindGroupByName(_ context.Context, groupType, name string) (*store.Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, g := range f.groups[groupType] {
		if g.Name == name {
			cp := *g
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeGroups) ExpiredGroupMembers(_ context.Context, now time.Time) ([][2]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][2]int64
	for gid, members := range f.serverMembers {
		for uid, expires := range members {
			if expires != nil && !expires.After(now) {
				out = append(out, [2]int64{uid, gid})
				delete(members, uid)
			}
		}
	}
	return out, nil
}

func (f *fakeGroups) UserUniqueID(_ context.Context, userID int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if uid, ok := f.uniqueIDs[userID]; ok {
		return uid, nil
	}
	return "", fmt.Errorf("user not found")
}

// SetGroupIcon marks the icon on the fake group.
func (f *fakeGroups) SetGroupIcon(_ context.Context, groupID int64, icon string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.groups["server"][groupID]
	if !ok {
		return fmt.Errorf("group not found")
	}
	g.Icon = icon
	return nil
}

// permKey builds the fake store key for a written permission entry.
func permKey(tier store.PermTier, target store.PermTarget, key string) string {
	return fmt.Sprintf("%s|%d|%d|%d|%s", tier, target.GroupID, target.UserID, target.ChannelID, key)
}

func (f *fakeGroups) SetPermission(_ context.Context, tier store.PermTier, target store.PermTarget, key string, value, grant int, skip, negate bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.perms[permKey(tier, target, key)] = fakeGroupPerm{value: value, grant: grant, skip: skip, negate: negate}
	return nil
}

func (f *fakeGroups) UnsetPermission(_ context.Context, tier store.PermTier, target store.PermTarget, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.perms, permKey(tier, target, key))
	return nil
}

// ListPermissions returns the fake entries for a target on a tier.
func (f *fakeGroups) ListPermissions(_ context.Context, tier store.PermTier, target store.PermTarget) ([]store.PermEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := fmt.Sprintf("%s|%d|%d|%d|", tier, target.GroupID, target.UserID, target.ChannelID)
	var out []store.PermEntry
	for k, p := range f.perms {
		if rest, ok := strings.CutPrefix(k, prefix); ok {
			out = append(out, store.PermEntry{Key: rest, Value: p.value, Grant: p.grant, Skip: p.skip, Negate: p.negate})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (f *fakeGroups) Audit(_ context.Context, actor, action, target, detail string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audit = append(f.audit, store.AuditEntry{
		ID: int64(len(f.audit) + 1), Actor: actor, Action: action,
		Target: target, Detail: detail, CreatedAt: time.Now(),
	})
}

func (f *fakeGroups) AuditList(_ context.Context, beforeID int64, limit int) ([]store.AuditEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var out []store.AuditEntry
	for i := len(f.audit) - 1; i >= 0 && len(out) < limit; i-- {
		if beforeID > 0 && f.audit[i].ID >= beforeID {
			continue
		}
		out = append(out, f.audit[i])
	}
	return out, nil
}

// seedGroup plants a group with a fixed ID (default-group tests).
func (f *fakeGroups) seedGroup(groupType string, id int64, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups[groupType][id] = &store.Group{ID: id, Name: name}
}

// hasServerMember reports whether userID is in the server group.
func (f *fakeGroups) hasServerMember(groupID, userID int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.serverMembers[groupID][userID]
	return ok
}

// seedExpiredMember plants an already-expired membership (reaper tests).
func (f *fakeGroups) seedExpiredMember(groupID, userID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.serverMembers[groupID] == nil {
		f.serverMembers[groupID] = map[int64]*time.Time{}
	}
	past := time.Now().Add(-time.Minute)
	f.serverMembers[groupID][userID] = &past
}

// auditActions returns the recorded audit actions in order.
func (f *fakeGroups) auditActions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.audit))
	for _, e := range f.audit {
		out = append(out, e.Action)
	}
	return out
}

// ListServerGroupMembers returns the fake server-group members.
func (f *fakeGroups) ListServerGroupMembers(_ context.Context, groupID int64) ([]store.GroupMember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.GroupMember
	for userID, expires := range f.serverMembers[groupID] {
		gm := store.GroupMember{UniqueID: f.uniqueIDs[userID], Nickname: "user", ExpiresAt: expires}
		out = append(out, gm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UniqueID < out[j].UniqueID })
	return out, nil
}

// ListChannelGroupMembers returns the fake channel-group members.
func (f *fakeGroups) ListChannelGroupMembers(_ context.Context, groupID, channelID int64) ([]store.GroupMember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.GroupMember
	for key, gid := range f.channelMembers {
		if gid != groupID || key[1] != channelID {
			continue
		}
		out = append(out, store.GroupMember{UniqueID: f.uniqueIDs[key[0]], Nickname: "user"})
	}
	return out, nil
}

// --- helpers -----------------------------------------------------------------

// readError reads the next MsgError frame and decodes it.
func readError(t *testing.T, conn net.Conn) netproto.Error {
	t.Helper()
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error frame: %v", err)
	}
	return e
}

// invalidateAllRecorded reports whether a full cache invalidation happened.
func (f *fakePerms) invalidateAllRecorded() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, inv := range f.invalidations {
		if inv[0] == -1 {
			return true
		}
	}
	return false
}

// --- group CRUD tests --------------------------------------------------------

// TestGroupCreateListRenameDelete covers the group CRUD happy path as admin.
func TestGroupCreateListRenameDelete(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	conn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer conn.Close()

	// Empty list first.
	send(t, conn, netproto.MsgGroupList, netproto.GroupList{Type: "server"})
	f := readOfType(t, conn, netproto.MsgGroupListResponse)
	var list netproto.GroupListResponse
	if err := netproto.Decode(f, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Groups) != 0 {
		t.Fatalf("groups = %+v, want empty", list.Groups)
	}

	// Create returns the refreshed list containing the new group.
	send(t, conn, netproto.MsgGroupCreate, netproto.GroupCreate{Type: "server", Name: "Mods", SortID: 5})
	f = readOfType(t, conn, netproto.MsgGroupListResponse)
	if err := netproto.Decode(f, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Groups) != 1 || list.Groups[0].Name != "Mods" || list.Groups[0].SortID != 5 {
		t.Fatalf("groups after create = %+v", list.Groups)
	}
	groupID := list.Groups[0].ID

	// Rename (no response frame: poll the store).
	send(t, conn, netproto.MsgGroupRename, netproto.GroupRename{Type: "server", GroupID: groupID, Name: "Moderators"})
	waitFor(t, "group renamed", func() bool {
		g, _ := env.groups.GetGroup(context.Background(), "server", groupID)
		return g != nil && g.Name == "Moderators"
	})

	// Delete.
	send(t, conn, netproto.MsgGroupDelete, netproto.GroupDelete{Type: "server", GroupID: groupID})
	waitFor(t, "group deleted", func() bool {
		g, _ := env.groups.GetGroup(context.Background(), "server", groupID)
		return g == nil
	})

	// All three writes were audited in order.
	want := []string{"group_create", "group_rename", "group_delete"}
	got := env.groups.auditActions()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("audit actions = %v, want %v", got, want)
	}
}

// TestGroupManageGate verifies deny-on-unset for non-admins and that the
// b_server_group_manage / b_channel_group_manage keys open the gate.
func TestGroupManageGate(t *testing.T) {
	// No permissions at all: everything is denied.
	env := startTestEnv(t, nil)
	defer env.stop()
	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgGroupCreate, netproto.GroupCreate{Type: "server", Name: "Nope"})
	if e := readError(t, conn); e.Code != errCodePermissionDenied {
		t.Fatalf("error = %+v, want permission denied", e)
	}

	// With b_server_group_manage the server-type gate opens; the channel-type
	// gate stays closed (separate key).
	tp := tieredWith(boolPerm(permissions.PermissionKeyServerGroupManage, true))
	env2 := startTestEnv(t, &tp)
	defer env2.stop()
	conn2, _ := dialAuthed(t, env2.addr, "user-uid")
	defer conn2.Close()

	send(t, conn2, netproto.MsgGroupCreate, netproto.GroupCreate{Type: "server", Name: "OK"})
	readOfType(t, conn2, netproto.MsgGroupListResponse)

	send(t, conn2, netproto.MsgGroupCreate, netproto.GroupCreate{Type: "channel", Name: "Nope"})
	if e := readError(t, conn2); e.Code != errCodePermissionDenied {
		t.Fatalf("error = %+v, want permission denied", e)
	}
}

// TestGroupDeleteRequiresForce verifies a non-empty group survives delete
// without force.
func TestGroupDeleteRequiresForce(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	conn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer conn.Close()

	gid, err := env.groups.CreateGroup(context.Background(), "server", "Busy", 0)
	if err != nil {
		t.Fatalf("seed group: %v", err)
	}
	if err := env.groups.AssignServerGroup(context.Background(), gid, 2, 0); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	send(t, conn, netproto.MsgGroupDelete, netproto.GroupDelete{Type: "server", GroupID: gid})
	if e := readError(t, conn); e.Code != errCodeMalformed {
		t.Fatalf("error = %+v, want refusal", e)
	}
	if !env.groups.hasServerMember(gid, 2) {
		t.Fatal("member lost after refused delete")
	}

	send(t, conn, netproto.MsgGroupDelete, netproto.GroupDelete{Type: "server", GroupID: gid, Force: true})
	waitFor(t, "group force-deleted", func() bool {
		g, _ := env.groups.GetGroup(context.Background(), "server", gid)
		return g == nil
	})
	if !env.perms.invalidateAllRecorded() {
		t.Fatal("no cache invalidation after force delete")
	}
}

// --- membership tests --------------------------------------------------------

// TestGroupAssign verifies the write, the cache invalidation, the audit row,
// and the event to the target and the acting admin.
func TestGroupAssign(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer adminConn.Close()
	userConn, _ := dialAuthed(t, env.addr, "user-uid")
	defer userConn.Close()

	gid, _ := env.groups.CreateGroup(context.Background(), "server", "VIPs", 0)

	send(t, adminConn, netproto.MsgGroupAssign, netproto.GroupAssign{
		Type: "server", GroupID: gid, UniqueID: "user-uid", ExpiresInSeconds: 3600,
	})

	// The target and the admin both receive the event.
	data := readEventOfType(t, userConn, "group_assigned")
	var evt struct {
		GroupID   int64  `json:"group_id"`
		GroupName string `json:"group_name"`
	}
	if err := json.Unmarshal(data, &evt); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if evt.GroupID != gid || evt.GroupName != "VIPs" {
		t.Fatalf("event = %+v", evt)
	}
	readEventOfType(t, adminConn, "group_assigned")

	if !env.groups.hasServerMember(gid, 2) {
		t.Fatal("membership not recorded")
	}
	if !env.perms.invalidateAllRecorded() {
		t.Fatal("no cache invalidation after assign")
	}
	if got := env.groups.auditActions(); len(got) == 0 || got[len(got)-1] != "group_assign" {
		t.Fatalf("audit actions = %v", got)
	}

	// Unassign reverses it.
	send(t, adminConn, netproto.MsgGroupUnassign, netproto.GroupUnassign{
		Type: "server", GroupID: gid, UniqueID: "user-uid",
	})
	readEventOfType(t, userConn, "group_unassigned")
	waitFor(t, "membership removed", func() bool { return !env.groups.hasServerMember(gid, 2) })
}

// TestGroupAssignChannelRequiresChannelID verifies channel-group addressing.
func TestGroupAssignChannelRequiresChannelID(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	conn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer conn.Close()

	gid, _ := env.groups.CreateGroup(context.Background(), "channel", "ChanOps", 0)

	send(t, conn, netproto.MsgGroupAssign, netproto.GroupAssign{Type: "channel", GroupID: gid, UniqueID: "user-uid"})
	if e := readError(t, conn); e.Code != errCodeMalformed {
		t.Fatalf("error = %+v, want malformed (channel_id required)", e)
	}

	send(t, conn, netproto.MsgGroupAssign, netproto.GroupAssign{Type: "channel", GroupID: gid, UniqueID: "user-uid", ChannelID: 1})
	readEventOfType(t, conn, "group_assigned")
	env.groups.mu.Lock()
	got := env.groups.channelMembers[[2]int64{2, 1}]
	env.groups.mu.Unlock()
	if got != gid {
		t.Fatalf("channel membership group = %d, want %d", got, gid)
	}
}

// --- permission write path tests ----------------------------------------------

// TestPermSetUnset verifies writes across tiers, the audit rows, and the
// cache invalidation.
func TestPermSetUnset(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	conn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgPermSet, netproto.PermSet{
		Tier: "server_group", GroupID: 3, Key: "i_client_talk_power", Value: 10, Grant: 50,
	})
	// No response frame on success: poll the fake store.
	key := permKey(store.PermTierServerGroup, store.PermTarget{GroupID: 3}, "i_client_talk_power")
	waitFor(t, "perm written", func() bool {
		env.groups.mu.Lock()
		defer env.groups.mu.Unlock()
		p, ok := env.groups.perms[key]
		return ok && p.value == 10 && p.grant == 50
	})
	if !env.perms.invalidateAllRecorded() {
		t.Fatal("no cache invalidation after perm set")
	}

	// Client tier addressing resolves the unique ID to a user ID.
	send(t, conn, netproto.MsgPermSet, netproto.PermSet{
		Tier: "client", UniqueID: "user-uid", Key: "b_client_video_publish", Value: 0, Negate: true,
	})
	key = permKey(store.PermTierClient, store.PermTarget{UserID: 2}, "b_client_video_publish")
	waitFor(t, "client perm written", func() bool {
		env.groups.mu.Lock()
		defer env.groups.mu.Unlock()
		p, ok := env.groups.perms[key]
		return ok && p.negate
	})

	// Unset removes it again.
	send(t, conn, netproto.MsgPermUnset, netproto.PermUnset{
		Tier: "client", UniqueID: "user-uid", Key: "b_client_video_publish",
	})
	waitFor(t, "client perm removed", func() bool {
		env.groups.mu.Lock()
		defer env.groups.mu.Unlock()
		_, ok := env.groups.perms[key]
		return !ok
	})

	want := []string{"perm_set", "perm_set", "perm_unset"}
	if got := env.groups.auditActions(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("audit actions = %v, want %v", got, want)
	}
}

// TestPermSetGate verifies b_permission_manage is required (deny-on-unset).
func TestPermSetGate(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgPermSet, netproto.PermSet{
		Tier: "server_group", GroupID: 3, Key: "i_client_talk_power", Value: 1,
	})
	if e := readError(t, conn); e.Code != errCodePermissionDenied {
		t.Fatalf("error = %+v, want permission denied", e)
	}
}

// TestPermSetGrantCap verifies the TS3-lite grant semantics: a non-admin may
// only set value/grant <= their own grant for that key.
func TestPermSetGrantCap(t *testing.T) {
	manage := boolPerm(permissions.PermissionKeyPermissionManage, true)
	own := &permissions.Permission{
		Key:   "i_channel_modify_power",
		Type:  permissions.PermissionTypeInteger,
		Value: 5,
		Grant: 5,
	}
	tp := tieredWith(manage, own)
	env := startTestEnv(t, &tp)
	defer env.stop()
	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	// Within the cap: allowed.
	send(t, conn, netproto.MsgPermSet, netproto.PermSet{
		Tier: "server_group", GroupID: 3, Key: "i_channel_modify_power", Value: 5, Grant: 5,
	})
	key := permKey(store.PermTierServerGroup, store.PermTarget{GroupID: 3}, "i_channel_modify_power")
	waitFor(t, "capped perm written", func() bool {
		env.groups.mu.Lock()
		defer env.groups.mu.Unlock()
		_, ok := env.groups.perms[key]
		return ok
	})

	// Above the cap: denied.
	send(t, conn, netproto.MsgPermSet, netproto.PermSet{
		Tier: "server_group", GroupID: 3, Key: "i_channel_modify_power", Value: 6,
	})
	if e := readError(t, conn); e.Code != errCodePermissionDenied {
		t.Fatalf("error = %+v, want grant-cap denial", e)
	}

	// A key the caller holds no grant for at all: denied.
	send(t, conn, netproto.MsgPermSet, netproto.PermSet{
		Tier: "server_group", GroupID: 3, Key: "i_client_ban_power", Value: 1,
	})
	if e := readError(t, conn); e.Code != errCodePermissionDenied {
		t.Fatalf("error = %+v, want grant-cap denial", e)
	}
}

// TestPermUnsetGrantCap verifies the grant cap guards removals against the
// entry being deleted: an under-granted caller is refused (it must not be able
// to strip an admin group of a permission it could never set), and a caller
// whose own grant covers the entry succeeds.
func TestPermUnsetGrantCap(t *testing.T) {
	ctx := context.Background()
	target := store.PermTarget{GroupID: 3}

	// Caller with a grant of 5 for i_channel_modify_power and none for
	// i_client_ban_power.
	tp := tieredWith(
		boolPerm(permissions.PermissionKeyPermissionManage, true),
		&permissions.Permission{Key: "i_channel_modify_power", Type: permissions.PermissionTypeInteger, Value: 5, Grant: 5},
	)
	env := startTestEnv(t, &tp)
	defer env.stop()
	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	for _, key := range []string{"i_channel_modify_power", "i_client_ban_power"} {
		if err := env.groups.SetPermission(ctx, store.PermTierServerGroup, target, key, 100, 100, false, false); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}

	// No grant for the key at all: denied.
	send(t, conn, netproto.MsgPermUnset, netproto.PermUnset{
		Tier: "server_group", GroupID: 3, Key: "i_client_ban_power",
	})
	if e := readError(t, conn); e.Code != errCodePermissionDenied {
		t.Fatalf("error = %+v, want grant-cap denial", e)
	}

	// A grant of 5 against an entry granted 100: denied.
	send(t, conn, netproto.MsgPermUnset, netproto.PermUnset{
		Tier: "server_group", GroupID: 3, Key: "i_channel_modify_power",
	})
	if e := readError(t, conn); e.Code != errCodePermissionDenied {
		t.Fatalf("error = %+v, want grant-cap denial", e)
	}
	for _, key := range []string{"i_client_ban_power", "i_channel_modify_power"} {
		env.groups.mu.Lock()
		_, survived := env.groups.perms[permKey(store.PermTierServerGroup, target, key)]
		env.groups.mu.Unlock()
		if !survived {
			t.Fatalf("%s was unset by an under-granted caller", key)
		}
	}

	// Same entry, caller whose own grant covers it: allowed.
	strong := tieredWith(
		boolPerm(permissions.PermissionKeyPermissionManage, true),
		&permissions.Permission{Key: "i_channel_modify_power", Type: permissions.PermissionTypeInteger, Value: 100, Grant: 100},
	)
	env2 := startTestEnv(t, &strong)
	defer env2.stop()
	conn2, _ := dialAuthed(t, env2.addr, "user-uid")
	defer conn2.Close()

	if err := env2.groups.SetPermission(ctx, store.PermTierServerGroup, target, "i_channel_modify_power", 100, 100, false, false); err != nil {
		t.Fatalf("seed: %v", err)
	}
	send(t, conn2, netproto.MsgPermUnset, netproto.PermUnset{
		Tier: "server_group", GroupID: 3, Key: "i_channel_modify_power",
	})
	waitFor(t, "capped perm removed", func() bool {
		env2.groups.mu.Lock()
		defer env2.groups.mu.Unlock()
		_, ok := env2.groups.perms[permKey(store.PermTierServerGroup, target, "i_channel_modify_power")]
		return !ok
	})
}

// TestPermTemplateApply verifies the built-in templates flow through the
// write path.
func TestPermTemplateApply(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	conn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgPermTemplateApply, netproto.PermTemplateApply{
		Template: "moderator", Tier: "server_group", GroupID: 4,
	})
	waitFor(t, "template applied", func() bool {
		env.groups.mu.Lock()
		defer env.groups.mu.Unlock()
		_, ok := env.groups.perms[permKey(store.PermTierServerGroup, store.PermTarget{GroupID: 4}, "b_chat_delete_any")]
		return ok
	})
	env.groups.mu.Lock()
	n := 0
	for k := range env.groups.perms {
		if len(k) > 0 && k[:len("server_group|4|")] == "server_group|4|" {
			n++
		}
	}
	env.groups.mu.Unlock()
	if n != len(permTemplates["moderator"]) {
		t.Fatalf("template wrote %d entries, want %d", n, len(permTemplates["moderator"]))
	}

	// Unknown template: rejected.
	send(t, conn, netproto.MsgPermTemplateApply, netproto.PermTemplateApply{
		Template: "superuser", Tier: "server_group", GroupID: 4,
	})
	if e := readError(t, conn); e.Code != errCodeMalformed {
		t.Fatalf("error = %+v, want malformed", e)
	}
}

// TestPermTemplateApplyGrantCap verifies a b_permission_manage holder cannot
// escalate through the admin template: every key it carries is capped.
func TestPermTemplateApplyGrantCap(t *testing.T) {
	tp := tieredWith(boolPerm(permissions.PermissionKeyPermissionManage, true))
	env := startTestEnv(t, &tp)
	defer env.stop()
	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgPermTemplateApply, netproto.PermTemplateApply{
		Template: "admin", Tier: "server_group", GroupID: 4,
	})
	if e := readError(t, conn); e.Code != errCodePermissionDenied {
		t.Fatalf("error = %+v, want grant-cap denial", e)
	}
	env.groups.mu.Lock()
	n := len(env.groups.perms)
	env.groups.mu.Unlock()
	if n != 0 {
		t.Fatalf("denied template wrote %d entries", n)
	}
}

// TestPermTemplateApplyOverwriteGrantCap verifies the template path also caps
// against the entries it would replace: a caller that may write the template's
// values still cannot overwrite an entry granted above its own level.
func TestPermTemplateApplyOverwriteGrantCap(t *testing.T) {
	perms := []*permissions.Permission{boolPerm(permissions.PermissionKeyPermissionManage, true)}
	for key := range permTemplates["moderator"] {
		perms = append(perms, &permissions.Permission{
			Key: permissions.PermissionKey(key), Type: permissions.PermissionTypeBoolean, Value: 1, Grant: 1,
		})
	}
	tp := tieredWith(perms...)
	env := startTestEnv(t, &tp)
	defer env.stop()
	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	// Untouched target: every key is within the caller's grant, so it lands.
	send(t, conn, netproto.MsgPermTemplateApply, netproto.PermTemplateApply{
		Template: "moderator", Tier: "server_group", GroupID: 4,
	})
	waitFor(t, "template applied", func() bool {
		env.groups.mu.Lock()
		defer env.groups.mu.Unlock()
		_, ok := env.groups.perms[permKey(store.PermTierServerGroup, store.PermTarget{GroupID: 4}, "b_channel_modify")]
		return ok
	})

	// Target holding one of the template's keys above the caller's grant: the
	// whole apply is refused and the target is left alone.
	target := store.PermTarget{GroupID: 5}
	if err := env.groups.SetPermission(context.Background(), store.PermTierServerGroup, target, "b_channel_modify", 1, 100, false, false); err != nil {
		t.Fatalf("seed: %v", err)
	}
	send(t, conn, netproto.MsgPermTemplateApply, netproto.PermTemplateApply{
		Template: "moderator", Tier: "server_group", GroupID: 5,
	})
	if e := readError(t, conn); e.Code != errCodePermissionDenied {
		t.Fatalf("error = %+v, want grant-cap denial", e)
	}
	env.groups.mu.Lock()
	n := 0
	for k := range env.groups.perms {
		if strings.HasPrefix(k, "server_group|5|") {
			n++
		}
	}
	kept := env.groups.perms[permKey(store.PermTierServerGroup, target, "b_channel_modify")]
	env.groups.mu.Unlock()
	if n != 1 || kept.grant != 100 {
		t.Fatalf("denied template touched the target: %d entries, b_channel_modify grant = %d", n, kept.grant)
	}
}

// --- trace tests --------------------------------------------------------------

// TestPermTrace verifies the winning tier and all contributing entries match
// the resolver's precedence (server group wins over the channel tier).
func TestPermTrace(t *testing.T) {
	tp := permissions.NewTieredPermissions()
	sg := permissions.NewPermissionSet()
	sg.Set(&permissions.Permission{Key: "i_client_talk_power", Type: permissions.PermissionTypeInteger, Value: 10, Grant: 50})
	tp.Set(permissions.TierServerGroup, sg)
	ch := permissions.NewPermissionSet()
	ch.Set(&permissions.Permission{Key: "i_client_talk_power", Type: permissions.PermissionTypeInteger, Value: 99})
	tp.Set(permissions.TierChannel, ch)

	env := startTestEnv(t, &tp)
	defer env.stop()
	conn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgPermTrace, netproto.PermTrace{UniqueID: "user-uid", Key: "i_client_talk_power", ChannelID: 1})
	f := readOfType(t, conn, netproto.MsgPermTraceResponse)
	var resp netproto.PermTraceResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode trace: %v", err)
	}
	if resp.Effective != 10 || resp.EffectiveTier != "server_group" {
		t.Fatalf("effective = %d/%q, want 10/server_group", resp.Effective, resp.EffectiveTier)
	}
	var sgEntry, chEntry *netproto.PermTraceEntry
	for i := range resp.Entries {
		switch resp.Entries[i].Tier {
		case "server_group":
			sgEntry = &resp.Entries[i]
		case "channel":
			chEntry = &resp.Entries[i]
		}
	}
	if sgEntry == nil || !sgEntry.Present || !sgEntry.Winning || sgEntry.Value != 10 || sgEntry.Grant != 50 {
		t.Fatalf("server_group entry = %+v", sgEntry)
	}
	if chEntry == nil || !chEntry.Present || chEntry.Winning || chEntry.Value != 99 {
		t.Fatalf("channel entry = %+v", chEntry)
	}
}

// TestPermTraceGate verifies the trace is a permission-management view.
func TestPermTraceGate(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgPermTrace, netproto.PermTrace{UniqueID: "user-uid", Key: "i_client_talk_power"})
	if e := readError(t, conn); e.Code != errCodePermissionDenied {
		t.Fatalf("error = %+v, want permission denied", e)
	}
}

// --- audit log tests ----------------------------------------------------------

// TestAuditLog verifies the gate, paging, and newest-first order.
func TestAuditLog(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	// Non-admin without b_audit_view: denied.
	userConn, _ := dialAuthed(t, env.addr, "user-uid")
	defer userConn.Close()
	send(t, userConn, netproto.MsgAuditLog, netproto.AuditLog{})
	if e := readError(t, userConn); e.Code != errCodePermissionDenied {
		t.Fatalf("error = %+v, want permission denied", e)
	}

	// Seed three entries via real writes (group creates as admin).
	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer adminConn.Close()
	for _, name := range []string{"g1", "g2", "g3"} {
		send(t, adminConn, netproto.MsgGroupCreate, netproto.GroupCreate{Type: "server", Name: name})
		readOfType(t, adminConn, netproto.MsgGroupListResponse)
	}

	send(t, adminConn, netproto.MsgAuditLog, netproto.AuditLog{Limit: 2})
	f := readOfType(t, adminConn, netproto.MsgAuditLogResponse)
	var page netproto.AuditLogResponse
	if err := netproto.Decode(f, &page); err != nil {
		t.Fatalf("decode audit page: %v", err)
	}
	if len(page.Entries) != 2 {
		t.Fatalf("entries = %+v, want 2", page.Entries)
	}
	if page.Entries[0].Action != "group_create" || page.Entries[0].Target != "server:g3" {
		t.Fatalf("newest entry = %+v", page.Entries[0])
	}
	if page.Entries[0].Actor != "admin-uid" {
		t.Fatalf("actor = %q", page.Entries[0].Actor)
	}

	// Page 2 via before_id.
	send(t, adminConn, netproto.MsgAuditLog, netproto.AuditLog{BeforeID: page.Entries[1].ID, Limit: 10})
	f = readOfType(t, adminConn, netproto.MsgAuditLogResponse)
	if err := netproto.Decode(f, &page); err != nil {
		t.Fatalf("decode audit page 2: %v", err)
	}
	if len(page.Entries) != 1 || page.Entries[0].Target != "server:g1" {
		t.Fatalf("page 2 = %+v", page.Entries)
	}
}

// --- default group tests ------------------------------------------------------

// TestAssignDefaultGroup verifies a registered user with no memberships joins
// the Member group on first login, exactly once.
func TestAssignDefaultGroup(t *testing.T) {
	env := startTestEnvDeps(t, nil, nil, func(d *Deps) {
		d.Groups.(*fakeGroups).seedGroup("server", 7, "Member")
		d.DefaultMemberGroupID = 7
	})
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	conn.Close()
	waitFor(t, "default group assigned", func() bool { return env.groups.hasServerMember(7, 2) })

	// Second login: the user already has a membership, no duplicate work.
	conn2, _ := dialAuthed(t, env.addr, "user-uid")
	conn2.Close()
	ids, err := env.groups.UserGroupIDs(context.Background(), 2)
	if err != nil || len(ids) != 1 || ids[0] != 7 {
		t.Fatalf("groups after relogin = %v, err=%v", ids, err)
	}
}

// TestNoDefaultGroupWhenDisabled verifies assignment is skipped when no
// default group is configured (0).
func TestNoDefaultGroupWhenDisabled(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	conn, _ := dialAuthed(t, env.addr, "user-uid")
	conn.Close()
	ids, err := env.groups.UserGroupIDs(context.Background(), 2)
	if err != nil || len(ids) != 0 {
		t.Fatalf("groups = %v, want none", ids)
	}
}

// TestGuestVirtualGroup verifies guests virtually hold the default Guest
// group's permission set.
func TestGuestVirtualGroup(t *testing.T) {
	set := permissions.NewPermissionSet()
	set.Set(boolPerm(permissions.PermissionKeyChannelCreateTemporary, true))
	fp := &fakePerms{tp: permissions.NewTieredPermissions(), groupSet: set}

	srv := New(&config.Config{}, testLogger(), &Deps{
		Perms:               fp,
		Resolver:            permissions.NewResolver(),
		DefaultGuestGroupID: 3,
	})
	guest := &Client{ID: "guest-1"} // UserID 0 = guest
	pc, err := srv.permCheckerFor(context.Background(), guest)
	if err != nil {
		t.Fatalf("permCheckerFor: %v", err)
	}
	if !pc.granted(permissions.PermissionKeyChannelCreateTemporary) {
		t.Fatal("guest did not inherit the Guest group permission")
	}
	if pc.granted(permissions.PermissionKeyPermissionManage) {
		t.Fatal("guest unexpectedly holds b_permission_manage")
	}

	// Without a configured guest group the set is empty.
	srv2 := New(&config.Config{}, testLogger(), &Deps{
		Perms:    fp,
		Resolver: permissions.NewResolver(),
	})
	pc2, err := srv2.permCheckerFor(context.Background(), guest)
	if err != nil {
		t.Fatalf("permCheckerFor: %v", err)
	}
	if pc2.granted(permissions.PermissionKeyChannelCreateTemporary) {
		t.Fatal("guest holds permissions without a guest group")
	}
}

// --- reaper tests -------------------------------------------------------------

// TestReapExpiredGroups verifies expired memberships are removed, the cache
// is invalidated, and the online target is notified.
func TestReapExpiredGroups(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	userConn, _ := dialAuthed(t, env.addr, "user-uid")
	defer userConn.Close()

	gid, _ := env.groups.CreateGroup(context.Background(), "server", "TempTrial", 0)
	env.groups.seedExpiredMember(gid, 2)

	env.srv.ReapExpiredGroups(context.Background())

	if env.groups.hasServerMember(gid, 2) {
		t.Fatal("expired membership not reaped")
	}
	if !env.perms.invalidateAllRecorded() {
		t.Fatal("no cache invalidation after reaping")
	}
	data := readEventOfType(t, userConn, "group_expired")
	var evt struct {
		GroupID int64 `json:"group_id"`
	}
	if err := json.Unmarshal(data, &evt); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if evt.GroupID != gid {
		t.Fatalf("event group_id = %d, want %d", evt.GroupID, gid)
	}
}

// TestReapKeepsPermanentMembers verifies permanent and future-expiring
// memberships survive the reaper.
func TestReapKeepsPermanentMembers(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	gid, _ := env.groups.CreateGroup(context.Background(), "server", "Keepers", 0)
	if err := env.groups.AssignServerGroup(context.Background(), gid, 2, 0); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := env.groups.AssignServerGroup(context.Background(), gid, 1, time.Hour); err != nil {
		t.Fatalf("assign timed: %v", err)
	}

	env.srv.ReapExpiredGroups(context.Background())

	if !env.groups.hasServerMember(gid, 2) || !env.groups.hasServerMember(gid, 1) {
		t.Fatal("reaper removed a live membership")
	}
}

// --- ban administration fakes + tests (wave 6b) -------------------------------

// fakeBanAdmin implements BanAdminStore in memory.
type fakeBanAdmin struct {
	mu   sync.Mutex
	bans []store.BanRecord
}

func (f *fakeBanAdmin) ListBans(context.Context) ([]store.BanRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.BanRecord, len(f.bans))
	copy(out, f.bans)
	return out, nil
}

func (f *fakeBanAdmin) DeleteBan(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, b := range f.bans {
		if b.ID == id {
			f.bans = append(f.bans[:i], f.bans[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("ban not found")
}

// TestBanListRemove verifies the ban list + lift flow and its gate.
func TestBanListRemove(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	expires := time.Now().Add(time.Hour)
	env.banAdmin.bans = []store.BanRecord{
		{ID: 1, Type: 1, Value: "bad-uid", Reason: "spam", BannedBy: "admin-uid", CreatedAt: time.Now()},
		{ID: 2, Type: 1, Value: "worse-uid", Reason: "griefing", CreatedAt: time.Now(), ExpiresAt: &expires},
	}

	// Non-admin without ban permission: denied.
	userConn, _ := dialAuthed(t, env.addr, "user-uid")
	defer userConn.Close()
	send(t, userConn, netproto.MsgBanList, netproto.BanList{})
	if e := readError(t, userConn); e.Code != errCodePermissionDenied {
		t.Fatalf("error = %+v, want permission denied", e)
	}

	// Admin lists.
	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer adminConn.Close()
	send(t, adminConn, netproto.MsgBanList, netproto.BanList{})
	f := readOfType(t, adminConn, netproto.MsgBanListResponse)
	var list netproto.BanListResponse
	if err := netproto.Decode(f, &list); err != nil {
		t.Fatalf("decode ban list: %v", err)
	}
	if len(list.Bans) != 2 || list.Bans[0].Value != "bad-uid" || list.Bans[1].ExpiresAt == 0 {
		t.Fatalf("bans = %+v", list.Bans)
	}

	// Admin lifts one; the audit row lands.
	send(t, adminConn, netproto.MsgBanRemove, netproto.BanRemove{BanID: 1})
	waitFor(t, "ban removed", func() bool {
		bans, _ := env.banAdmin.ListBans(context.Background())
		return len(bans) == 1 && bans[0].ID == 2
	})
	if got := env.groups.auditActions(); len(got) == 0 || got[len(got)-1] != "ban_remove" {
		t.Fatalf("audit actions = %v", got)
	}
}

// TestGroupIconSetGet verifies the group icon round trip.
func TestGroupIconSetGet(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	conn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer conn.Close()

	// Unknown group: empty payload (not an error).
	send(t, conn, netproto.MsgGroupIconGet, netproto.GroupIconGet{GroupID: 42})
	f := readOfType(t, conn, netproto.MsgGroupIconData)
	var empty netproto.GroupIconData
	if err := netproto.Decode(f, &empty); err != nil {
		t.Fatalf("decode icon data: %v", err)
	}
	if empty.DataBase64 != "" {
		t.Fatalf("icon for unknown group = %d bytes, want none", len(empty.DataBase64))
	}

	// Set, then get.
	gid, _ := env.groups.CreateGroup(context.Background(), "server", "Iconed", 0)
	send(t, conn, netproto.MsgGroupIconSet, netproto.GroupIconSet{GroupID: gid, DataBase64: b64(tinyPNG)})
	// The set has no response frame; poll the get.
	deadline := time.Now().Add(3 * time.Second)
	for {
		send(t, conn, netproto.MsgGroupIconGet, netproto.GroupIconGet{GroupID: gid})
		f = readOfType(t, conn, netproto.MsgGroupIconData)
		var data netproto.GroupIconData
		if err := netproto.Decode(f, &data); err != nil {
			t.Fatalf("decode icon data: %v", err)
		}
		if data.DataBase64 == b64(tinyPNG) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("icon not readable after set")
		}
	}
}

// TestGroupMembers verifies member listings for both group types.
func TestGroupMembers(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	conn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer conn.Close()

	gid, _ := env.groups.CreateGroup(context.Background(), "server", "Membered", 0)
	if err := env.groups.AssignServerGroup(context.Background(), gid, 2, time.Hour); err != nil {
		t.Fatalf("assign: %v", err)
	}

	send(t, conn, netproto.MsgGroupMembers, netproto.GroupMembers{Type: "server", GroupID: gid})
	f := readOfType(t, conn, netproto.MsgGroupMembersResponse)
	var resp netproto.GroupMembersResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode members: %v", err)
	}
	if len(resp.Members) != 1 || resp.Members[0].UniqueID != "user-uid" || resp.Members[0].ExpiresAt == 0 {
		t.Fatalf("members = %+v", resp.Members)
	}

	// Channel groups require channel_id.
	cgid, _ := env.groups.CreateGroup(context.Background(), "channel", "ChanGroup", 0)
	send(t, conn, netproto.MsgGroupMembers, netproto.GroupMembers{Type: "channel", GroupID: cgid})
	if e := readError(t, conn); e.Code != errCodeMalformed {
		t.Fatalf("error = %+v, want malformed", e)
	}
	if err := env.groups.AssignChannelGroup(context.Background(), cgid, 2, 5); err != nil {
		t.Fatalf("channel assign: %v", err)
	}
	send(t, conn, netproto.MsgGroupMembers, netproto.GroupMembers{Type: "channel", GroupID: cgid, ChannelID: 5})
	f = readOfType(t, conn, netproto.MsgGroupMembersResponse)
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode members: %v", err)
	}
	if len(resp.Members) != 1 || resp.Members[0].UniqueID != "user-uid" {
		t.Fatalf("channel members = %+v", resp.Members)
	}
}

// TestPermList verifies the editor read path: set entries are listed back,
// and the gate matches the write path.
func TestPermList(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	// Gate: non-admin without b_permission_manage is denied.
	userConn, _ := dialAuthed(t, env.addr, "user-uid")
	defer userConn.Close()
	send(t, userConn, netproto.MsgPermList, netproto.PermList{Tier: "server_group", GroupID: 3})
	if e := readError(t, userConn); e.Code != errCodePermissionDenied {
		t.Fatalf("error = %+v, want permission denied", e)
	}

	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer adminConn.Close()
	send(t, adminConn, netproto.MsgPermSet, netproto.PermSet{
		Tier: "server_group", GroupID: 3, Key: "i_client_talk_power", Value: 10, Grant: 50, Skip: true,
	})
	waitFor(t, "perm written", func() bool {
		env.groups.mu.Lock()
		defer env.groups.mu.Unlock()
		_, ok := env.groups.perms[permKey(store.PermTierServerGroup, store.PermTarget{GroupID: 3}, "i_client_talk_power")]
		return ok
	})

	send(t, adminConn, netproto.MsgPermList, netproto.PermList{Tier: "server_group", GroupID: 3})
	f := readOfType(t, adminConn, netproto.MsgPermListResponse)
	var resp netproto.PermListResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode perm list: %v", err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("entries = %+v, want 1", resp.Entries)
	}
	e := resp.Entries[0]
	if e.Key != "i_client_talk_power" || e.Value != 10 || e.Grant != 50 || !e.Skip || e.Negate {
		t.Fatalf("entry = %+v", e)
	}
}

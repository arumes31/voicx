// groups_test.go DB-backed tests for the wave-6a group/permission store.
// They follow the repository's skip pattern: without a reachable Postgres the
// tests skip. Set VOICX_TEST_DATABASE_URL to override the default dev URL.
package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// testDBStore opens the test database or skips. Migrations are applied so the
// wave-6a tables/columns exist.
func testDBStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(testDBURL(), testLogger(), 2, 1, time.Minute)
	if err != nil {
		t.Skipf("no database available (%v); skipping DB-backed test", err)
	}
	if err := s.Migrate(); err != nil {
		s.Close()
		t.Skipf("migrations failed (%v); skipping DB-backed test", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedTestUser inserts a throwaway user row and removes it (cascading) at
// test end.
func seedTestUser(t *testing.T, s *Store, suffix string) int64 {
	t.Helper()
	var id int64
	err := s.DB().QueryRowContext(context.Background(),
		`INSERT INTO users (unique_id, nickname, password_hash, created_at)
		 VALUES ($1, $2, 'x', NOW()) RETURNING id`,
		"w6atest_"+suffix, "w6a-"+suffix).Scan(&id)
	if err != nil {
		t.Fatalf("seeding user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.DB().ExecContext(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// TestGroupsDB covers group CRUD, membership (incl. timed expiry + reaping),
// and member counts against a real database.
func TestGroupsDB(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	userID := seedTestUser(t, s, suffix)

	// CRUD.
	gid, err := s.CreateGroup(ctx, "server", "w6a_test_"+suffix, 42)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteGroup(ctx, "server", gid, true) })

	g, err := s.FindGroupByName(ctx, "server", "w6a_test_"+suffix)
	if err != nil || g == nil || g.ID != gid || g.SortID != 42 {
		t.Fatalf("FindGroupByName = %+v, err=%v", g, err)
	}
	if err := s.RenameGroup(ctx, "server", gid, "w6a_test_renamed_"+suffix); err != nil {
		t.Fatalf("RenameGroup: %v", err)
	}
	g, _ = s.GetGroup(ctx, "server", gid)
	if g == nil || g.Name != "w6a_test_renamed_"+suffix {
		t.Fatalf("after rename = %+v", g)
	}

	// Membership + count.
	if err := s.AssignServerGroup(ctx, gid, userID, 0); err != nil {
		t.Fatalf("AssignServerGroup: %v", err)
	}
	g, _ = s.GetGroup(ctx, "server", gid)
	if g.MemberCount != 1 {
		t.Fatalf("member count = %d, want 1", g.MemberCount)
	}

	// Delete refuses with members unless forced.
	if err := s.DeleteGroup(ctx, "server", gid, false); err == nil {
		t.Fatal("delete without force succeeded on a non-empty group")
	}

	// Timed membership: expire immediately, then reap.
	if err := s.AssignServerGroup(ctx, gid, userID, time.Millisecond); err != nil {
		t.Fatalf("timed assign: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	pairs, err := s.ExpiredGroupMembers(ctx, time.Now())
	if err != nil {
		t.Fatalf("ExpiredGroupMembers: %v", err)
	}
	if len(pairs) != 1 || pairs[0][0] != userID || pairs[0][1] != gid {
		t.Fatalf("reaped pairs = %v", pairs)
	}
	if ids, _ := s.UserGroupIDs(ctx, userID); len(ids) != 0 {
		t.Fatalf("groups after reap = %v", ids)
	}

	// Unassign.
	if err := s.AssignServerGroup(ctx, gid, userID, 0); err != nil {
		t.Fatalf("re-assign: %v", err)
	}
	if err := s.UnassignServerGroup(ctx, gid, userID); err != nil {
		t.Fatalf("UnassignServerGroup: %v", err)
	}
	if err := s.DeleteGroup(ctx, "server", gid, false); err != nil {
		t.Fatalf("delete after unassign: %v", err)
	}

	// UserUniqueID round-trip.
	uid, err := s.UserUniqueID(ctx, userID)
	if err != nil || uid != "w6atest_"+suffix {
		t.Fatalf("UserUniqueID = %q, err=%v", uid, err)
	}
}

// TestChannelGroupsDB covers channel-group assign/unassign addressing.
func TestChannelGroupsDB(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	userID := seedTestUser(t, s, suffix)

	gid, err := s.CreateGroup(ctx, "channel", "w6a_chan_"+suffix, 0)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteGroup(ctx, "channel", gid, true) })

	var channelID int64
	err = s.DB().QueryRowContext(ctx,
		`INSERT INTO channels (name, channel_type, created_at) VALUES ($1, 2, NOW()) RETURNING id`,
		"w6a_chan_"+suffix).Scan(&channelID)
	if err != nil {
		t.Fatalf("seeding channel: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.DB().ExecContext(ctx, `DELETE FROM channels WHERE id = $1`, channelID)
	})

	if err := s.AssignChannelGroup(ctx, gid, userID, channelID); err != nil {
		t.Fatalf("AssignChannelGroup: %v", err)
	}
	g, _ := s.GetGroup(ctx, "channel", gid)
	if g == nil || g.MemberCount != 1 {
		t.Fatalf("channel group = %+v, want 1 member", g)
	}
	if err := s.UnassignChannelGroup(ctx, userID, channelID); err != nil {
		t.Fatalf("UnassignChannelGroup: %v", err)
	}
}

// TestPermWritePathDB covers SetPermission/UnsetPermission on all five tiers,
// including the new channel tier (channel_permissions, migration 009).
func TestPermWritePathDB(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	userID := seedTestUser(t, s, suffix)

	sgID, err := s.CreateGroup(ctx, "server", "w6a_perm_"+suffix, 0)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteGroup(ctx, "server", sgID, true) })
	cgID, err := s.CreateGroup(ctx, "channel", "w6a_perm_"+suffix, 0)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteGroup(ctx, "channel", cgID, true) })
	var channelID int64
	if err := s.DB().QueryRowContext(ctx,
		`INSERT INTO channels (name, channel_type, created_at) VALUES ($1, 2, NOW()) RETURNING id`,
		"w6a_perm_"+suffix).Scan(&channelID); err != nil {
		t.Fatalf("seeding channel: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.DB().ExecContext(ctx, `DELETE FROM channels WHERE id = $1`, channelID)
	})

	key := "w6a_test_perm_" + suffix
	cases := []struct {
		tier   PermTier
		target PermTarget
	}{
		{PermTierServerGroup, PermTarget{GroupID: sgID}},
		{PermTierChannelGroup, PermTarget{GroupID: cgID}},
		{PermTierClient, PermTarget{UserID: userID}},
		{PermTierChannelClient, PermTarget{UserID: userID, ChannelID: channelID}},
		{PermTierChannel, PermTarget{ChannelID: channelID}},
	}
	for _, tc := range cases {
		if err := s.SetPermission(ctx, tc.tier, tc.target, key, 7, 70, true, false); err != nil {
			t.Fatalf("SetPermission(%s): %v", tc.tier, err)
		}
		// Replace with different flags (same key, same target).
		if err := s.SetPermission(ctx, tc.tier, tc.target, key, 9, 0, false, true); err != nil {
			t.Fatalf("SetPermission replace (%s): %v", tc.tier, err)
		}
		if err := s.UnsetPermission(ctx, tc.tier, tc.target, key); err != nil {
			t.Fatalf("UnsetPermission(%s): %v", tc.tier, err)
		}
	}

	// The channel tier row must have existed between set and unset (verify via
	// a final set + direct query).
	if err := s.SetPermission(ctx, PermTierChannel, PermTarget{ChannelID: channelID}, key, 5, 0, false, false); err != nil {
		t.Fatalf("SetPermission channel: %v", err)
	}
	var value int
	err = s.DB().QueryRowContext(ctx,
		`SELECT p.value FROM channel_permissions cp
		 JOIN permissions p ON p.id = cp.permission_id
		 WHERE cp.channel_id = $1 AND p.permission_key = $2`, channelID, key).Scan(&value)
	if err != nil || value != 5 {
		t.Fatalf("channel tier row value = %d, err=%v", value, err)
	}
	if err := s.UnsetPermission(ctx, PermTierChannel, PermTarget{ChannelID: channelID}, key); err != nil {
		t.Fatalf("UnsetPermission channel: %v", err)
	}
}

// TestAuditDB covers audit writes and paged reads.
func TestAuditDB(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())

	actor := "w6a_audit_" + suffix
	for i := 0; i < 3; i++ {
		s.Audit(ctx, actor, "group_create", fmt.Sprintf("server:g%d", i), "")
	}
	t.Cleanup(func() {
		_, _ = s.DB().ExecContext(ctx, `DELETE FROM audit_log WHERE actor_unique_id = $1`, actor)
	})

	entries, err := s.AuditList(ctx, 0, 2)
	if err != nil {
		t.Fatalf("AuditList: %v", err)
	}
	if len(entries) != 2 || entries[0].Actor != actor || entries[0].Target != "server:g2" {
		t.Fatalf("page 1 = %+v", entries)
	}
	// Page 2 via before_id returns the oldest of the three.
	entries, err = s.AuditList(ctx, entries[1].ID, 10)
	if err != nil {
		t.Fatalf("AuditList page 2: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Actor == actor && e.Target == "server:g0" {
			found = true
		}
	}
	if !found {
		t.Fatalf("page 2 missing oldest entry: %+v", entries)
	}
}

// TestBansDB covers ListBans/DeleteBan (wave 6b ban administration).
func TestBansDB(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())

	value := "w6a_ban_" + suffix
	if _, err := s.DB().ExecContext(ctx,
		`INSERT INTO bans (ban_type, value, reason, expires_at) VALUES (1, $1, 'test ban', NOW() + interval '1 hour')`, value); err != nil {
		t.Fatalf("seeding ban: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.DB().ExecContext(ctx, `DELETE FROM bans WHERE value = $1`, value)
	})

	bans, err := s.ListBans(ctx)
	if err != nil {
		t.Fatalf("ListBans: %v", err)
	}
	var found *BanRecord
	for i := range bans {
		if bans[i].Value == value {
			found = &bans[i]
		}
	}
	if found == nil || found.Reason != "test ban" || found.ExpiresAt == nil {
		t.Fatalf("seeded ban = %+v", found)
	}

	if err := s.DeleteBan(ctx, found.ID); err != nil {
		t.Fatalf("DeleteBan: %v", err)
	}
	bans, _ = s.ListBans(ctx)
	for _, b := range bans {
		if b.Value == value {
			t.Fatal("ban still listed after delete")
		}
	}
}

// TestGroupMembersDB covers the member listings against a real database.
func TestGroupMembersDB(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	userID := seedTestUser(t, s, suffix)

	gid, err := s.CreateGroup(ctx, "server", "w6a_mem_"+suffix, 0)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteGroup(ctx, "server", gid, true) })

	if err := s.AssignServerGroup(ctx, gid, userID, time.Hour); err != nil {
		t.Fatalf("AssignServerGroup: %v", err)
	}
	members, err := s.ListServerGroupMembers(ctx, gid)
	if err != nil {
		t.Fatalf("ListServerGroupMembers: %v", err)
	}
	if len(members) != 1 || members[0].UniqueID != "w6atest_"+suffix || members[0].ExpiresAt == nil {
		t.Fatalf("members = %+v", members)
	}
}

// TestGroupCosmeticsDB covers the colour/hoist/sort setter (178/179): a nil
// field must leave the stored column alone, so a dialog can send only what the
// operator touched.
func TestGroupCosmeticsDB(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())

	gid, err := s.CreateGroup(ctx, "server", "cosmetics_"+suffix, 1)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteGroup(ctx, "server", gid, true) })

	color, hoist, sortID := "#a1b2c3", true, 7
	if err := s.SetGroupCosmetics(ctx, gid, &color, &hoist, &sortID); err != nil {
		t.Fatalf("SetGroupCosmetics: %v", err)
	}
	g, err := s.GetGroup(ctx, "server", gid)
	if err != nil || g == nil {
		t.Fatalf("GetGroup: %v %+v", err, g)
	}
	if g.Color != color || !g.Hoist || g.SortID != 7 {
		t.Fatalf("group after full edit = %+v", g)
	}

	// Partial edit: only the hoist changes.
	off := false
	if err := s.SetGroupCosmetics(ctx, gid, nil, &off, nil); err != nil {
		t.Fatalf("SetGroupCosmetics partial: %v", err)
	}
	if g, err = s.GetGroup(ctx, "server", gid); err != nil || g == nil {
		t.Fatalf("GetGroup: %v %+v", err, g)
	}
	if g.Color != color || g.Hoist || g.SortID != 7 {
		t.Fatalf("group after partial edit = %+v", g)
	}

	if err := s.SetGroupCosmetics(ctx, gid, nil, nil, nil); err == nil {
		t.Fatalf("SetGroupCosmetics with no fields returned nil error")
	}
	if err := s.SetGroupCosmetics(ctx, 0, &color, nil, nil); err == nil {
		t.Fatalf("SetGroupCosmetics on a missing group returned nil error")
	}
}

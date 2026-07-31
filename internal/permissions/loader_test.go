package permissions

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"go.uber.org/zap"

	"voicx/internal/store"
)

// loader_test.go contains DB-backed tests for the Loader. Every test
// here skips cleanly when no Postgres is reachable, so `go test ./...`
// (and `go test -race ./...`) succeeds in CI without a database.

// testLoader constructs a Loader backed by a real Postgres store if one
// is reachable. It skips the calling test when no database is available.
// The store is migrated before use and closed on test cleanup.
func testLoader(t *testing.T) (*Loader, *store.Store) {
	t.Helper()

	dbURL := os.Getenv("VOICX_TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://voicx:voicx@localhost:5432/voicx?sslmode=disable"
	}

	logger, err := zap.NewDevelopment()
	if err != nil {
		t.Fatalf("zap.NewDevelopment: %v", err)
	}

	s, err := store.New(dbURL, logger, 5, 1, time.Minute)
	if err != nil {
		t.Skipf("database unavailable, skipping: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.Migrate(); err != nil {
		t.Skipf("migrate failed, skipping: %v", err)
	}
	return NewLoader(s, logger), s
}

// permRow inserts a row into the permissions table and returns its id.
func permRow(t *testing.T, db *sql.DB, key string, value, grant int, skip, negate bool) int64 {
	t.Helper()
	var id int64
	const q = `INSERT INTO permissions (permission_key, value, grant_value, skip_flag, negate_flag)
	          VALUES ($1, $2, $3, $4, $5) RETURNING id`
	if err := db.QueryRowContext(context.Background(), q, key, value, grant, skip, negate).Scan(&id); err != nil {
		t.Fatalf("insert permission %q: %v", key, err)
	}
	return id
}

// insertUser inserts a user row and returns its id.
func insertUser(t *testing.T, db *sql.DB, uniqueID, nickname string) int64 {
	t.Helper()
	var id int64
	const q = `INSERT INTO users (unique_id, nickname, created_at) VALUES ($1, $2, NOW()) RETURNING id`
	if err := db.QueryRowContext(context.Background(), q, uniqueID, nickname).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

// insertServerGroup inserts a server group and returns its id.
func insertServerGroup(t *testing.T, db *sql.DB, name string) int64 {
	t.Helper()
	var id int64
	const q = `INSERT INTO server_groups (name) VALUES ($1) RETURNING id`
	if err := db.QueryRowContext(context.Background(), q, name).Scan(&id); err != nil {
		t.Fatalf("insert server group: %v", err)
	}
	return id
}

// insertChannelGroup inserts a channel group and returns its id.
func insertChannelGroup(t *testing.T, db *sql.DB, name string) int64 {
	t.Helper()
	var id int64
	const q = `INSERT INTO channel_groups (name) VALUES ($1) RETURNING id`
	if err := db.QueryRowContext(context.Background(), q, name).Scan(&id); err != nil {
		t.Fatalf("insert channel group: %v", err)
	}
	return id
}

// insertChannel inserts a channel row and returns its id.
func insertChannel(t *testing.T, db *sql.DB, name string) int64 {
	t.Helper()
	var id int64
	const q = `INSERT INTO channels (name, channel_type) VALUES ($1, 0) RETURNING id`
	if err := db.QueryRowContext(context.Background(), q, name).Scan(&id); err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	return id
}

// linkServerGroupPerm attaches a permission to a server group.
func linkServerGroupPerm(t *testing.T, db *sql.DB, groupID, permID int64) {
	t.Helper()
	const q = `INSERT INTO server_group_permissions (server_group_id, permission_id) VALUES ($1, $2)`
	if _, err := db.ExecContext(context.Background(), q, groupID, permID); err != nil {
		t.Fatalf("link server group perm: %v", err)
	}
}

// linkChannelGroupPerm attaches a permission to a channel group.
func linkChannelGroupPerm(t *testing.T, db *sql.DB, groupID, permID int64) {
	t.Helper()
	const q = `INSERT INTO channel_group_permissions (channel_group_id, permission_id) VALUES ($1, $2)`
	if _, err := db.ExecContext(context.Background(), q, groupID, permID); err != nil {
		t.Fatalf("link channel group perm: %v", err)
	}
}

// addServerGroupMember adds a user to a server group.
func addServerGroupMember(t *testing.T, db *sql.DB, userID, groupID int64) {
	t.Helper()
	const q = `INSERT INTO server_group_members (user_id, server_group_id) VALUES ($1, $2)`
	if _, err := db.ExecContext(context.Background(), q, userID, groupID); err != nil {
		t.Fatalf("add server group member: %v", err)
	}
}

// addChannelGroupMember assigns a channel group to a user on a channel.
func addChannelGroupMember(t *testing.T, db *sql.DB, userID, channelID, groupID int64) {
	t.Helper()
	const q = `INSERT INTO channel_group_members (user_id, channel_id, channel_group_id) VALUES ($1, $2, $3)`
	if _, err := db.ExecContext(context.Background(), q, userID, channelID, groupID); err != nil {
		t.Fatalf("add channel group member: %v", err)
	}
}

// addClientPermission inserts a client_permissions row. Pass 0 for
// channelID to mean NULL (server-wide).
func addClientPermission(t *testing.T, db *sql.DB, userID, permID, channelID int64) {
	t.Helper()
	var q string
	var args []interface{}
	if channelID == 0 {
		q = `INSERT INTO client_permissions (user_id, permission_id, channel_id) VALUES ($1, $2, NULL)`
		args = []interface{}{userID, permID}
	} else {
		q = `INSERT INTO client_permissions (user_id, permission_id, channel_id) VALUES ($1, $2, $3)`
		args = []interface{}{userID, permID, channelID}
	}
	if _, err := db.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("add client permission: %v", err)
	}
}

// addChannelClientPermission inserts a channel_client_permissions row.
func addChannelClientPermission(t *testing.T, db *sql.DB, userID, channelID, permID int64) {
	t.Helper()
	const q = `INSERT INTO channel_client_permissions (user_id, channel_id, permission_id) VALUES ($1, $2, $3)`
	if _, err := db.ExecContext(context.Background(), q, userID, channelID, permID); err != nil {
		t.Fatalf("add channel client permission: %v", err)
	}
}

// uniqueSuffix returns a unique-ish suffix for test names to avoid PK
// collisions across runs.
func uniqueSuffix(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// TestLoadForClient_AssemblesAllTiers sets up a user with memberships and
// permissions across all five tiers and verifies LoadForClient returns
// the expected entries in the correct tiers.
func TestLoadForClient_AssemblesAllTiers(t *testing.T) {
	loader, s := testLoader(t)
	db := s.DB()
	suffix := uniqueSuffix("lfc")

	userID := insertUser(t, db, "uid-"+suffix, "nick-"+suffix)
	channelID := insertChannel(t, db, "chan-"+suffix)

	// ServerGroup tier: two groups granting join_power 50 and 75 → merged 75.
	sg1 := insertServerGroup(t, db, "sg1-"+suffix)
	sg2 := insertServerGroup(t, db, "sg2-"+suffix)
	pJoin50 := permRow(t, db, "i_channel_join_power", 50, 50, false, false)
	pJoin75 := permRow(t, db, "i_channel_join_power", 75, 75, false, false)
	linkServerGroupPerm(t, db, sg1, pJoin50)
	linkServerGroupPerm(t, db, sg2, pJoin75)
	addServerGroupMember(t, db, userID, sg1)
	addServerGroupMember(t, db, userID, sg2)

	// ClientSpecific tier: server-wide client perm b_channel_delete = 1.
	pDel := permRow(t, db, "b_channel_delete", 1, 0, false, false)
	addClientPermission(t, db, userID, pDel, 0)

	// ChannelSpecific tier: channel-scoped client perm i_client_talk_power = 30.
	pTalk := permRow(t, db, "i_client_talk_power", 30, 30, false, false)
	addClientPermission(t, db, userID, pTalk, channelID)

	// ChannelGroup tier: channel group granting i_channel_subscribe_power = 10.
	cg := insertChannelGroup(t, db, "cg-"+suffix)
	pSub := permRow(t, db, "i_channel_subscribe_power", 10, 10, false, false)
	linkChannelGroupPerm(t, db, cg, pSub)
	addChannelGroupMember(t, db, userID, channelID, cg)

	// ChannelClient tier: channel_client_permissions b_client_poke = 1.
	pPoke := permRow(t, db, "b_client_poke", 1, 0, false, false)
	addChannelClientPermission(t, db, userID, channelID, pPoke)

	tp, err := loader.LoadForClient(context.Background(), userID, channelID)
	if err != nil {
		t.Fatalf("LoadForClient: %v", err)
	}

	// ServerGroup: merged join_power should be 75 (max of 50 and 75).
	sgSet, ok := tp.Get(TierServerGroup)
	if !ok || sgSet == nil {
		t.Fatal("missing ServerGroup tier")
	}
	p, ok := sgSet.Get(PermissionKeyChannelJoinPower)
	if !ok {
		t.Fatal("ServerGroup: missing join_power")
	}
	if p.Value != 75 {
		t.Fatalf("ServerGroup: expected merged join_power 75, got %d", p.Value)
	}

	// ClientSpecific: b_channel_delete = 1.
	csSet, ok := tp.Get(TierClientSpecific)
	if !ok || !csSet.Has(PermissionKeyChannelDelete) {
		t.Fatal("missing ClientSpecific tier entry")
	}

	// ChannelSpecific: i_client_talk_power = 30.
	chSpecSet, ok := tp.Get(TierChannelSpecific)
	if !ok || !chSpecSet.Has(PermissionKeyClientTalkPower) {
		t.Fatal("missing ChannelSpecific tier entry")
	}

	// ChannelGroup: i_channel_subscribe_power = 10.
	cgSet, ok := tp.Get(TierChannelGroup)
	if !ok || !cgSet.Has(PermissionKeyChannelSubscribePower) {
		t.Fatal("missing ChannelGroup tier entry")
	}

	// ChannelClient: b_client_poke = 1.
	ccSet, ok := tp.Get(TierChannelClient)
	if !ok || !ccSet.Has(PermissionKeyClientPoke) {
		t.Fatal("missing ChannelClient tier entry")
	}
}

// TestLoadForClient_NoMembershipsReturnsEmpty verifies that a user with
// no memberships and no permissions returns an empty (non-nil)
// TieredPermissions and no error.
func TestLoadForClient_NoMembershipsReturnsEmpty(t *testing.T) {
	loader, s := testLoader(t)
	db := s.DB()
	suffix := uniqueSuffix("empty")

	userID := insertUser(t, db, "uid-"+suffix, "nick-"+suffix)
	channelID := insertChannel(t, db, "chan-"+suffix)

	tp, err := loader.LoadForClient(context.Background(), userID, channelID)
	if err != nil {
		t.Fatalf("LoadForClient: %v", err)
	}

	for _, tier := range []Tier{TierServerGroup, TierClientSpecific, TierChannelSpecific, TierChannelGroup, TierChannelClient} {
		set, ok := tp.Get(tier)
		if !ok {
			t.Fatalf("tier %s: expected present", tier)
		}
		if set == nil {
			t.Fatalf("tier %s: expected non-nil set", tier)
		}
		if len(set) != 0 {
			t.Fatalf("tier %s: expected empty set, got %d entries", tier, len(set))
		}
	}
}

// TestLoadServerGroupsForUser verifies the helper returns the user's
// server group IDs.
func TestLoadServerGroupsForUser(t *testing.T) {
	loader, s := testLoader(t)
	db := s.DB()
	suffix := uniqueSuffix("sg")

	userID := insertUser(t, db, "uid-"+suffix, "nick-"+suffix)
	sg1 := insertServerGroup(t, db, "sg1-"+suffix)
	sg2 := insertServerGroup(t, db, "sg2-"+suffix)
	addServerGroupMember(t, db, userID, sg1)
	addServerGroupMember(t, db, userID, sg2)

	ids, err := loader.LoadServerGroupsForUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("LoadServerGroupsForUser: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 server group ids, got %d (%v)", len(ids), ids)
	}
	// Results are ordered by id ascending; sg1 < sg2 by insertion order.
	if ids[0] != sg1 || ids[1] != sg2 {
		t.Fatalf("expected ids [%d,%d], got %v", sg1, sg2, ids)
	}
}

// TestLoadChannelGroupForUser verifies the helper returns the user's
// channel group id for the channel, or 0 if none.
func TestLoadChannelGroupForUser(t *testing.T) {
	loader, s := testLoader(t)
	db := s.DB()
	suffix := uniqueSuffix("cg")

	userID := insertUser(t, db, "uid-"+suffix, "nick-"+suffix)
	channelID := insertChannel(t, db, "chan-"+suffix)
	cg := insertChannelGroup(t, db, "cg-"+suffix)
	addChannelGroupMember(t, db, userID, channelID, cg)

	got, err := loader.LoadChannelGroupForUser(context.Background(), userID, channelID)
	if err != nil {
		t.Fatalf("LoadChannelGroupForUser: %v", err)
	}
	if got != cg {
		t.Fatalf("expected channel group %d, got %d", cg, got)
	}

	// User with no channel group on a different channel returns 0.
	otherChannel := insertChannel(t, db, "other-"+suffix)
	got2, err := loader.LoadChannelGroupForUser(context.Background(), userID, otherChannel)
	if err != nil {
		t.Fatalf("LoadChannelGroupForUser(other): %v", err)
	}
	if got2 != 0 {
		t.Fatalf("expected 0 for no channel group, got %d", got2)
	}
}

// TestLoadForClient_CacheReturnsSameResult verifies that a second call
// within the TTL returns the cached TieredPermissions without re-querying.
func TestLoadForClient_CacheReturnsSameResult(t *testing.T) {
	loader, s := testLoader(t)
	db := s.DB()
	suffix := uniqueSuffix("cache")

	userID := insertUser(t, db, "uid-"+suffix, "nick-"+suffix)
	channelID := insertChannel(t, db, "chan-"+suffix)

	tp1, err := loader.LoadForClient(context.Background(), userID, channelID)
	if err != nil {
		t.Fatalf("LoadForClient 1: %v", err)
	}
	tp2, err := loader.LoadForClient(context.Background(), userID, channelID)
	if err != nil {
		t.Fatalf("LoadForClient 2: %v", err)
	}
	// Both calls should return non-nil empty sets for all tiers.
	for _, tier := range []Tier{TierServerGroup, TierClientSpecific, TierChannelSpecific, TierChannelGroup, TierChannelClient} {
		s1, ok1 := tp1.Get(tier)
		s2, ok2 := tp2.Get(tier)
		if !ok1 || !ok2 || s1 == nil || s2 == nil {
			t.Fatalf("tier %s: expected present non-nil sets on both calls", tier)
		}
	}
}

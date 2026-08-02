// inherit_test.go covers the channel-tier inheritance chain (157). Like the
// rest of the loader tests these need a reachable Postgres and skip without.
package permissions

import (
	"context"
	"database/sql"
	"testing"
)

// insertChildChannel inserts a channel under parentID with the given
// inheritance flag and returns its id.
func insertChildChannel(t *testing.T, db *sql.DB, name string, parentID int64, inherit bool) int64 {
	t.Helper()
	var id int64
	const q = `INSERT INTO channels (name, channel_type, parent_id, inherit_permissions)
	          VALUES ($1, 0, $2, $3) RETURNING id`
	var parent any
	if parentID != 0 {
		parent = parentID
	}
	if err := db.QueryRowContext(context.Background(), q, name, parent, inherit).Scan(&id); err != nil {
		t.Fatalf("insert child channel: %v", err)
	}
	return id
}

// addChannelPermission attaches a permission to a channel object.
func addChannelPermission(t *testing.T, db *sql.DB, channelID, permID int64) {
	t.Helper()
	const q = `INSERT INTO channel_permissions (channel_id, permission_id) VALUES ($1, $2)`
	if _, err := db.ExecContext(context.Background(), q, channelID, permID); err != nil {
		t.Fatalf("add channel permission: %v", err)
	}
}

// channelTier returns the resolved channel-tier set for a channel.
func channelTier(t *testing.T, loader *Loader, userID, channelID int64) PermissionSet {
	t.Helper()
	tp, err := loader.LoadForClient(context.Background(), userID, channelID)
	if err != nil {
		t.Fatalf("LoadForClient: %v", err)
	}
	set, _ := tp.Get(TierChannel)
	return set
}

// TestChannelInheritDefaultsOff is the regression guard for the migration's
// DEFAULT FALSE: a child that has not opted in must not see its parent's
// channel permissions at all.
func TestChannelInheritDefaultsOff(t *testing.T) {
	loader, s := testLoader(t)
	db := s.DB()
	suffix := uniqueSuffix("inh-off")

	userID := insertUser(t, db, "uid-"+suffix, "nick-"+suffix)
	parent := insertChannel(t, db, "parent-"+suffix)
	child := insertChildChannel(t, db, "child-"+suffix, parent, false)

	pTalk := permRow(t, db, "i_client_talk_power", 90, 90, false, false)
	addChannelPermission(t, db, parent, pTalk)

	if set := channelTier(t, loader, userID, child); set.Has("i_client_talk_power") {
		t.Fatalf("child inherited the parent's channel permission with inherit_permissions FALSE")
	}
	// The parent itself still resolves its own entry — the chain is not what
	// broke, inheritance is simply off.
	if set := channelTier(t, loader, userID, parent); !set.Has("i_client_talk_power") {
		t.Fatalf("parent lost its own channel permission")
	}
}

// TestChannelInheritMergesAncestors verifies an opted-in child picks up an
// ancestor's entries.
func TestChannelInheritMergesAncestors(t *testing.T) {
	loader, s := testLoader(t)
	db := s.DB()
	suffix := uniqueSuffix("inh-on")

	userID := insertUser(t, db, "uid-"+suffix, "nick-"+suffix)
	grandparent := insertChannel(t, db, "gp-"+suffix)
	parent := insertChildChannel(t, db, "p-"+suffix, grandparent, true)
	child := insertChildChannel(t, db, "c-"+suffix, parent, true)

	pTalk := permRow(t, db, "i_client_talk_power", 90, 90, false, false)
	addChannelPermission(t, db, grandparent, pTalk)
	pDel := permRow(t, db, "b_channel_delete", 1, 0, false, false)
	addChannelPermission(t, db, parent, pDel)

	set := channelTier(t, loader, userID, child)
	if p, ok := set.Get("i_client_talk_power"); !ok || p.Value != 90 {
		t.Errorf("grandparent entry = %v, want value 90", p)
	}
	if p, ok := set.Get("b_channel_delete"); !ok || p.Value != 1 {
		t.Errorf("parent entry = %v, want value 1", p)
	}
}

// TestChannelInheritNearestWins pins the override semantics: the nearest
// channel is the more specific statement about a key and must win even when
// it grants LESS than the ancestor. A max-style merge would return 90 here
// and quietly hand out the parent's power inside a restricted sub-channel.
func TestChannelInheritNearestWins(t *testing.T) {
	loader, s := testLoader(t)
	db := s.DB()
	suffix := uniqueSuffix("inh-near")

	userID := insertUser(t, db, "uid-"+suffix, "nick-"+suffix)
	parent := insertChannel(t, db, "parent-"+suffix)
	child := insertChildChannel(t, db, "child-"+suffix, parent, true)

	addChannelPermission(t, db, parent, permRow(t, db, "i_client_talk_power", 90, 90, false, false))
	addChannelPermission(t, db, child, permRow(t, db, "i_client_talk_power", 10, 10, false, false))

	set := channelTier(t, loader, userID, child)
	if p, ok := set.Get("i_client_talk_power"); !ok || p.Value != 10 {
		t.Fatalf("i_client_talk_power = %v, want the child's 10", p)
	}
}

// TestChannelInheritStopsAtFirstOptOut verifies the walk halts at the first
// channel without the flag, so an opted-out middle channel shields the
// subtree from everything above it.
func TestChannelInheritStopsAtFirstOptOut(t *testing.T) {
	loader, s := testLoader(t)
	db := s.DB()
	suffix := uniqueSuffix("inh-stop")

	userID := insertUser(t, db, "uid-"+suffix, "nick-"+suffix)
	grandparent := insertChannel(t, db, "gp-"+suffix)
	parent := insertChildChannel(t, db, "p-"+suffix, grandparent, false)
	child := insertChildChannel(t, db, "c-"+suffix, parent, true)

	addChannelPermission(t, db, grandparent, permRow(t, db, "i_client_talk_power", 90, 90, false, false))
	addChannelPermission(t, db, parent, permRow(t, db, "b_channel_delete", 1, 0, false, false))

	set := channelTier(t, loader, userID, child)
	if !set.Has("b_channel_delete") {
		t.Errorf("child did not inherit its immediate parent's entry")
	}
	if set.Has("i_client_talk_power") {
		t.Errorf("child inherited past the parent, which has inherit_permissions FALSE")
	}
}

// TestChannelInheritUnknownChannel verifies an id the channels table does not
// have still resolves (to nothing) instead of erroring.
func TestChannelInheritUnknownChannel(t *testing.T) {
	loader, s := testLoader(t)
	db := s.DB()
	suffix := uniqueSuffix("inh-unknown")

	userID := insertUser(t, db, "uid-"+suffix, "nick-"+suffix)
	if set := channelTier(t, loader, userID, -1); len(set) != 0 {
		t.Fatalf("unknown channel resolved to %v, want an empty set", set)
	}
}

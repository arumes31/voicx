package permissions

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// loader.go implements the DB-backed loading of permissions into the
// TieredPermissions structure. It is the bridge between the relational
// schema (migrations/001_init.sql) and the in-memory tiered model used
// by the resolver.
//
// The Loader is intentionally NOT wired into main.go in this step; that
// wiring happens with the TCP middleware in Steps 241-260. Here we only
// provide the package code and tests.
//
// ---------------------------------------------------------------------
// LOADING STRATEGY
// ---------------------------------------------------------------------
//
// LoadForClient(userID, channelID) assembles the full TieredPermissions
// for a user in the context of a specific channel by querying five
// independent data sources, one per tier:
//
//  1. ServerGroup tier (TierServerGroup):
//     - Look up the user's server group memberships
//       (server_group_members).
//     - For each server group, load its permissions via
//       server_group_permissions JOIN permissions.
//     - Build one PermissionSet per group, then MergeSet them into a
//       single set for the tier.
//
//  2. ClientSpecific tier (TierClientSpecific):
//     - Load client_permissions WHERE user_id = userID AND channel_id
//       IS NULL (server-wide client-specific perms).
//     - One PermissionSet (no merge needed; at most one entry per key
//       per (user, permission, NULL-channel) by PK).
//
//  3. ChannelSpecific tier (TierChannelSpecific):
//     - Load client_permissions WHERE user_id = userID AND channel_id
//       = channelID (channel-scoped client perms).
//     - One PermissionSet.
//
//  4. ChannelGroup tier (TierChannelGroup):
//     - Look up the user's channel group membership(s) for channelID
//       (channel_group_members). TS3 allows at most one channel group
//       per user per channel, but the loader tolerates several rows
//       and merges them.
//     - For each channel group, load its permissions via
//       channel_group_permissions JOIN permissions.
//     - MergeSet the per-group sets into one set for the tier.
//
//  5. ChannelClient tier (TierChannelClient):
//     - Load channel_client_permissions WHERE user_id = userID AND
//       channel_id = channelID.
//     - One PermissionSet.
//
// Each permission row maps to a Permission{Key, Type, Value, Grant,
// Skip, Negate}. The Type is inferred from the key prefix:
//   - "b_" → PermissionTypeBoolean
//   - "i_" → PermissionTypeInteger
//
// If no permissions are found for the user, an empty (but non-nil)
// TieredPermissions is returned — NOT an error. ErrPermissionNotSet is
// a resolver-level concept and is not returned by the loader.
//
// ---------------------------------------------------------------------
// CACHING
// ---------------------------------------------------------------------
//
// A small in-memory cache (sync.Map keyed by cacheKey{userID,channelID})
// avoids re-querying the database on every permission check. Entries
// expire after cacheTTL (5s). The cache is best-effort: a miss or an
// expired entry simply triggers a fresh query. There is no explicit
// invalidation here; invalidation hooks are added when the loader is
// wired into the mutation paths in later steps.

type Loader struct {
	store  StoreBackend
	logger *zap.Logger

	cache sync.Map // map[cacheKey]cacheEntry
}

// StoreBackend is the minimal subset of *store.Store that the loader
// needs. Defining it as an interface keeps the loader testable with a
// fake backend and decouples it from the concrete store package (which
// would create an import cycle if referenced directly here, since
// store imports nothing from permissions but permissions importing
// store is fine — we still prefer the interface for testability).
type StoreBackend interface {
	// DB returns the underlying *sql.DB. The loader runs its own
	// context-aware queries against it.
	DB() *sql.DB
}

// cacheKey identifies a cached TieredPermissions by user and channel.
type cacheKey struct {
	userID    int64
	channelID int64
}

type cacheEntry struct {
	tp      TieredPermissions
	expires time.Time
}

// cacheTTL is how long a cached TieredPermissions is considered fresh.
const cacheTTL = 5 * time.Second

// New constructs a Loader backed by the given store and logger. The
// logger may be nil. The returned Loader is safe for concurrent use.
func NewLoader(store StoreBackend, logger *zap.Logger) *Loader {
	return &Loader{store: store, logger: logger}
}

// LoadForClient assembles the full TieredPermissions for userID in the
// context of channelID by querying the database. See the file-level
// comment for the per-tier loading strategy.
//
// Returns an empty (non-nil) TieredPermissions if the user has no
// permissions; never returns ErrPermissionNotSet.
func (l *Loader) LoadForClient(ctx context.Context, userID int64, channelID int64) (TieredPermissions, error) {
	key := cacheKey{userID: userID, channelID: channelID}
	if v, ok := l.cache.Load(key); ok {
		ce := v.(cacheEntry)
		if time.Now().Before(ce.expires) {
			return ce.tp, nil
		}
		l.cache.Delete(key)
	}

	tp, err := l.loadFromDB(ctx, userID, channelID)
	if err != nil {
		return TieredPermissions{}, err
	}

	l.cache.Store(key, cacheEntry{tp: tp, expires: time.Now().Add(cacheTTL)})
	return tp, nil
}

// Invalidate removes any cached TieredPermissions for the given
// user/channel pair. Callers should invoke this after mutating the
// user's permissions or group memberships so subsequent reads observe
// fresh data.
func (l *Loader) Invalidate(userID int64, channelID int64) {
	l.cache.Delete(cacheKey{userID: userID, channelID: channelID})
}

// loadFromDB performs the actual queries and assembles the
// TieredPermissions without consulting the cache.
func (l *Loader) loadFromDB(ctx context.Context, userID int64, channelID int64) (TieredPermissions, error) {
	tp := NewTieredPermissions()

	// 1. ServerGroup tier.
	groupIDs, err := l.LoadServerGroupsForUser(ctx, userID)
	if err != nil {
		return TieredPermissions{}, fmt.Errorf("loading server groups for user %d: %w", userID, err)
	}
	var groupSets []PermissionSet
	for _, gid := range groupIDs {
		set, err := l.loadServerGroupPermissions(ctx, gid)
		if err != nil {
			return TieredPermissions{}, fmt.Errorf("loading server group %d permissions: %w", gid, err)
		}
		groupSets = append(groupSets, set)
	}
	tp.Set(TierServerGroup, MergeSet(groupSets...))

	// 2. ClientSpecific tier (server-wide client perms, channel_id NULL).
	clientServerSet, err := l.loadClientPermissions(ctx, userID, true /*nullChannel*/)
	if err != nil {
		return TieredPermissions{}, fmt.Errorf("loading client-specific permissions for user %d: %w", userID, err)
	}
	tp.Set(TierClientSpecific, clientServerSet)

	// 3. ChannelSpecific tier (channel-scoped client perms).
	channelSpecificSet, err := l.loadClientPermissionsForChannel(ctx, userID, channelID)
	if err != nil {
		return TieredPermissions{}, fmt.Errorf("loading channel-specific permissions for user %d channel %d: %w", userID, channelID, err)
	}
	tp.Set(TierChannelSpecific, channelSpecificSet)

	// 4. ChannelGroup tier.
	channelGroupIDs, err := l.LoadChannelGroupsForUser(ctx, userID, channelID)
	if err != nil {
		return TieredPermissions{}, fmt.Errorf("loading channel groups for user %d channel %d: %w", userID, channelID, err)
	}
	var channelGroupSets []PermissionSet
	for _, cgid := range channelGroupIDs {
		set, err := l.loadChannelGroupPermissions(ctx, cgid)
		if err != nil {
			return TieredPermissions{}, fmt.Errorf("loading channel group %d permissions: %w", cgid, err)
		}
		channelGroupSets = append(channelGroupSets, set)
	}
	tp.Set(TierChannelGroup, MergeSet(channelGroupSets...))

	// 5. ChannelClient tier.
	channelClientSet, err := l.loadChannelClientPermissions(ctx, userID, channelID)
	if err != nil {
		return TieredPermissions{}, fmt.Errorf("loading channel-client permissions for user %d channel %d: %w", userID, channelID, err)
	}
	tp.Set(TierChannelClient, channelClientSet)

	return tp, nil
}

// LoadServerGroupsForUser returns the IDs of all server groups the
// user belongs to. Returns an empty (non-nil) slice if the user has no
// server group memberships.
func (l *Loader) LoadServerGroupsForUser(ctx context.Context, userID int64) ([]int64, error) {
	const q = `SELECT server_group_id FROM server_group_members WHERE user_id = $1 ORDER BY server_group_id`
	rows, err := l.store.DB().QueryContext(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// LoadChannelGroupForUser returns the single channel group ID the user
// has on the given channel, or 0 if the user has none. TS3 allows at
// most one channel group per user per channel; if the data model
// contains more than one (e.g. due to manual edits), the lowest ID is
// returned for determinism.
func (l *Loader) LoadChannelGroupForUser(ctx context.Context, userID int64, channelID int64) (int64, error) {
	const q = `SELECT channel_group_id FROM channel_group_members
		WHERE user_id = $1 AND channel_id = $2
		ORDER BY channel_group_id ASC LIMIT 1`
	var id int64
	err := l.store.DB().QueryRowContext(ctx, q, userID, channelID).Scan(&id)
	if err != nil {
		return 0, nil // no row is not an error here
	}
	return id, nil
}

// LoadChannelGroupsForUser returns all channel group IDs the user has
// on the given channel. Used internally so the merge can tolerate
// multiple rows even though TS3 normally allows one.
func (l *Loader) LoadChannelGroupsForUser(ctx context.Context, userID int64, channelID int64) ([]int64, error) {
	const q = `SELECT channel_group_id FROM channel_group_members
		WHERE user_id = $1 AND channel_id = $2
		ORDER BY channel_group_id`
	rows, err := l.store.DB().QueryContext(ctx, q, userID, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// loadServerGroupPermissions loads all permissions attached to a
// server group via server_group_permissions JOIN permissions.
func (l *Loader) loadServerGroupPermissions(ctx context.Context, groupID int64) (PermissionSet, error) {
	const q = `SELECT p.permission_key, p.value, p.grant_value, p.skip_flag, p.negate_flag
		FROM server_group_permissions sgp
		JOIN permissions p ON p.id = sgp.permission_id
		WHERE sgp.server_group_id = $1`
	return l.loadPermissionRows(ctx, q, groupID)
}

// loadChannelGroupPermissions loads all permissions attached to a
// channel group via channel_group_permissions JOIN permissions.
func (l *Loader) loadChannelGroupPermissions(ctx context.Context, groupID int64) (PermissionSet, error) {
	const q = `SELECT p.permission_key, p.value, p.grant_value, p.skip_flag, p.negate_flag
		FROM channel_group_permissions cgp
		JOIN permissions p ON p.id = cgp.permission_id
		WHERE cgp.channel_group_id = $1`
	return l.loadPermissionRows(ctx, q, groupID)
}

// loadClientPermissions loads client_permissions for the user. If
// nullChannel is true, only server-wide entries (channel_id IS NULL)
// are returned; otherwise only channel-scoped entries for the user's
// target channel are returned via loadClientPermissionsForChannel.
func (l *Loader) loadClientPermissions(ctx context.Context, userID int64, nullChannel bool) (PermissionSet, error) {
	const q = `SELECT p.permission_key, p.value, p.grant_value, p.skip_flag, p.negate_flag
		FROM client_permissions cp
		JOIN permissions p ON p.id = cp.permission_id
		WHERE cp.user_id = $1 AND cp.channel_id IS NULL`
	if !nullChannel {
		panic("loader: loadClientPermissions(nullChannel=false) is not supported; use loadClientPermissionsForChannel")
	}
	return l.loadPermissionRows(ctx, q, userID)
}

// loadClientPermissionsForChannel loads client_permissions scoped to a
// specific channel (channel_id = channelID).
func (l *Loader) loadClientPermissionsForChannel(ctx context.Context, userID int64, channelID int64) (PermissionSet, error) {
	const q = `SELECT p.permission_key, p.value, p.grant_value, p.skip_flag, p.negate_flag
		FROM client_permissions cp
		JOIN permissions p ON p.id = cp.permission_id
		WHERE cp.user_id = $1 AND cp.channel_id = $2`
	return l.loadPermissionRows(ctx, q, userID, channelID)
}

// loadChannelClientPermissions loads channel_client_permissions for the
// user on the given channel.
func (l *Loader) loadChannelClientPermissions(ctx context.Context, userID int64, channelID int64) (PermissionSet, error) {
	const q = `SELECT p.permission_key, p.value, p.grant_value, p.skip_flag, p.negate_flag
		FROM channel_client_permissions ccp
		JOIN permissions p ON p.id = ccp.permission_id
		WHERE ccp.user_id = $1 AND ccp.channel_id = $2`
	return l.loadPermissionRows(ctx, q, userID, channelID)
}

// loadPermissionRows runs a query that returns the standard five
// permission columns (permission_key, value, grant_value, skip_flag,
// negate_flag) and maps each row to a Permission in a fresh
// PermissionSet. The Type is inferred from the key prefix.
func (l *Loader) loadPermissionRows(ctx context.Context, query string, args ...interface{}) (PermissionSet, error) {
	rows, err := l.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	set := NewPermissionSet()
	for rows.Next() {
		var (
			keyText    string
			value      int
			grant      int
			skipFlag   bool
			negateFlag bool
		)
		if err := rows.Scan(&keyText, &value, &grant, &skipFlag, &negateFlag); err != nil {
			return nil, err
		}
		set.Set(&Permission{
			Key:    PermissionKey(keyText),
			Type:   inferType(keyText),
			Value:  value,
			Grant:  grant,
			Skip:   skipFlag,
			Negate: negateFlag,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return set, nil
}

// inferType maps a permission key prefix to its PermissionType:
// "b_" → Boolean, "i_" → Integer. Unknown prefixes default to
// Integer, which is the safer choice for power comparisons.
func inferType(key string) PermissionType {
	if strings.HasPrefix(key, "b_") {
		return PermissionTypeBoolean
	}
	return PermissionTypeInteger
}

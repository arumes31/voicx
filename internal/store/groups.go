// groups.go implements the store layer for wave-6a permission/group
// management: server/channel group CRUD, membership (with timed
// assignments), the permission write path (all five tiers), and the audit
// log.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Group describes a server or channel group.
type Group struct {
	ID          int64
	Name        string
	SortID      int
	IsTemplate  bool
	Icon        string
	Color       string
	Hoist       bool
	MemberCount int
}

// ListGroups returns groups ordered by sort_id/name with member counts.
// groupType is "server" or "channel".
func (s *Store) ListGroups(ctx context.Context, groupType string) ([]Group, error) {
	var q string
	if groupType == "channel" {
		q = `SELECT g.id, g.name, g.sort_id,
		            (SELECT COUNT(*) FROM channel_group_members m WHERE m.channel_group_id = g.id)
		     FROM channel_groups g ORDER BY g.sort_id, g.name`
	} else {
		q = `SELECT g.id, g.name, g.sort_id, g.is_template, g.icon, g.color, g.hoist,
		            (SELECT COUNT(*) FROM server_group_members m WHERE m.server_group_id = g.id)
		     FROM server_groups g ORDER BY g.sort_id, g.name`
	}
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("listing %s groups: %w", groupType, err)
	}
	defer closeRows(rows)
	var out []Group
	for rows.Next() {
		var g Group
		if groupType == "channel" {
			if err := rows.Scan(&g.ID, &g.Name, &g.SortID, &g.MemberCount); err != nil {
				return nil, err
			}
		} else {
			if err := rows.Scan(&g.ID, &g.Name, &g.SortID, &g.IsTemplate, &g.Icon, &g.Color, &g.Hoist, &g.MemberCount); err != nil {
				return nil, err
			}
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GetGroup returns one group by ID ("" fields when absent).
func (s *Store) GetGroup(ctx context.Context, groupType string, id int64) (*Group, error) {
	groups, err := s.ListGroups(ctx, groupType)
	if err != nil {
		return nil, err
	}
	for i := range groups {
		if groups[i].ID == id {
			return &groups[i], nil
		}
	}
	return nil, nil
}

// CreateGroup inserts a group and returns its ID.
func (s *Store) CreateGroup(ctx context.Context, groupType, name string, sortID int) (int64, error) {
	var q string
	if groupType == "channel" {
		q = `INSERT INTO channel_groups (name, sort_id) VALUES ($1, $2) RETURNING id`
	} else {
		q = `INSERT INTO server_groups (name, sort_id) VALUES ($1, $2) RETURNING id`
	}
	var id int64
	if err := s.db.QueryRowContext(ctx, q, name, sortID).Scan(&id); err != nil {
		return 0, fmt.Errorf("creating %s group: %w", groupType, err)
	}
	return id, nil
}

// RenameGroup renames a group.
func (s *Store) RenameGroup(ctx context.Context, groupType string, id int64, name string) error {
	table := "server_groups"
	if groupType == "channel" {
		table = "channel_groups"
	}
	res, err := s.db.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET name = $1 WHERE id = $2`, table), name, id)
	if err != nil {
		return fmt.Errorf("renaming group: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("group not found")
	}
	return nil
}

// DeleteGroup deletes a group. It refuses when the group still has members
// and force is false.
func (s *Store) DeleteGroup(ctx context.Context, groupType string, id int64, force bool) error {
	g, err := s.GetGroup(ctx, groupType, id)
	if err != nil {
		return err
	}
	if g == nil {
		return errors.New("group not found")
	}
	if g.MemberCount > 0 && !force {
		return fmt.Errorf("group has %d members (use force)", g.MemberCount)
	}
	table := "server_groups"
	if groupType == "channel" {
		table = "channel_groups"
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id = $1`, table), id); err != nil {
		return fmt.Errorf("deleting group: %w", err)
	}
	return nil
}

// AssignServerGroup adds a user to a server group (idempotent). expiresIn >
// 0 sets an expiry that the reaper enforces (145).
func (s *Store) AssignServerGroup(ctx context.Context, groupID, userID int64, expiresIn time.Duration) error {
	var expires any
	if expiresIn > 0 {
		expires = time.Now().Add(expiresIn)
	}
	const q = `INSERT INTO server_group_members (user_id, server_group_id, expires_at)
	          VALUES ($1, $2, $3) ON CONFLICT (user_id, server_group_id) DO UPDATE SET expires_at = EXCLUDED.expires_at`
	if _, err := s.db.ExecContext(ctx, q, userID, groupID, expires); err != nil {
		return fmt.Errorf("assigning server group: %w", err)
	}
	return nil
}

// UnassignServerGroup removes a user from a server group.
func (s *Store) UnassignServerGroup(ctx context.Context, groupID, userID int64) error {
	const q = `DELETE FROM server_group_members WHERE user_id = $1 AND server_group_id = $2`
	if _, err := s.db.ExecContext(ctx, q, userID, groupID); err != nil {
		return fmt.Errorf("unassigning server group: %w", err)
	}
	return nil
}

// AssignChannelGroup adds a user to a channel group on a channel (idempotent).
// A user holds at most one channel group per channel (TS3 semantics), so any
// previous membership on that channel is replaced; the table's PK covers the
// group ID and cannot express that with ON CONFLICT alone.
func (s *Store) AssignChannelGroup(ctx context.Context, groupID, userID, channelID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning channel group assignment: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := assignChannelGroupTx(ctx, tx, groupID, userID, channelID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing channel group assignment: %w", err)
	}
	return nil
}

func assignChannelGroupTx(ctx context.Context, tx *sql.Tx, groupID, userID, channelID int64) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		fmt.Sprintf("channel-group:%d:%d", userID, channelID)); err != nil {
		return fmt.Errorf("locking channel group membership: %w", err)
	}
	const del = `DELETE FROM channel_group_members WHERE user_id = $1 AND channel_id = $2`
	if _, err := tx.ExecContext(ctx, del, userID, channelID); err != nil {
		return fmt.Errorf("replacing channel group membership: %w", err)
	}
	const ins = `INSERT INTO channel_group_members (user_id, channel_id, channel_group_id)
	          VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`
	if _, err := tx.ExecContext(ctx, ins, userID, channelID, groupID); err != nil {
		return fmt.Errorf("assigning channel group: %w", err)
	}
	return nil
}

// ApplyChannelGroupAutoAssignment selects the highest-priority enabled rule
// for a subscribed channel and atomically replaces the user's channel group.
func (s *Store) ApplyChannelGroupAutoAssignment(ctx context.Context, userID, channelID int64) (int64, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("beginning channel-group auto assignment: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	const rule = `SELECT channel_group_id FROM channel_group_auto_rules
	              WHERE channel_id = $1 AND enabled
	              ORDER BY priority DESC, id LIMIT 1`
	var groupID int64
	if err := tx.QueryRowContext(ctx, rule, channelID).Scan(&groupID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("looking up channel-group auto rule: %w", err)
	}
	if err := assignChannelGroupTx(ctx, tx, groupID, userID, channelID); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("committing channel-group auto assignment: %w", err)
	}
	return groupID, true, nil
}

// UnassignChannelGroup removes a user from a channel group on a channel.
func (s *Store) UnassignChannelGroup(ctx context.Context, userID, channelID int64) error {
	const q = `DELETE FROM channel_group_members WHERE user_id = $1 AND channel_id = $2`
	if _, err := s.db.ExecContext(ctx, q, userID, channelID); err != nil {
		return fmt.Errorf("unassigning channel group: %w", err)
	}
	return nil
}

// ExpiredGroupMembers removes expired server-group memberships and returns
// the affected (userID, groupID) pairs (for cache invalidation + notify).
func (s *Store) ExpiredGroupMembers(ctx context.Context, now time.Time) ([][2]int64, error) {
	const del = `DELETE FROM server_group_members WHERE expires_at IS NOT NULL AND expires_at <= $1
	             RETURNING user_id, server_group_id`
	rows, err := s.db.QueryContext(ctx, del, now)
	if err != nil {
		return nil, fmt.Errorf("reaping expired group members: %w", err)
	}
	defer closeRows(rows)
	var out [][2]int64
	for rows.Next() {
		var pair [2]int64
		if err := rows.Scan(&pair[0], &pair[1]); err != nil {
			return nil, err
		}
		out = append(out, pair)
	}
	return out, rows.Err()
}

// UserGroupIDs returns the server group IDs of a user (for "has any group"
// checks during default-group assignment).
func (s *Store) UserGroupIDs(ctx context.Context, userID int64) ([]int64, error) {
	const q = `SELECT server_group_id FROM server_group_members WHERE user_id = $1`
	rows, err := s.db.QueryContext(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// UserUniqueID maps a users.id back to its unique ID (reaper notifications).
func (s *Store) UserUniqueID(ctx context.Context, userID int64) (string, error) {
	const q = `SELECT unique_id FROM users WHERE id = $1`
	var uniqueID string
	if err := s.db.QueryRowContext(ctx, q, userID).Scan(&uniqueID); err != nil {
		return "", fmt.Errorf("looking up user %d: %w", userID, err)
	}
	return uniqueID, nil
}

// SetGroupIcon marks a server group's icon file name (surfaced in GroupList).
func (s *Store) SetGroupIcon(ctx context.Context, groupID int64, icon string) error {
	const q = `UPDATE server_groups SET icon = $1 WHERE id = $2`
	result, err := s.db.ExecContext(ctx, q, icon, groupID)
	if err != nil {
		return fmt.Errorf("setting group icon: %w", err)
	}
	return checkGroupIconUpdate(result)
}

func checkGroupIconUpdate(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading group icon update result: %w", err)
	}
	if affected == 0 {
		return errors.New("group not found")
	}
	return nil
}

// SetGroupCosmetics updates a server group's colour, hoisting and sort order
// (178/179). A nil field is left unchanged so a dialog may send only what the
// operator touched; colour/hoist exist only on server groups.
func (s *Store) SetGroupCosmetics(ctx context.Context, groupID int64, color *string, hoist *bool, sortID *int) error {
	sets := make([]string, 0, 3)
	args := make([]any, 0, 4)
	if color != nil {
		args = append(args, *color)
		sets = append(sets, fmt.Sprintf("color = $%d", len(args)))
	}
	if hoist != nil {
		args = append(args, *hoist)
		sets = append(sets, fmt.Sprintf("hoist = $%d", len(args)))
	}
	if sortID != nil {
		args = append(args, *sortID)
		sets = append(sets, fmt.Sprintf("sort_id = $%d", len(args)))
	}
	if len(sets) == 0 {
		return errors.New("no cosmetic fields to set")
	}
	args = append(args, groupID)
	// #nosec G201 -- every SET expression is assembled from the three fixed
	// column names above; all caller-controlled values remain parameters.
	q := fmt.Sprintf(`UPDATE server_groups SET %s WHERE id = $%d`, strings.Join(sets, ", "), len(args))
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("setting group cosmetics: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("group not found")
	}
	return nil
}

// FindGroupByName returns a group by name, or nil.
func (s *Store) FindGroupByName(ctx context.Context, groupType, name string) (*Group, error) {
	groups, err := s.ListGroups(ctx, groupType)
	if err != nil {
		return nil, err
	}
	for i := range groups {
		if groups[i].Name == name {
			return &groups[i], nil
		}
	}
	return nil, nil
}

// GroupMember describes one member of a group (with the user join).
type GroupMember struct {
	UniqueID  string
	Nickname  string
	ExpiresAt *time.Time // nil = permanent
}

// ListServerGroupMembers returns the members of a server group, ordered by
// nickname.
func (s *Store) ListServerGroupMembers(ctx context.Context, groupID int64) ([]GroupMember, error) {
	const q = `SELECT u.unique_id, COALESCE(u.nickname, ''), m.expires_at
		FROM server_group_members m JOIN users u ON u.id = m.user_id
		WHERE m.server_group_id = $1 ORDER BY u.nickname, u.unique_id`
	return s.scanGroupMembers(ctx, q, groupID)
}

// ListChannelGroupMembers returns the members of a channel group on one
// channel (membership is channel-scoped).
func (s *Store) ListChannelGroupMembers(ctx context.Context, groupID, channelID int64) ([]GroupMember, error) {
	const q = `SELECT u.unique_id, COALESCE(u.nickname, ''), NULL
		FROM channel_group_members m JOIN users u ON u.id = m.user_id
		WHERE m.channel_group_id = $1 AND m.channel_id = $2 ORDER BY u.nickname, u.unique_id`
	rows, err := s.db.QueryContext(ctx, q, groupID, channelID)
	if err != nil {
		return nil, fmt.Errorf("listing channel group members: %w", err)
	}
	defer closeRows(rows)
	var out []GroupMember
	for rows.Next() {
		var gm GroupMember
		var expires any
		if err := rows.Scan(&gm.UniqueID, &gm.Nickname, &expires); err != nil {
			return nil, err
		}
		out = append(out, gm)
	}
	return out, rows.Err()
}

// scanGroupMembers runs a member query with a single group-ID argument.
func (s *Store) scanGroupMembers(ctx context.Context, q string, groupID int64) ([]GroupMember, error) {
	rows, err := s.db.QueryContext(ctx, q, groupID)
	if err != nil {
		return nil, fmt.Errorf("listing group members: %w", err)
	}
	defer closeRows(rows)
	var out []GroupMember
	for rows.Next() {
		var gm GroupMember
		if err := rows.Scan(&gm.UniqueID, &gm.Nickname, &gm.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, gm)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Permission write path (all five tiers)
// ---------------------------------------------------------------------------

// PermTier identifies the write-path tier.
type PermTier string

const (
	PermTierServerGroup   PermTier = "server_group"
	PermTierClient        PermTier = "client"
	PermTierChannelClient PermTier = "channel_client"
	PermTierChannel       PermTier = "channel"
	PermTierChannelGroup  PermTier = "channel_group"
)

// PermTarget addresses a permission write: group IDs for group tiers,
// userID for client tiers (0 = unused), channelID for channel tiers.
type PermTarget struct {
	GroupID   int64
	UserID    int64
	ChannelID int64
}

type permissionExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// upsertPermissionRow finds or creates the permissions row for the exact
// (key, value, grant, skip, negate) tuple and returns its id.
func upsertPermissionRow(ctx context.Context, db permissionExecutor, key string, value, grant int, skip, negate bool) (int64, error) {
	const sel = `SELECT id FROM permissions
	             WHERE permission_key = $1 AND value = $2 AND grant_value = $3 AND skip_flag = $4 AND negate_flag = $5`
	var id int64
	err := db.QueryRowContext(ctx, sel, key, value, grant, skip, negate).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("finding permission row: %w", err)
	}
	const ins = `INSERT INTO permissions (permission_key, value, grant_value, skip_flag, negate_flag)
	             VALUES ($1, $2, $3, $4, $5) RETURNING id`
	if err := db.QueryRowContext(ctx, ins, key, value, grant, skip, negate).Scan(&id); err != nil {
		return 0, fmt.Errorf("creating permission row: %w", err)
	}
	return id, nil
}

// SetPermission writes a permission for a target on a tier: existing entries
// for the same key on the same target are replaced.
func (s *Store) SetPermission(ctx context.Context, tier PermTier, target PermTarget, key string, value, grant int, skip, negate bool) error {
	return setPermissionWith(ctx, s.db, tier, target, key, value, grant, skip, negate)
}

func setPermissionWith(ctx context.Context, db permissionExecutor, tier PermTier, target PermTarget, key string, value, grant int, skip, negate bool) error {
	permID, err := upsertPermissionRow(ctx, db, key, value, grant, skip, negate)
	if err != nil {
		return err
	}

	// Remove previous entries for this key on this target, then link the
	// new row.
	var del, ins string
	var delArgs, insArgs []any
	switch tier {
	case PermTierServerGroup:
		del = `DELETE FROM server_group_permissions WHERE server_group_id = $1 AND permission_id IN (SELECT id FROM permissions WHERE permission_key = $2)`
		delArgs = []any{target.GroupID, key}
		ins = `INSERT INTO server_group_permissions (server_group_id, permission_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`
		insArgs = []any{target.GroupID, permID}
	case PermTierChannelGroup:
		del = `DELETE FROM channel_group_permissions WHERE channel_group_id = $1 AND permission_id IN (SELECT id FROM permissions WHERE permission_key = $2)`
		delArgs = []any{target.GroupID, key}
		ins = `INSERT INTO channel_group_permissions (channel_group_id, permission_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`
		insArgs = []any{target.GroupID, permID}
	case PermTierClient:
		del = `DELETE FROM client_permissions WHERE user_id = $1 AND channel_id IS NULL AND permission_id IN (SELECT id FROM permissions WHERE permission_key = $2)`
		delArgs = []any{target.UserID, key}
		ins = `INSERT INTO client_permissions (user_id, permission_id, channel_id) VALUES ($1, $2, NULL) ON CONFLICT DO NOTHING`
		insArgs = []any{target.UserID, permID}
	case PermTierChannelClient:
		del = `DELETE FROM channel_client_permissions WHERE user_id = $1 AND channel_id = $2 AND permission_id IN (SELECT id FROM permissions WHERE permission_key = $3)`
		delArgs = []any{target.UserID, target.ChannelID, key}
		ins = `INSERT INTO channel_client_permissions (user_id, channel_id, permission_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`
		insArgs = []any{target.UserID, target.ChannelID, permID}
	case PermTierChannel:
		del = `DELETE FROM channel_permissions WHERE channel_id = $1 AND permission_id IN (SELECT id FROM permissions WHERE permission_key = $2)`
		delArgs = []any{target.ChannelID, key}
		ins = `INSERT INTO channel_permissions (channel_id, permission_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`
		insArgs = []any{target.ChannelID, permID}
	default:
		return fmt.Errorf("unknown permission tier %q", tier)
	}

	if _, err := db.ExecContext(ctx, del, delArgs...); err != nil {
		return fmt.Errorf("replacing %s permission: %w", tier, err)
	}
	if _, err := db.ExecContext(ctx, ins, insArgs...); err != nil {
		return fmt.Errorf("setting %s permission: %w", tier, err)
	}
	return nil
}

// PermEntry describes one stored permission entry (read path, wave 6b).
type PermEntry struct {
	Key    string
	Value  int
	Grant  int
	Skip   bool
	Negate bool
}

// ListPermissions returns the current permission entries of a target on a
// tier (the editor's read path).
func (s *Store) ListPermissions(ctx context.Context, tier PermTier, target PermTarget) ([]PermEntry, error) {
	const cols = `SELECT p.permission_key, p.value, p.grant_value, p.skip_flag, p.negate_flag FROM permissions p`
	var q string
	var args []any
	switch tier {
	case PermTierServerGroup:
		q = cols + ` JOIN server_group_permissions x ON x.permission_id = p.id WHERE x.server_group_id = $1 ORDER BY p.permission_key`
		args = []any{target.GroupID}
	case PermTierChannelGroup:
		q = cols + ` JOIN channel_group_permissions x ON x.permission_id = p.id WHERE x.channel_group_id = $1 ORDER BY p.permission_key`
		args = []any{target.GroupID}
	case PermTierClient:
		q = cols + ` JOIN client_permissions x ON x.permission_id = p.id WHERE x.user_id = $1 AND x.channel_id IS NULL ORDER BY p.permission_key`
		args = []any{target.UserID}
	case PermTierChannelClient:
		q = cols + ` JOIN channel_client_permissions x ON x.permission_id = p.id WHERE x.user_id = $1 AND x.channel_id = $2 ORDER BY p.permission_key`
		args = []any{target.UserID, target.ChannelID}
	case PermTierChannel:
		q = cols + ` JOIN channel_permissions x ON x.permission_id = p.id WHERE x.channel_id = $1 ORDER BY p.permission_key`
		args = []any{target.ChannelID}
	default:
		return nil, fmt.Errorf("unknown permission tier %q", tier)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("listing %s permissions: %w", tier, err)
	}
	defer closeRows(rows)
	var out []PermEntry
	for rows.Next() {
		var e PermEntry
		if err := rows.Scan(&e.Key, &e.Value, &e.Grant, &e.Skip, &e.Negate); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// UnsetPermission removes a permission entry for a target on a tier.
func (s *Store) UnsetPermission(ctx context.Context, tier PermTier, target PermTarget, key string) error {
	return unsetPermissionWith(ctx, s.db, tier, target, key)
}

func unsetPermissionWith(ctx context.Context, db permissionExecutor, tier PermTier, target PermTarget, key string) error {
	var del string
	var args []any
	switch tier {
	case PermTierServerGroup:
		del = `DELETE FROM server_group_permissions WHERE server_group_id = $1 AND permission_id IN (SELECT id FROM permissions WHERE permission_key = $2)`
		args = []any{target.GroupID, key}
	case PermTierChannelGroup:
		del = `DELETE FROM channel_group_permissions WHERE channel_group_id = $1 AND permission_id IN (SELECT id FROM permissions WHERE permission_key = $2)`
		args = []any{target.GroupID, key}
	case PermTierClient:
		del = `DELETE FROM client_permissions WHERE user_id = $1 AND channel_id IS NULL AND permission_id IN (SELECT id FROM permissions WHERE permission_key = $2)`
		args = []any{target.UserID, key}
	case PermTierChannelClient:
		del = `DELETE FROM channel_client_permissions WHERE user_id = $1 AND channel_id = $2 AND permission_id IN (SELECT id FROM permissions WHERE permission_key = $3)`
		args = []any{target.UserID, target.ChannelID, key}
	case PermTierChannel:
		del = `DELETE FROM channel_permissions WHERE channel_id = $1 AND permission_id IN (SELECT id FROM permissions WHERE permission_key = $2)`
		args = []any{target.ChannelID, key}
	default:
		return fmt.Errorf("unknown permission tier %q", tier)
	}
	if _, err := db.ExecContext(ctx, del, args...); err != nil {
		return fmt.Errorf("unsetting %s permission: %w", tier, err)
	}
	return nil
}

// CopyPermissions atomically removes destination-only keys and writes the
// source entries. Callers perform authorization before entering this store
// operation; the transaction guarantees Replace cannot leave a partial copy.
func (s *Store) CopyPermissions(ctx context.Context, tier PermTier, target PermTarget, remove []string, entries []PermEntry) (retErr error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning permission copy: %w", err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rolling back permission copy: %w", err))
		}
	}()
	for _, key := range remove {
		if err := unsetPermissionWith(ctx, tx, tier, target, key); err != nil {
			return err
		}
	}
	for _, entry := range entries {
		if err := setPermissionWith(ctx, tx, tier, target, entry.Key, entry.Value, entry.Grant, entry.Skip, entry.Negate); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing permission copy: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Audit log (149)
// ---------------------------------------------------------------------------

// AuditEntry is one audit log row.
type AuditEntry struct {
	ID        int64
	Actor     string
	Action    string
	Target    string
	Detail    string
	CreatedAt time.Time
}

// Audit writes an audit log entry.
func (s *Store) Audit(ctx context.Context, actor, action, target, detail string) {
	const q = `INSERT INTO audit_log (actor_unique_id, action, target, detail) VALUES ($1, $2, $3, $4)`
	if _, err := s.db.ExecContext(ctx, q, actor, action, target, detail); err != nil {
		// Audit is best-effort; it must never break the action itself.
		_ = err
	}
}

// AuditList returns up to limit audit entries, newest first, with id <
// beforeID (0 = latest page).
func (s *Store) AuditList(ctx context.Context, beforeID int64, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT id, actor_unique_id, action, target, detail, created_at FROM audit_log`
	args := []any{}
	if beforeID > 0 {
		q += ` WHERE id < $1 ORDER BY id DESC LIMIT $2`
		args = append(args, beforeID, limit)
	} else {
		q += ` ORDER BY id DESC LIMIT $1`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("listing audit log: %w", err)
	}
	defer closeRows(rows)
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.Actor, &e.Action, &e.Target, &e.Detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CompressPermissionAudit replaces permission mutation rows older than the
// cutoff with daily action/target counts in one transaction. It returns the
// number of detailed rows removed.
func (s *Store) CompressPermissionAudit(ctx context.Context, cutoff time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("beginning audit compression: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	const auditCompressionLock int64 = 0x564F494358415544
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, auditCompressionLock); err != nil {
		return 0, fmt.Errorf("locking audit compression: %w", err)
	}
	const aggregate = `INSERT INTO audit_log_daily (day, action, target, event_count)
	                   SELECT created_at::date, action, target, COUNT(*)
	                   FROM audit_log
	                   WHERE created_at < $1 AND (action LIKE 'perm_%' OR action LIKE '%group%')
	                   GROUP BY created_at::date, action, target
	                   ON CONFLICT (day, action, target) DO UPDATE
	                   SET event_count = audit_log_daily.event_count + EXCLUDED.event_count`
	if _, err := tx.ExecContext(ctx, aggregate, cutoff); err != nil {
		return 0, fmt.Errorf("aggregating audit log: %w", err)
	}
	const remove = `DELETE FROM audit_log
	                WHERE created_at < $1 AND (action LIKE 'perm_%' OR action LIKE '%group%')`
	res, err := tx.ExecContext(ctx, remove, cutoff)
	if err != nil {
		return 0, fmt.Errorf("removing compressed audit rows: %w", err)
	}
	count, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing audit compression: %w", err)
	}
	return count, nil
}

// groups.go implements the wave-6a permission/group management handlers:
// group CRUD + membership, the permission write path (with grant-cap
// semantics), built-in permission templates, the resolver trace, group
// icons, and the audit log. All write actions are audited.
package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"voicx/internal/netproto"
	"voicx/internal/permissions"
	"voicx/internal/store"
)

// Broadcast event types for group management.
const (
	eventGroupAssigned   = "group_assigned"
	eventGroupUnassigned = "group_unassigned"
	eventGroupExpired    = "group_expired"
)

// groupManageKey returns the gate for group management of a type.
func groupManageKey(groupType string) permissions.PermissionKey {
	if groupType == "channel" {
		return permissions.PermissionKeyChannelGroupManage
	}
	return permissions.PermissionKeyServerGroupManage
}

// audit writes an audit entry (best-effort; nil store = no-op).
func (s *TCPServer) audit(ctx context.Context, actor, action, target, detail string) {
	if s.deps == nil || s.deps.Groups == nil {
		return
	}
	s.deps.Groups.Audit(ctx, actor, action, target, detail)
}

// --- group list / CRUD -------------------------------------------------------

// groupListResponse builds the wire response for a group type.
func (s *TCPServer) groupListResponse(ctx context.Context, groupType string) (netproto.GroupListResponse, error) {
	groups, err := s.deps.Groups.ListGroups(ctx, groupType)
	if err != nil {
		return netproto.GroupListResponse{}, err
	}
	resp := netproto.GroupListResponse{Type: groupType, Groups: []netproto.GroupEntry{}}
	for _, g := range groups {
		resp.Groups = append(resp.Groups, netproto.GroupEntry{
			ID: g.ID, Name: g.Name, SortID: g.SortID, MemberCount: g.MemberCount,
			Icon: g.Icon, Color: g.Color, Hoist: g.Hoist,
		})
	}
	return resp, nil
}

// groupTypeOf normalizes a message group type: anything but "channel" is a
// server group.
func groupTypeOf(t string) string {
	if t == "channel" {
		return "channel"
	}
	return "server"
}

// handleGroupList lists groups with member counts and cosmetics.
func (s *TCPServer) handleGroupList(_ context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.GroupList
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed group_list: "+err.Error())
	}
	if s.deps == nil || s.deps.Groups == nil {
		return s.sendError(client, errCodeUnavailable, "group store unavailable")
	}
	groupType := groupTypeOf(msg.Type)
	resp, err := s.groupListResponse(context.Background(), groupType)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "group list failed")
	}
	return s.writeMessage(client, netproto.MsgGroupListResponse, resp)
}

// handleGroupCreate creates a group. Gated by b_*_group_manage.
func (s *TCPServer) handleGroupCreate(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.GroupCreate
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed group_create: "+err.Error())
	}
	if msg.Name == "" {
		return s.sendError(client, errCodeMalformed, "group name is required")
	}
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	key := groupManageKey(msg.Type)
	if !pc.granted(key) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(key))
	}
	if s.deps == nil || s.deps.Groups == nil {
		return s.sendError(client, errCodeUnavailable, "group store unavailable")
	}
	groupType := groupTypeOf(msg.Type)
	id, err := s.deps.Groups.CreateGroup(ctx, groupType, msg.Name, msg.SortID)
	if err != nil {
		return s.sendError(client, errCodeMalformed, "group create failed: "+err.Error())
	}
	s.audit(ctx, client.UniqueID, "group_create", groupType+":"+msg.Name, fmt.Sprintf("id=%d", id))
	resp, err := s.groupListResponse(ctx, groupType)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "group list failed")
	}
	return s.writeMessage(client, netproto.MsgGroupListResponse, resp)
}

// handleGroupRename renames a group.
func (s *TCPServer) handleGroupRename(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.GroupRename
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed group_rename: "+err.Error())
	}
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	key := groupManageKey(msg.Type)
	if !pc.granted(key) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(key))
	}
	if s.deps == nil || s.deps.Groups == nil {
		return s.sendError(client, errCodeUnavailable, "group store unavailable")
	}
	if err := s.deps.Groups.RenameGroup(ctx, msg.Type, msg.GroupID, msg.Name); err != nil {
		return s.sendError(client, errCodeNotFound, "rename failed: "+err.Error())
	}
	s.audit(ctx, client.UniqueID, "group_rename", fmt.Sprintf("%s:%d", msg.Type, msg.GroupID), msg.Name)
	return nil
}

// handleGroupDelete deletes a group (force required when it has members).
func (s *TCPServer) handleGroupDelete(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.GroupDelete
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed group_delete: "+err.Error())
	}
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	key := groupManageKey(msg.Type)
	if !pc.granted(key) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(key))
	}
	if s.deps == nil || s.deps.Groups == nil {
		return s.sendError(client, errCodeUnavailable, "group store unavailable")
	}
	// The members are the audience of the invalidation, so they have to be
	// read while the memberships still exist (151).
	groupType := groupTypeOf(msg.Type)
	tier := store.PermTierServerGroup
	if groupType == "channel" {
		tier = store.PermTierChannelGroup
	}
	audience := s.permAudience(ctx, tier, store.PermTarget{GroupID: msg.GroupID})
	if err := s.deps.Groups.DeleteGroup(ctx, msg.Type, msg.GroupID, msg.Force); err != nil {
		return s.sendError(client, errCodeMalformed, "delete failed: "+err.Error())
	}
	s.audit(ctx, client.UniqueID, "group_delete", fmt.Sprintf("%s:%d", msg.Type, msg.GroupID), fmt.Sprintf("force=%t", msg.Force))
	if s.deps.Perms != nil {
		s.deps.Perms.InvalidateAll()
	}
	s.notifyPermsInvalid("group_delete", audience)
	return nil
}

// validGroupColor accepts "#rrggbb", or "" to clear back to the theme default
// (178). The value is painted straight into the client's CSS, so anything else
// is refused rather than stored.
func validGroupColor(c string) bool {
	if c == "" {
		return true
	}
	if len(c) != 7 || c[0] != '#' {
		return false
	}
	for i := 1; i < 7; i++ {
		switch {
		case c[i] >= '0' && c[i] <= '9',
			c[i] >= 'a' && c[i] <= 'f',
			c[i] >= 'A' && c[i] <= 'F':
		default:
			return false
		}
	}
	return true
}

// handleGroupEdit writes a server group's cosmetics: colour (178), hoisting
// (179) and sort order. A nil field is left unchanged. Colour and hoist exist
// only on server groups, so this is gated by b_server_group_manage.
func (s *TCPServer) handleGroupEdit(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.GroupEdit
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed group_edit: "+err.Error())
	}
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.granted(permissions.PermissionKeyServerGroupManage) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyServerGroupManage))
	}
	if s.deps == nil || s.deps.Groups == nil {
		return s.sendError(client, errCodeUnavailable, "group store unavailable")
	}
	if msg.Color == nil && msg.Hoist == nil && msg.SortID == nil {
		return s.sendError(client, errCodeMalformed, "group_edit carries no fields")
	}
	if msg.Color != nil && !validGroupColor(*msg.Color) {
		return s.sendError(client, errCodeMalformed, "color must be #rrggbb or empty")
	}
	if err := s.deps.Groups.SetGroupCosmetics(ctx, msg.GroupID, msg.Color, msg.Hoist, msg.SortID); err != nil {
		return s.sendError(client, errCodeNotFound, "group edit failed: "+err.Error())
	}
	detail := ""
	if msg.Color != nil {
		detail += "color=" + *msg.Color + " "
	}
	if msg.Hoist != nil {
		detail += fmt.Sprintf("hoist=%t ", *msg.Hoist)
	}
	if msg.SortID != nil {
		detail += fmt.Sprintf("sort_id=%d", *msg.SortID)
	}
	s.audit(ctx, client.UniqueID, "group_edit", fmt.Sprintf("server:%d", msg.GroupID), strings.TrimSpace(detail))
	// The members are the ones whose nickname colour and hoisted section just
	// moved, so they are the ones that must refetch (151).
	s.notifyPermsInvalid("group_edit", s.serverGroupAudience(ctx, msg.GroupID))
	resp, err := s.groupListResponse(ctx, "server")
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "group list failed")
	}
	return s.writeMessage(client, netproto.MsgGroupListResponse, resp)
}

// --- membership --------------------------------------------------------------

// handleGroupAssign assigns a user to a group (timed when
// ExpiresInSeconds > 0) and invalidates the target's permission cache.
func (s *TCPServer) handleGroupAssign(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.GroupAssign
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed group_assign: "+err.Error())
	}
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	key := groupManageKey(msg.Type)
	if !pc.granted(key) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(key))
	}
	if s.deps == nil || s.deps.Groups == nil || s.deps.Auth == nil {
		return s.sendError(client, errCodeUnavailable, "group store unavailable")
	}
	user, err := s.deps.Auth.LookupUser(ctx, msg.UniqueID)
	if err != nil {
		return s.sendError(client, errCodeNotFound, "target user not found")
	}

	expiresIn := time.Duration(msg.ExpiresInSeconds) * time.Second
	if msg.Type == "channel" {
		if msg.ChannelID == 0 {
			return s.sendError(client, errCodeMalformed, "channel_id is required for channel groups")
		}
		if err := s.deps.Groups.AssignChannelGroup(ctx, msg.GroupID, user.ID, msg.ChannelID); err != nil {
			return s.sendError(client, errCodeUnavailable, "assign failed: "+err.Error())
		}
	} else {
		if err := s.deps.Groups.AssignServerGroup(ctx, msg.GroupID, user.ID, expiresIn); err != nil {
			return s.sendError(client, errCodeUnavailable, "assign failed: "+err.Error())
		}
	}
	s.audit(ctx, client.UniqueID, "group_assign", fmt.Sprintf("%s:%d", msg.Type, msg.GroupID),
		fmt.Sprintf("user=%s expires_in=%ds", msg.UniqueID, msg.ExpiresInSeconds))

	if s.deps.Perms != nil {
		s.deps.Perms.InvalidateAll()
	}
	s.notifyPermsInvalid("group_assign", []string{user.UniqueID})
	if g, _ := s.deps.Groups.GetGroup(ctx, msg.Type, msg.GroupID); g != nil {
		s.notifyGroupEvent(user.UniqueID, eventGroupAssigned, map[string]any{
			"group_id": msg.GroupID, "group_name": g.Name, "by": client.UniqueID,
			"expires_in_seconds": msg.ExpiresInSeconds,
		})
	}
	return nil
}

// handleGroupUnassign removes a user from a group.
func (s *TCPServer) handleGroupUnassign(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.GroupUnassign
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed group_unassign: "+err.Error())
	}
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	key := groupManageKey(msg.Type)
	if !pc.granted(key) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(key))
	}
	if s.deps == nil || s.deps.Groups == nil || s.deps.Auth == nil {
		return s.sendError(client, errCodeUnavailable, "group store unavailable")
	}
	user, err := s.deps.Auth.LookupUser(ctx, msg.UniqueID)
	if err != nil {
		return s.sendError(client, errCodeNotFound, "target user not found")
	}

	if msg.Type == "channel" {
		if err := s.deps.Groups.UnassignChannelGroup(ctx, user.ID, msg.ChannelID); err != nil {
			return s.sendError(client, errCodeUnavailable, "unassign failed: "+err.Error())
		}
	} else {
		if err := s.deps.Groups.UnassignServerGroup(ctx, msg.GroupID, user.ID); err != nil {
			return s.sendError(client, errCodeUnavailable, "unassign failed: "+err.Error())
		}
	}
	s.audit(ctx, client.UniqueID, "group_unassign", fmt.Sprintf("%s:%d", msg.Type, msg.GroupID), "user="+msg.UniqueID)

	if s.deps.Perms != nil {
		s.deps.Perms.InvalidateAll()
	}
	s.notifyPermsInvalid("group_unassign", []string{user.UniqueID})
	if g, _ := s.deps.Groups.GetGroup(ctx, msg.Type, msg.GroupID); g != nil {
		s.notifyGroupEvent(user.UniqueID, eventGroupUnassigned, map[string]any{
			"group_id": msg.GroupID, "group_name": g.Name, "by": client.UniqueID,
		})
	}
	return nil
}

// notifyGroupEvent sends a group event to the affected user (when online)
// and to every online server admin.
func (s *TCPServer) notifyGroupEvent(uniqueID, eventType string, data any) {
	if s.deps == nil || s.deps.Broadcast == nil {
		return
	}
	payload, err := eventEnvelope(eventType, data)
	if err != nil {
		return
	}
	sent := map[string]bool{}
	if tc, ok := s.clientByUniqueID(uniqueID); ok {
		if err := s.deps.Broadcast.BroadcastToClient(tc.ID, payload); err == nil {
			sent[tc.ID] = true
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.clients {
		if sent[c.ID] || !c.isAdmin() {
			continue
		}
		_ = s.deps.Broadcast.BroadcastToClient(c.ID, payload)
	}
}

// --- permission cache invalidation push (151) --------------------------------

// notifyPermsInvalid pushes MsgPermsInvalid to the named users that are
// currently online. It replaces the client's 5 s permission poll, so it has to
// carry the reason and reach everyone whose resolution moved — not just the
// caller who triggered the write.
func (s *TCPServer) notifyPermsInvalid(reason string, uniqueIDs []string) {
	if len(uniqueIDs) == 0 {
		return
	}
	want := make(map[string]bool, len(uniqueIDs))
	for _, id := range uniqueIDs {
		if id != "" {
			want[id] = true
		}
	}
	// Collect under the registry lock but write outside it: a slow peer must
	// not stall every other handler that needs the client registry.
	s.mu.RLock()
	targets := make([]*Client, 0, len(want))
	for _, c := range s.clients {
		if want[c.uniqueID()] {
			targets = append(targets, c)
		}
	}
	s.mu.RUnlock()
	for _, c := range targets {
		_ = s.writeMessage(c, netproto.MsgPermsInvalid, netproto.PermsInvalid{Reason: reason})
	}
}

// permAudience returns the unique IDs whose resolution a write on this
// tier/target can move (151). Group tiers reach every member, the channel tier
// reaches whoever occupies the channel, client tiers reach the one user.
// Channel-group membership is per channel, so only members standing in the
// channel their membership belongs to are affected.
func (s *TCPServer) permAudience(ctx context.Context, tier store.PermTier, target store.PermTarget) []string {
	if s.deps == nil || s.deps.Groups == nil {
		return nil
	}
	switch tier {
	case store.PermTierServerGroup:
		return s.serverGroupAudience(ctx, target.GroupID)
	case store.PermTierChannelGroup:
		var out []string
		for _, channelID := range s.occupiedChannels() {
			members, err := s.deps.Groups.ListChannelGroupMembers(ctx, target.GroupID, channelID)
			if err != nil {
				continue
			}
			for _, m := range members {
				out = append(out, m.UniqueID)
			}
		}
		return out
	case store.PermTierChannel:
		if s.deps.State == nil {
			return nil
		}
		var out []string
		for _, c := range s.deps.State.ChannelMembers(target.ChannelID) {
			out = append(out, c.UniqueID)
		}
		return out
	case store.PermTierClient, store.PermTierChannelClient:
		uniqueID, err := s.deps.Groups.UserUniqueID(ctx, target.UserID)
		if err != nil {
			return nil
		}
		return []string{uniqueID}
	}
	return nil
}

// serverGroupAudience returns the unique IDs of a server group's members.
func (s *TCPServer) serverGroupAudience(ctx context.Context, groupID int64) []string {
	if s.deps == nil || s.deps.Groups == nil {
		return nil
	}
	members, err := s.deps.Groups.ListServerGroupMembers(ctx, groupID)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(members))
	for _, m := range members {
		out = append(out, m.UniqueID)
	}
	return out
}

// occupiedChannels returns the distinct channels online clients stand in, so
// the channel-group audience is looked up only where it can matter.
func (s *TCPServer) occupiedChannels() []int64 {
	if s.deps == nil || s.deps.State == nil {
		return nil
	}
	seen := map[int64]bool{}
	var out []int64
	for _, c := range s.deps.State.ListClients() {
		if c.ChannelID == 0 || seen[c.ChannelID] {
			continue
		}
		seen[c.ChannelID] = true
		out = append(out, c.ChannelID)
	}
	return out
}

// NotifyGroupExpired informs an online user that a timed membership ended
// (called by the reaper in main).
func (s *TCPServer) NotifyGroupExpired(uniqueID string, groupID int64) {
	s.notifyGroupEvent(uniqueID, eventGroupExpired, map[string]any{"group_id": groupID})
}

// ReapExpiredGroups removes expired timed server-group memberships (145),
// invalidates the permission cache, and notifies affected online users. It
// is invoked periodically (every 60s) by the reaper goroutine in main.
func (s *TCPServer) ReapExpiredGroups(ctx context.Context) {
	if s.deps == nil || s.deps.Groups == nil {
		return
	}
	pairs, err := s.deps.Groups.ExpiredGroupMembers(ctx, time.Now())
	if err != nil {
		s.logger.Warn("reaping expired group members failed", zap.Error(err))
		return
	}
	if len(pairs) == 0 {
		return
	}
	if s.deps.Perms != nil {
		s.deps.Perms.InvalidateAll()
	}
	for _, pair := range pairs {
		userID, groupID := pair[0], pair[1]
		uniqueID, err := s.deps.Groups.UserUniqueID(ctx, userID)
		if err != nil {
			continue
		}
		s.logger.Info("timed group membership expired",
			zap.String("unique_id", uniqueID),
			zap.Int64("group_id", groupID),
		)
		s.NotifyGroupExpired(uniqueID, groupID)
		s.notifyPermsInvalid("group_expired", []string{uniqueID})
	}
}

// --- group icons (177) -------------------------------------------------------

// groupIconDir returns the group-icon storage directory.
func (s *TCPServer) groupIconDir() string {
	return filepath.Join(s.cfg.FileRoot, "group_icons")
}

// handleGroupIconSet stores a server-group icon and marks the group.
func (s *TCPServer) handleGroupIconSet(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.GroupIconSet
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed group_icon_set: "+err.Error())
	}
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.granted(permissions.PermissionKeyServerGroupManage) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyServerGroupManage))
	}
	raw, ext, err := decodeImage(msg.DataBase64)
	if err != nil {
		return s.sendError(client, errCodeMalformed, err.Error())
	}
	dir := s.groupIconDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return s.sendError(client, errCodeUnavailable, "icon storage unavailable")
	}
	fileName := strconv.FormatInt(msg.GroupID, 10) + ext
	if err := os.WriteFile(filepath.Join(dir, fileName), raw, 0o644); err != nil {
		return s.sendError(client, errCodeUnavailable, "icon write failed")
	}
	if s.deps != nil && s.deps.Groups != nil {
		if err := s.deps.Groups.SetGroupIcon(ctx, msg.GroupID, fileName); err != nil {
			s.logger.Warn("marking group icon failed", zap.Int64("group_id", msg.GroupID), zap.Error(err))
		}
	}
	s.audit(ctx, client.UniqueID, "group_icon_set", fmt.Sprintf("server:%d", msg.GroupID), fileName)
	return nil
}

// handleGroupIconGet returns a server-group's icon (wave 6b; mirrors
// handleAvatarGet). Ungated like GroupList: icons are presentation data.
func (s *TCPServer) handleGroupIconGet(_ context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.GroupIconGet
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed group_icon_get: "+err.Error())
	}
	name := strconv.FormatInt(msg.GroupID, 10)
	for ct, ext := range imageExts {
		raw, err := os.ReadFile(filepath.Join(s.groupIconDir(), name+ext))
		if err != nil {
			continue
		}
		return s.writeMessage(client, netproto.MsgGroupIconData, netproto.GroupIconData{
			GroupID:     msg.GroupID,
			DataBase64:  base64.StdEncoding.EncodeToString(raw),
			ContentType: ct,
		})
	}
	return s.writeMessage(client, netproto.MsgGroupIconData, netproto.GroupIconData{GroupID: msg.GroupID})
}

// handleGroupMembers lists a group's members (wave 6b). Ungated like
// GroupList: membership is presentation data in TS3.
func (s *TCPServer) handleGroupMembers(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.GroupMembers
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed group_members: "+err.Error())
	}
	if s.deps == nil || s.deps.Groups == nil {
		return s.sendError(client, errCodeUnavailable, "group store unavailable")
	}
	var members []store.GroupMember
	var err error
	if msg.Type == "channel" {
		if msg.ChannelID == 0 {
			return s.sendError(client, errCodeMalformed, "channel_id is required for channel groups")
		}
		members, err = s.deps.Groups.ListChannelGroupMembers(ctx, msg.GroupID, msg.ChannelID)
	} else {
		members, err = s.deps.Groups.ListServerGroupMembers(ctx, msg.GroupID)
	}
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "member list failed")
	}
	resp := netproto.GroupMembersResponse{Type: groupTypeOf(msg.Type), GroupID: msg.GroupID, Members: []netproto.GroupMemberEntry{}}
	for _, m := range members {
		e := netproto.GroupMemberEntry{UniqueID: m.UniqueID, Nickname: m.Nickname}
		if m.ExpiresAt != nil {
			e.ExpiresAt = m.ExpiresAt.Unix()
		}
		resp.Members = append(resp.Members, e)
	}
	return s.writeMessage(client, netproto.MsgGroupMembersResponse, resp)
}

// --- permission write path ---------------------------------------------------

// permTarget resolves the message addressing to a store target + tier.
func (s *TCPServer) permTarget(ctx context.Context, tier, uniqueID string, groupID, channelID int64) (store.PermTier, store.PermTarget, error) {
	var target store.PermTarget
	switch store.PermTier(tier) {
	case store.PermTierServerGroup, store.PermTierChannelGroup:
		if groupID == 0 {
			return "", target, errors.New("group_id is required for group tiers")
		}
		target.GroupID = groupID
	case store.PermTierClient:
		if uniqueID == "" {
			return "", target, errors.New("unique_id is required for the client tier")
		}
		user, err := s.deps.Auth.LookupUser(ctx, uniqueID)
		if err != nil {
			return "", target, errors.New("target user not found")
		}
		target.UserID = user.ID
	case store.PermTierChannelClient:
		if uniqueID == "" || channelID == 0 {
			return "", target, errors.New("unique_id and channel_id are required for the channel_client tier")
		}
		user, err := s.deps.Auth.LookupUser(ctx, uniqueID)
		if err != nil {
			return "", target, errors.New("target user not found")
		}
		target.UserID = user.ID
		target.ChannelID = channelID
	case store.PermTierChannel:
		if channelID == 0 {
			return "", target, errors.New("channel_id is required for the channel tier")
		}
		target.ChannelID = channelID
	default:
		return "", target, fmt.Errorf("unknown tier %q (want server_group|client|channel_client|channel|channel_group)", tier)
	}
	return store.PermTier(tier), target, nil
}

// grantCapOk enforces the TS3-lite grant semantics: a non-admin caller may
// only set value/grant <= their own grant for that key. Admins and holders
// of a sufficient grant pass.
func (s *TCPServer) grantCapOk(pc *permChecker, key string, value, grant int) bool {
	if pc.admin || pc.granted(permissions.PermissionKeyPermissionModifyPowerIgnore) {
		return true
	}
	own, tier, err := pc.resolver.Resolve(pc.tp, permissions.PermissionKey(key))
	if err != nil || own == nil {
		return false // no own grant for this key
	}
	_ = tier
	return own.Grant >= value && own.Grant >= grant
}

// currentPerms indexes a target's existing entries by key so a write can be
// capped against what it replaces or removes. Admins skip the lookup: the cap
// never applies to them.
func (s *TCPServer) currentPerms(ctx context.Context, pc *permChecker, tier store.PermTier, target store.PermTarget) (map[string]store.PermEntry, error) {
	if pc.admin {
		return nil, nil
	}
	entries, err := s.deps.Groups.ListPermissions(ctx, tier, target)
	if err != nil {
		return nil, err
	}
	out := make(map[string]store.PermEntry, len(entries))
	for _, e := range entries {
		out[e.Key] = e
	}
	return out, nil
}

// grantCapOkWrite caps a write against both what it writes and the entry it
// replaces or removes. The second half matters: capping only the new
// value/grant lets a caller holding a grant of 0 wipe or overwrite an entry
// (e.g. the admin group's b_permission_manage) it could never set itself.
func (s *TCPServer) grantCapOkWrite(pc *permChecker, current map[string]store.PermEntry, key string, value, grant int) bool {
	if !s.grantCapOk(pc, key, value, grant) {
		return false
	}
	if e, ok := current[key]; ok {
		return s.grantCapOk(pc, key, e.Value, e.Grant)
	}
	return true
}

// handlePermSet writes a permission entry on a tier.
func (s *TCPServer) handlePermSet(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.PermSet
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed perm_set: "+err.Error())
	}
	if s.deps == nil || s.deps.Groups == nil {
		return s.sendError(client, errCodeUnavailable, "group store unavailable")
	}
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.granted(permissions.PermissionKeyPermissionManage) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyPermissionManage))
	}
	tier, target, err := s.permTarget(ctx, msg.Tier, msg.UniqueID, msg.GroupID, msg.ChannelID)
	if err != nil {
		return s.sendError(client, errCodeMalformed, err.Error())
	}
	s.permWriteMu.Lock()
	current, err := s.currentPerms(ctx, pc, tier, target)
	if err != nil {
		s.permWriteMu.Unlock()
		return s.sendError(client, errCodeUnavailable, "perm lookup failed")
	}
	if !s.grantCapOkWrite(pc, current, msg.Key, msg.Value, msg.Grant) {
		s.permWriteMu.Unlock()
		return s.sendError(client, errCodePermissionDenied, "grant cap exceeded: you may only set values <= your own grant for this key")
	}
	if err := s.deps.Groups.SetPermission(ctx, tier, target, msg.Key, msg.Value, msg.Grant, msg.Skip, msg.Negate); err != nil {
		s.permWriteMu.Unlock()
		return s.sendError(client, errCodeUnavailable, "perm set failed: "+err.Error())
	}
	s.permWriteMu.Unlock()
	s.audit(ctx, client.UniqueID, "perm_set", fmt.Sprintf("%s/%s", msg.Tier, msg.Key),
		fmt.Sprintf("value=%d grant=%d skip=%t negate=%t target=%+v", msg.Value, msg.Grant, msg.Skip, msg.Negate, target))
	if s.deps.Perms != nil {
		s.deps.Perms.InvalidateAll()
	}
	s.notifyPermsInvalid("perm_set", s.permAudience(ctx, tier, target))
	return nil
}

// handlePermList returns a target's current permission entries (the wave-6b
// editor's read path). Gated by b_permission_manage like the write path.
func (s *TCPServer) handlePermList(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.PermList
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed perm_list: "+err.Error())
	}
	if s.deps == nil || s.deps.Groups == nil {
		return s.sendError(client, errCodeUnavailable, "group store unavailable")
	}
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.granted(permissions.PermissionKeyPermissionManage) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyPermissionManage))
	}
	tier, target, err := s.permTarget(ctx, msg.Tier, msg.UniqueID, msg.GroupID, msg.ChannelID)
	if err != nil {
		return s.sendError(client, errCodeMalformed, err.Error())
	}
	entries, err := s.deps.Groups.ListPermissions(ctx, tier, target)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "perm list failed")
	}
	resp := netproto.PermListResponse{Tier: string(tier), Entries: []netproto.PermissionEntry{}}
	for _, e := range entries {
		resp.Entries = append(resp.Entries, netproto.PermissionEntry{
			Key: e.Key, Value: e.Value, Grant: e.Grant, Skip: e.Skip, Negate: e.Negate,
		})
	}
	return s.writeMessage(client, netproto.MsgPermListResponse, resp)
}

// handlePermUnset removes a permission entry.
func (s *TCPServer) handlePermUnset(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.PermUnset
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed perm_unset: "+err.Error())
	}
	if s.deps == nil || s.deps.Groups == nil {
		return s.sendError(client, errCodeUnavailable, "group store unavailable")
	}
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.granted(permissions.PermissionKeyPermissionManage) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyPermissionManage))
	}
	tier, target, err := s.permTarget(ctx, msg.Tier, msg.UniqueID, msg.GroupID, msg.ChannelID)
	if err != nil {
		return s.sendError(client, errCodeMalformed, err.Error())
	}
	// Removing an entry is a write like setting it, and the cap has to be
	// measured against the entry being deleted: otherwise a low-power operator
	// could delete entries far above their own level.
	s.permWriteMu.Lock()
	current, err := s.currentPerms(ctx, pc, tier, target)
	if err != nil {
		s.permWriteMu.Unlock()
		return s.sendError(client, errCodeUnavailable, "perm lookup failed")
	}
	if !s.grantCapOkWrite(pc, current, msg.Key, 0, 0) {
		s.permWriteMu.Unlock()
		return s.sendError(client, errCodePermissionDenied, "grant cap exceeded: you may only unset entries within your own grant for this key")
	}
	if err := s.deps.Groups.UnsetPermission(ctx, tier, target, msg.Key); err != nil {
		s.permWriteMu.Unlock()
		return s.sendError(client, errCodeUnavailable, "perm unset failed: "+err.Error())
	}
	s.permWriteMu.Unlock()
	s.audit(ctx, client.UniqueID, "perm_unset", fmt.Sprintf("%s/%s", msg.Tier, msg.Key), fmt.Sprintf("target=%+v", target))
	if s.deps.Perms != nil {
		s.deps.Perms.InvalidateAll()
	}
	s.notifyPermsInvalid("perm_unset", s.permAudience(ctx, tier, target))
	return nil
}

// --- permission copy (141) ----------------------------------------------------

// permCopyTarget resolves one side of a copy to a store tier and target.
// A client kind with a channel scopes to the channel_client tier, which is the
// only way a per-channel client copy can be addressed.
func (s *TCPServer) permCopyTarget(ctx context.Context, kind, id string, channelID int64) (store.PermTier, store.PermTarget, error) {
	var target store.PermTarget
	switch kind {
	case "servergroup", "channelgroup":
		groupID, err := strconv.ParseInt(id, 10, 64)
		if err != nil || groupID == 0 {
			return "", target, fmt.Errorf("%s id must be a group id", kind)
		}
		target.GroupID = groupID
		if kind == "channelgroup" {
			return store.PermTierChannelGroup, target, nil
		}
		return store.PermTierServerGroup, target, nil
	case "client":
		if id == "" {
			return "", target, errors.New("client id must be a unique id")
		}
		if s.deps == nil || s.deps.Auth == nil {
			return "", target, errors.New("auth backend unavailable")
		}
		user, err := s.deps.Auth.LookupUser(ctx, id)
		if err != nil {
			return "", target, fmt.Errorf("user %q not found", id)
		}
		target.UserID = user.ID
		if channelID != 0 {
			target.ChannelID = channelID
			return store.PermTierChannelClient, target, nil
		}
		return store.PermTierClient, target, nil
	default:
		return "", target, fmt.Errorf("unknown kind %q (want servergroup|channelgroup|client)", kind)
	}
}

// handlePermCopy copies a target's permission entries onto another target
// (141). Every entry is capped like a direct write, in both directions: a
// caller may not deposit an entry it could not have set, and with Replace it
// may not clear one it could not have unset.
func (s *TCPServer) handlePermCopy(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.PermCopy
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed perm_copy: "+err.Error())
	}
	if s.deps == nil || s.deps.Groups == nil {
		return s.sendError(client, errCodeUnavailable, "group store unavailable")
	}
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.granted(permissions.PermissionKeyPermissionManage) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyPermissionManage))
	}
	fromTier, fromTarget, err := s.permCopyTarget(ctx, msg.FromKind, msg.FromID, msg.ChannelID)
	if err != nil {
		return s.sendError(client, errCodeMalformed, "source: "+err.Error())
	}
	toTier, toTarget, err := s.permCopyTarget(ctx, msg.ToKind, msg.ToID, msg.ChannelID)
	if err != nil {
		return s.sendError(client, errCodeMalformed, "destination: "+err.Error())
	}
	if fromTier == toTier && fromTarget == toTarget {
		return s.sendError(client, errCodeMalformed, "source and destination are the same target")
	}

	s.permWriteMu.Lock()
	errCode, errMessage, src := s.doPermCopyLocked(ctx, pc, fromTier, fromTarget, toTier, toTarget, msg.Replace)
	s.permWriteMu.Unlock()
	if errMessage != "" {
		return s.sendError(client, errCode, errMessage)
	}

	s.audit(ctx, client.UniqueID, "perm_copy",
		fmt.Sprintf("%s:%s->%s:%s", msg.FromKind, msg.FromID, msg.ToKind, msg.ToID),
		fmt.Sprintf("entries=%d replace=%t channel_id=%d", len(src), msg.Replace, msg.ChannelID))
	if s.deps.Perms != nil {
		s.deps.Perms.InvalidateAll()
	}
	s.notifyPermsInvalid("perm_copy", s.permAudience(ctx, toTier, toTarget))
	return nil
}

// doPermCopyLocked performs only the permission-store work and returns an
// error response for the caller to send after releasing permWriteMu.
func (s *TCPServer) doPermCopyLocked(ctx context.Context, pc *permChecker, fromTier store.PermTier, fromTarget store.PermTarget, toTier store.PermTier, toTarget store.PermTarget, replace bool) (uint16, string, []store.PermEntry) {
	src, err := s.deps.Groups.ListPermissions(ctx, fromTier, fromTarget)
	if err != nil {
		return errCodeUnavailable, "perm lookup failed", nil
	}
	dstEntries, err := s.deps.Groups.ListPermissions(ctx, toTier, toTarget)
	if err != nil {
		return errCodeUnavailable, "perm lookup failed", nil
	}
	current := make(map[string]store.PermEntry, len(dstEntries))
	for _, e := range dstEntries {
		current[e.Key] = e
	}
	copied := make(map[string]bool, len(src))
	for _, e := range src {
		copied[e.Key] = true
		if !s.grantCapOkWrite(pc, current, e.Key, e.Value, e.Grant) {
			return errCodePermissionDenied, "grant cap exceeded: " + e.Key + " is outside your own grant", nil
		}
	}
	var remove []string
	if replace {
		for key, e := range current {
			if copied[key] {
				continue
			}
			if !s.grantCapOk(pc, key, e.Value, e.Grant) {
				return errCodePermissionDenied, "grant cap exceeded: replacing would clear " + key + ", which is outside your own grant", nil
			}
			remove = append(remove, key)
		}
	}
	if err := s.deps.Groups.CopyPermissions(ctx, toTier, toTarget, remove, src); err != nil {
		return errCodeUnavailable, "perm copy failed: " + err.Error(), nil
	}
	return 0, "", src
}

// --- permission templates (142) ----------------------------------------------

// permTemplateEntry is one key in a bundle. Grant matters as much as Value:
// an entry written with grant 0 can never be delegated further, so a bundle
// that hands out b_permission_manage without a grant produces an operator who
// can open the editor and write nothing (142).
type permTemplateEntry struct {
	Value  int
	Grant  int
	Negate bool
}

// permTemplates are the four built-in permission bundles (fixed in code).
//
// Only keys that are denied-on-unset need to appear: the open defaults (talk,
// whisper, file transfer, channel join) are already allowed for everyone, and
// an entry would only narrow them. Powers rise with the tier so the needed_*
// counterparts an operator sets on a target keep working.
var permTemplates = map[string]map[string]permTemplateEntry{
	// guest: nothing. Every privileged action is denied-on-unset already, and
	// everything else is open, which is exactly the guest experience.
	"guest": {
		"i_ft_file_upload_power": {Negate: true},
		"b_client_avatar_upload": {Negate: true},
	},
	// member: the two things a registered user is expected to do that the
	// deny-on-unset default otherwise blocks.
	"member": {
		"b_client_poke":              {Value: 1},
		"b_channel_create_temporary": {Value: 1},
	},
	"moderator": {
		"b_client_poke":              {Value: 1},
		"b_channel_create_temporary": {Value: 1},
		"b_chat_delete_any":          {Value: 1},
		"b_chat_mention_all":         {Value: 1},
		"b_chat_slowmode_bypass":     {Value: 1},
		// moderating chat means owning the word/link lists too, or 117/118
		// stay admin-only in practice.
		"b_chat_filter_manage":      {Value: 1},
		"b_channel_modify":          {Value: 1},
		"b_channel_delete":          {Value: 1},
		"b_client_priority_speaker": {Value: 1},
		"b_ft_delete":               {Value: 1},
		// enforcement powers: without these a moderator can see the offence
		// and do nothing about it.
		"b_client_ban":                     {Value: 1},
		"i_client_ban_power":               {Value: 50},
		"i_client_kick_from_channel_power": {Value: 50},
		"i_client_kick_from_server_power":  {Value: 50},
		"i_client_move_power":              {Value: 50},
	},
	// admin: everything a moderator holds, at a power that outranks it, plus
	// the management keys — each with a grant so an admin can actually
	// delegate the bundle instead of only holding it.
	"admin": {
		"b_client_poke":                    {Value: 1, Grant: 1},
		"b_channel_create_temporary":       {Value: 1, Grant: 1},
		"b_channel_create_semi_permanent":  {Value: 1, Grant: 1},
		"b_channel_create_permanent":       {Value: 1, Grant: 1},
		"b_chat_delete_any":                {Value: 1, Grant: 1},
		"b_chat_mention_all":               {Value: 1, Grant: 1},
		"b_chat_slowmode_bypass":           {Value: 1, Grant: 1},
		"b_chat_filter_manage":             {Value: 1, Grant: 1},
		"b_channel_modify":                 {Value: 1, Grant: 1},
		"b_channel_delete":                 {Value: 1, Grant: 1},
		"b_client_priority_speaker":        {Value: 1, Grant: 1},
		"b_ft_delete":                      {Value: 1, Grant: 1},
		"b_client_ban":                     {Value: 1, Grant: 1},
		"i_client_ban_power":               {Value: 75, Grant: 75},
		"i_client_kick_from_channel_power": {Value: 75, Grant: 75},
		"i_client_kick_from_server_power":  {Value: 75, Grant: 75},
		"i_client_move_power":              {Value: 75, Grant: 75},
		"b_client_remoteaddress_view":      {Value: 1, Grant: 1},
		"b_virtualserver_recording":        {Value: 1, Grant: 1},
		"b_emoji_manage":                   {Value: 1, Grant: 1},
		"b_permission_manage":              {Value: 1, Grant: 1},
		"b_permission_modify_power_ignore": {Value: 1, Grant: 1},
		"b_server_group_manage":            {Value: 1, Grant: 1},
		"b_channel_group_manage":           {Value: 1, Grant: 1},
		"b_audit_view":                     {Value: 1, Grant: 1},
	},
}

// handlePermTemplateApply applies a built-in template to a group or user.
func (s *TCPServer) handlePermTemplateApply(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.PermTemplateApply
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed perm_template_apply: "+err.Error())
	}
	tmpl, ok := permTemplates[msg.Template]
	if !ok {
		return s.sendError(client, errCodeMalformed, "unknown template (want guest|member|moderator|admin)")
	}
	if s.deps == nil || s.deps.Groups == nil {
		return s.sendError(client, errCodeUnavailable, "group store unavailable")
	}
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.granted(permissions.PermissionKeyPermissionManage) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyPermissionManage))
	}
	tier, target, err := s.permTarget(ctx, msg.Tier, msg.UniqueID, msg.GroupID, 0)
	if err != nil {
		return s.sendError(client, errCodeMalformed, err.Error())
	}
	if tier != store.PermTierServerGroup && tier != store.PermTierClient {
		return s.sendError(client, errCodeMalformed, "templates apply to server_group or client tiers")
	}
	// A template is a batch of perm_set writes: cap every key it carries
	// against the value it writes and the entry it would replace, or the admin
	// template becomes a privilege-escalation shortcut. The lock spans the
	// check and the writes for the same reason perm_set holds it.
	s.permWriteMu.Lock()
	defer s.permWriteMu.Unlock()
	current, err := s.currentPerms(ctx, pc, tier, target)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "perm lookup failed")
	}
	for key, e := range tmpl {
		if !s.grantCapOkWrite(pc, current, key, e.Value, e.Grant) {
			return s.sendError(client, errCodePermissionDenied, "grant cap exceeded: the template's "+key+" is outside your own grant")
		}
	}
	for key, e := range tmpl {
		if err := s.deps.Groups.SetPermission(ctx, tier, target, key, e.Value, e.Grant, false, e.Negate); err != nil {
			return s.sendError(client, errCodeUnavailable, "template apply failed: "+err.Error())
		}
	}
	s.audit(ctx, client.UniqueID, "perm_template_apply", msg.Template, fmt.Sprintf("tier=%s target=%+v", msg.Tier, target))
	if s.deps.Perms != nil {
		s.deps.Perms.InvalidateAll()
	}
	s.notifyPermsInvalid("perm_template_apply", s.permAudience(ctx, tier, target))
	return nil
}

// --- permission trace (155) --------------------------------------------------

// handlePermTrace returns the resolver's winning tier and all tier entries
// for a key, for a user in an optional channel context.
func (s *TCPServer) handlePermTrace(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.PermTrace
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed perm_trace: "+err.Error())
	}
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	// Trace is a permission-management view: gate it like editing.
	if !pc.granted(permissions.PermissionKeyPermissionManage) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyPermissionManage))
	}
	if s.deps == nil || s.deps.Auth == nil {
		return s.sendError(client, errCodeUnavailable, "auth backend unavailable")
	}
	user, err := s.deps.Auth.LookupUser(ctx, msg.UniqueID)
	if err != nil {
		return s.sendError(client, errCodeNotFound, "target user not found")
	}
	tp, err := s.deps.Perms.LoadForClient(ctx, user.ID, msg.ChannelID)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "loading permissions failed")
	}

	key := permissions.PermissionKey(msg.Key)
	resp := netproto.PermTraceResponse{Key: msg.Key, Entries: []netproto.PermTraceEntry{}}
	winner, winTier, err := s.deps.Resolver.Resolve(tp, key)
	if err == nil && winner != nil {
		resp.Effective = winner.Value
		resp.EffectiveTier = winTier.String()
	} else {
		resp.Effective = 0
		resp.EffectiveTier = ""
	}

	for _, tier := range []permissions.Tier{
		permissions.TierServerGroup,
		permissions.TierClientSpecific,
		permissions.TierChannelSpecific,
		permissions.TierChannel,
		permissions.TierChannelGroup,
		permissions.TierChannelClient,
	} {
		entry := netproto.PermTraceEntry{Tier: tier.String(), Winning: tier == winTier && winner != nil}
		if set, ok := tp.Get(tier); ok {
			if p, ok := set.Get(key); ok {
				entry.Present = true
				entry.Value = p.Value
				entry.Grant = p.Grant
				entry.Skip = p.Skip
				entry.Negate = p.Negate
			}
		}
		resp.Entries = append(resp.Entries, entry)
	}
	return s.writeMessage(client, netproto.MsgPermTraceResponse, resp)
}

// --- audit log (149) ---------------------------------------------------------

// handleAuditLog returns a paged audit log. Gated by b_audit_view (or admin).
func (s *TCPServer) handleAuditLog(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.AuditLog
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed audit_log: "+err.Error())
	}
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.granted(permissions.PermissionKeyAuditView) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyAuditView))
	}
	if s.deps == nil || s.deps.Groups == nil {
		return s.sendError(client, errCodeUnavailable, "audit store unavailable")
	}
	entries, err := s.deps.Groups.AuditList(ctx, msg.BeforeID, msg.Limit)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "audit query failed")
	}
	resp := netproto.AuditLogResponse{Entries: []netproto.AuditEntry{}}
	for _, e := range entries {
		resp.Entries = append(resp.Entries, netproto.AuditEntry{
			ID: e.ID, Actor: e.Actor, Action: e.Action, Target: e.Target,
			Detail: e.Detail, CreatedAt: e.CreatedAt.Unix(),
		})
	}
	return s.writeMessage(client, netproto.MsgAuditLogResponse, resp)
}

// --- default groups (143/144) ------------------------------------------------

// assignDefaultGroup assigns the Member default group to a registered user
// with no server-group memberships (first login).
func (s *TCPServer) assignDefaultGroup(ctx context.Context, client *Client) {
	if s.deps == nil || s.deps.Groups == nil || client.UserID == 0 || s.deps.DefaultMemberGroupID == 0 {
		return
	}
	ids, err := s.deps.Groups.UserGroupIDs(ctx, client.UserID)
	if err != nil || len(ids) > 0 {
		return
	}
	if err := s.deps.Groups.AssignServerGroup(ctx, s.deps.DefaultMemberGroupID, client.UserID, 0); err != nil {
		s.logger.Warn("default group assignment failed",
			zap.String("client_id", client.ID),
			zap.Error(err),
		)
		return
	}
	s.logger.Info("assigned default Member group",
		zap.String("client_id", client.ID),
		zap.String("unique_id", client.UniqueID),
	)
}

// --- bot accounts (180) -------------------------------------------------------

// ClientIsBot reports whether a connected client's account carries
// b_client_is_bot. It resolves through the tier hierarchy like any other
// permission but deliberately does not honour the admin bypass permChecker
// applies: bot-ness is a property of the account, not a privilege, and every
// server admin would otherwise wear a bot badge. Guests can never be bots —
// the flag is granted through a users row's groups.
func (s *TCPServer) ClientIsBot(ctx context.Context, client *Client) bool {
	if client == nil || client.UserID == 0 {
		return false
	}
	if s.deps == nil || s.deps.Perms == nil || s.deps.Resolver == nil {
		return false
	}
	tp, err := s.deps.Perms.LoadForClient(ctx, client.UserID, 0)
	if err != nil {
		return false
	}
	perm, _, err := s.deps.Resolver.Resolve(tp, permissions.PermissionKeyClientIsBot)
	if err != nil || perm == nil {
		return false
	}
	return !perm.Negate && perm.Value != 0
}

// guestGroupSet returns the Guest default group's permission set for
// guests (virtual membership — guests have no users row).
func (s *TCPServer) guestGroupSet(ctx context.Context) (permissions.PermissionSet, bool) {
	if s.deps == nil || s.deps.Perms == nil || s.deps.DefaultGuestGroupID == 0 {
		return nil, false
	}
	set, err := s.deps.Perms.LoadGroupPermissions(ctx, s.deps.DefaultGuestGroupID)
	if err != nil {
		return nil, false
	}
	return set, true
}

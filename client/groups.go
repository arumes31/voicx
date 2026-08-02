// groups.go defines the Wails-bound API for wave-6b permission/group
// management: group CRUD and membership, the permission write path,
// templates, traces, the audit log, and ban administration. Read operations
// use the serialized request/response pattern (connManager.request); writes
// are fire-and-forget — failures arrive as MsgError and surface via the
// "servererror" event, matching the existing channel-edit UX.
package main

import (
	"strings"
	"time"

	"voicx/internal/netproto"
)

// IsAdmin reports whether the authenticated user is a server admin (used to
// show/hide admin-only UI).
func (a *App) IsAdmin() bool {
	return a.cmLoad() != nil && a.cmLoad().isAdminSnapshot()
}

// --- group list / CRUD -------------------------------------------------------

// GroupList returns the groups of a type ("server" or "channel").
func (a *App) GroupList(groupType string) (netproto.GroupListResponse, error) {
	f, err := a.cmLoad().request(netproto.MsgGroupList, netproto.MsgGroupListResponse,
		netproto.GroupList{Type: groupType}, 5*time.Second)
	if err != nil {
		return netproto.GroupListResponse{}, err
	}
	var resp netproto.GroupListResponse
	if err := decodeJSON(f, &resp); err != nil {
		return netproto.GroupListResponse{}, err
	}
	return resp, nil
}

// GroupCreate creates a group and returns the refreshed group list.
func (a *App) GroupCreate(groupType, name string, sortID int) (netproto.GroupListResponse, error) {
	f, err := a.cmLoad().request(netproto.MsgGroupCreate, netproto.MsgGroupListResponse,
		netproto.GroupCreate{Type: groupType, Name: name, SortID: sortID}, 5*time.Second)
	if err != nil {
		return netproto.GroupListResponse{}, err
	}
	var resp netproto.GroupListResponse
	if err := decodeJSON(f, &resp); err != nil {
		return netproto.GroupListResponse{}, err
	}
	return resp, nil
}

// GroupEdit writes a server group's cosmetics and returns the refreshed group
// list (178 colour, 179 hoisting). The dialog always submits the full form, so
// all three fields are sent explicitly; an empty colour clears back to the
// theme default. The server rejects anything but "#rrggbb" or "".
func (a *App) GroupEdit(groupID int64, color string, hoist bool, sortID int) (netproto.GroupListResponse, error) {
	f, err := a.cmLoad().request(netproto.MsgGroupEdit, netproto.MsgGroupListResponse,
		netproto.GroupEdit{GroupID: groupID, Color: &color, Hoist: &hoist, SortID: &sortID}, 5*time.Second)
	if err != nil {
		return netproto.GroupListResponse{}, err
	}
	var resp netproto.GroupListResponse
	if err := decodeJSON(f, &resp); err != nil {
		return netproto.GroupListResponse{}, err
	}
	return resp, nil
}

// GroupRename renames a group. Errors surface via the servererror event.
func (a *App) GroupRename(groupType string, groupID int64, name string) string {
	if err := a.cmLoad().write(netproto.MsgGroupRename, netproto.GroupRename{
		Type: groupType, GroupID: groupID, Name: name,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// GroupDelete deletes a group (force required when it has members).
func (a *App) GroupDelete(groupType string, groupID int64, force bool) string {
	if err := a.cmLoad().write(netproto.MsgGroupDelete, netproto.GroupDelete{
		Type: groupType, GroupID: groupID, Force: force,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// GroupAssign assigns a user to a group. expiresInSeconds > 0 makes the
// membership timed (145); channelID is required for channel groups.
func (a *App) GroupAssign(groupType string, groupID int64, uniqueID string, channelID int64, expiresInSeconds int64) string {
	if err := a.cmLoad().write(netproto.MsgGroupAssign, netproto.GroupAssign{
		Type: groupType, GroupID: groupID, UniqueID: uniqueID,
		ChannelID: channelID, ExpiresInSeconds: expiresInSeconds,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// GroupUnassign removes a user from a group.
func (a *App) GroupUnassign(groupType string, groupID int64, uniqueID string, channelID int64) string {
	if err := a.cmLoad().write(netproto.MsgGroupUnassign, netproto.GroupUnassign{
		Type: groupType, GroupID: groupID, UniqueID: uniqueID, ChannelID: channelID,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// GroupMembers returns a group's member list.
func (a *App) GroupMembers(groupType string, groupID, channelID int64) (netproto.GroupMembersResponse, error) {
	f, err := a.cmLoad().request(netproto.MsgGroupMembers, netproto.MsgGroupMembersResponse,
		netproto.GroupMembers{Type: groupType, GroupID: groupID, ChannelID: channelID}, 5*time.Second)
	if err != nil {
		return netproto.GroupMembersResponse{}, err
	}
	var resp netproto.GroupMembersResponse
	if err := decodeJSON(f, &resp); err != nil {
		return netproto.GroupMembersResponse{}, err
	}
	return resp, nil
}

// --- group icons -------------------------------------------------------------

// GroupIconSet uploads a server-group icon (base64 image, same validation as
// avatars).
func (a *App) GroupIconSet(groupID int64, dataBase64 string) string {
	if err := a.cmLoad().write(netproto.MsgGroupIconSet, netproto.GroupIconSet{
		GroupID: groupID, DataBase64: dataBase64,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// GroupIconGet fetches a server-group icon (empty DataBase64 = no icon).
func (a *App) GroupIconGet(groupID int64) (netproto.GroupIconData, error) {
	f, err := a.cmLoad().request(netproto.MsgGroupIconGet, netproto.MsgGroupIconData,
		netproto.GroupIconGet{GroupID: groupID}, 5*time.Second)
	if err != nil {
		return netproto.GroupIconData{}, err
	}
	var data netproto.GroupIconData
	if err := decodeJSON(f, &data); err != nil {
		return netproto.GroupIconData{}, err
	}
	return data, nil
}

// --- permission write path ---------------------------------------------------

// PermSet writes a permission entry on a tier. Errors surface via the
// servererror event (permission denied / grant cap exceeded).
func (a *App) PermSet(tier string, groupID int64, uniqueID string, channelID int64, key string, value, grant int, skip, negate bool) string {
	if err := a.cmLoad().write(netproto.MsgPermSet, netproto.PermSet{
		Tier: tier, GroupID: groupID, UniqueID: uniqueID, ChannelID: channelID,
		Key: key, Value: value, Grant: grant, Skip: skip, Negate: negate,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// PermUnset removes a permission entry (same addressing as PermSet).
func (a *App) PermUnset(tier string, groupID int64, uniqueID string, channelID int64, key string) string {
	if err := a.cmLoad().write(netproto.MsgPermUnset, netproto.PermUnset{
		Tier: tier, GroupID: groupID, UniqueID: uniqueID, ChannelID: channelID, Key: key,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// PermTemplateApply applies a built-in template (guest|member|moderator|
// admin) to a server group or client.
func (a *App) PermTemplateApply(template, tier string, groupID int64, uniqueID string) string {
	if err := a.cmLoad().write(netproto.MsgPermTemplateApply, netproto.PermTemplateApply{
		Template: template, Tier: tier, GroupID: groupID, UniqueID: uniqueID,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// PermCopy copies a target's permission entries onto another target (141).
// Kinds are "servergroup" | "channelgroup" | "client"; ids are decimal group
// ids or a unique ID for "client". replace makes the result an exact copy
// rather than a merge. The server caps the copy in both directions, so a
// denial is expected UX and arrives via the servererror event.
func (a *App) PermCopy(fromKind, fromID, toKind, toID string, channelID int64, replace bool) string {
	if err := a.cmLoad().write(netproto.MsgPermCopy, netproto.PermCopy{
		FromKind: fromKind, FromID: fromID, ToKind: toKind, ToID: toID,
		ChannelID: channelID, Replace: replace,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// PermList returns a target's current permission entries (editor read path).
func (a *App) PermList(tier string, groupID int64, uniqueID string, channelID int64) (netproto.PermListResponse, error) {
	f, err := a.cmLoad().request(netproto.MsgPermList, netproto.MsgPermListResponse,
		netproto.PermList{Tier: tier, GroupID: groupID, UniqueID: uniqueID, ChannelID: channelID}, 5*time.Second)
	if err != nil {
		return netproto.PermListResponse{}, err
	}
	var resp netproto.PermListResponse
	if err := decodeJSON(f, &resp); err != nil {
		return netproto.PermListResponse{}, err
	}
	return resp, nil
}

// PermTrace returns the winning-tier trace of a permission for a user.
func (a *App) PermTrace(uniqueID, key string, channelID int64) (netproto.PermTraceResponse, error) {
	f, err := a.cmLoad().request(netproto.MsgPermTrace, netproto.MsgPermTraceResponse,
		netproto.PermTrace{UniqueID: uniqueID, Key: key, ChannelID: channelID}, 5*time.Second)
	if err != nil {
		return netproto.PermTraceResponse{}, err
	}
	var resp netproto.PermTraceResponse
	if err := decodeJSON(f, &resp); err != nil {
		return netproto.PermTraceResponse{}, err
	}
	return resp, nil
}

// --- audit log ---------------------------------------------------------------

// AuditLog returns a page of the audit log (beforeID 0 = latest page).
func (a *App) AuditLog(beforeID int64, limit int) (netproto.AuditLogResponse, error) {
	f, err := a.cmLoad().request(netproto.MsgAuditLog, netproto.MsgAuditLogResponse,
		netproto.AuditLog{BeforeID: beforeID, Limit: limit}, 5*time.Second)
	if err != nil {
		return netproto.AuditLogResponse{}, err
	}
	var resp netproto.AuditLogResponse
	if err := decodeJSON(f, &resp); err != nil {
		return netproto.AuditLogResponse{}, err
	}
	return resp, nil
}

// --- bans --------------------------------------------------------------------

// BanList returns the ban list (gated server-side by ban power / admin).
func (a *App) BanList() (netproto.BanListResponse, error) {
	f, err := a.cmLoad().request(netproto.MsgBanList, netproto.MsgBanListResponse,
		netproto.BanList{}, 5*time.Second)
	if err != nil {
		return netproto.BanListResponse{}, err
	}
	var resp netproto.BanListResponse
	if err := decodeJSON(f, &resp); err != nil {
		return netproto.BanListResponse{}, err
	}
	return resp, nil
}

// BanRemove lifts one ban by ID.
func (a *App) BanRemove(banID int64) string {
	if err := a.cmLoad().write(netproto.MsgBanRemove, netproto.BanRemove{BanID: banID}); err != nil {
		return err.Error()
	}
	return ""
}

// KickClient kicks a client from its channel or the server (ban = record a
// ban and kick from the server; durationSeconds > 0 = temporary ban). Gated
// server-side by kick/ban powers.
func (a *App) KickClient(clientID string, fromServer, ban bool, reason string, durationSeconds int64) string {
	if err := a.cmLoad().write(netproto.MsgKickClient, netproto.KickClient{
		ClientID: clientID, FromServer: fromServer, Ban: ban, Reason: reason,
		DurationSeconds: durationSeconds,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// --- channels ----------------------------------------------------------------

// CreateChannel creates a channel (164 full create dialog). It returns "" on
// success or the failure reason; permission denials surface via servererror.
func (a *App) CreateChannel(name, topic string, parentID int64, channelType, maxClients int, password string, neededJoinPower, opusBitrate int, opusFEC, opusDTX, opusStereo bool) string {
	if err := a.cmLoad().write(netproto.MsgCreateChannel, netproto.CreateChannel{
		Name: name, Topic: topic, ParentID: parentID, Type: channelType,
		MaxClients: maxClients, Password: password, NeededJoinPower: neededJoinPower,
		OpusBitrate: opusBitrate, OpusFEC: &opusFEC, OpusDTX: &opusDTX, OpusStereo: &opusStereo,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// ChannelEditTree writes the four channel fields the edit dialog gained:
// needed join power (160), sort index (163), parent (168) and the sub-channel
// permission inheritance toggle (157). fields is a comma-separated subset of
// "join_power,order,parent,inherit" naming which arguments to honour —
// omitting a field leaves it unchanged, which matters because a parent_id of
// 0 is the valid "move to root" value and re-parenting drops the server's
// whole permission cache. Errors (invalid re-parent, join power above the
// caller's own) surface via the servererror event.
func (a *App) ChannelEditTree(channelID int64, fields string, neededJoinPower, orderIndex int, parentID int64, inheritPermissions bool) string {
	msg := netproto.ChannelEdit{ChannelID: channelID}
	for _, f := range strings.Split(fields, ",") {
		switch strings.TrimSpace(f) {
		case "join_power":
			msg.NeededJoinPower = &neededJoinPower
		case "order":
			msg.OrderIndex = &orderIndex
		case "parent":
			msg.ParentID = &parentID
		case "inherit":
			msg.InheritPermissions = &inheritPermissions
		}
	}
	if msg.NeededJoinPower == nil && msg.OrderIndex == nil && msg.ParentID == nil && msg.InheritPermissions == nil {
		return ""
	}
	if err := a.cmLoad().write(netproto.MsgChannelEdit, msg); err != nil {
		return err.Error()
	}
	return ""
}

// DeleteChannel deletes a channel (167 confirm + subtree warning in the UI).
func (a *App) DeleteChannel(channelID int64) string {
	if err := a.cmLoad().write(netproto.MsgDeleteChannel, netproto.DeleteChannel{ChannelID: channelID}); err != nil {
		return err.Error()
	}
	return ""
}

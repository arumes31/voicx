// extras.go implements the Phase 9 control handlers: avatars, channel icons,
// privilege-token redemption, complaints, and screen-share declaration.
package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"go.uber.org/zap"

	"voicx/internal/netproto"
	"voicx/internal/permissions"
	"voicx/internal/store"
)

// maxImageBytes is the maximum decoded size of an avatar or channel icon.
const maxImageBytes = 256 * 1024

// imageExts maps accepted content types to file extensions.
var imageExts = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// Event types for the Phase 9 features.
const (
	eventAvatarChanged       = "avatar_changed"
	eventChannelIconChanged  = "channel_icon_changed"
	eventScreenshareChanged  = "screenshare_changed"
	eventTokenUsed           = "token_used"
	eventServerBannerChanged = "server_banner_changed"
)

// decodeImage validates a base64-encoded avatar/icon image: it decodes,
// enforces the size cap, and sniffs the content type, returning the raw
// bytes and the file extension for the detected type.
func decodeImage(dataBase64 string) ([]byte, string, error) {
	raw, err := base64.StdEncoding.DecodeString(dataBase64)
	if err != nil {
		return nil, "", errors.New("invalid base64 data")
	}
	if len(raw) == 0 {
		return nil, "", errors.New("empty image data")
	}
	if len(raw) > maxImageBytes {
		return nil, "", errors.New("image too large (max 256 KiB)")
	}
	ext, ok := imageExts[http.DetectContentType(raw)]
	if !ok {
		return nil, "", errors.New("unsupported image type (want png, jpeg, gif, or webp)")
	}
	return raw, ext, nil
}

// avatarDir returns the avatar storage directory under the file root.
func (s *TCPServer) avatarDir() string {
	return filepath.Join(s.cfg.FileRoot, "avatars")
}

// iconDir returns the channel-icon storage directory under the file root.
func (s *TCPServer) iconDir() string {
	return filepath.Join(s.cfg.FileRoot, "icons")
}

// handleAvatarSet validates and stores the client's avatar, replacing any
// previous one, and announces the change.
func (s *TCPServer) handleAvatarSet(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.AvatarSet
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed avatar_set: "+err.Error())
	}
	if client.UserID == 0 {
		return s.sendError(client, errCodePermissionDenied, "guests cannot upload avatars")
	}
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.granted(permissions.PermissionKeyClientAvatarUpload) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyClientAvatarUpload))
	}

	raw, ext, err := decodeImage(msg.DataBase64)
	if err != nil {
		return s.sendError(client, errCodeMalformed, err.Error())
	}
	if err := os.MkdirAll(s.avatarDir(), 0o750); err != nil {
		return s.sendError(client, errCodeUnavailable, "avatar storage unavailable")
	}
	// Remove previous avatars of this user (any extension) so the latest set
	// wins deterministically.
	for _, e := range imageExts {
		_ = os.Remove(filepath.Join(s.avatarDir(), client.UniqueID+e))
	}
	if err := os.WriteFile(filepath.Join(s.avatarDir(), client.UniqueID+ext), raw, 0o640); err != nil {
		return s.sendError(client, errCodeUnavailable, "storing avatar failed")
	}

	s.broadcastEvent(eventAvatarChanged, userEvent{ClientID: client.ID, UniqueID: client.UniqueID})
	return nil
}

// handleAvatarGet returns another user's avatar, if set.
func (s *TCPServer) handleAvatarGet(_ context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.AvatarGet
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed avatar_get: "+err.Error())
	}

	for ct, ext := range imageExts {
		raw, err := os.ReadFile(filepath.Join(s.avatarDir(), msg.UniqueID+ext))
		if err != nil {
			continue
		}
		return s.writeMessage(client, netproto.MsgAvatarData, netproto.AvatarData{
			UniqueID:    msg.UniqueID,
			DataBase64:  base64.StdEncoding.EncodeToString(raw),
			ContentType: ct,
		})
	}
	return s.sendError(client, errCodeNotFound, "no avatar for this user")
}

// handleChannelIconSet stores an icon for a channel (perm-gated by
// b_channel_modify) and flags the channel in state.
func (s *TCPServer) handleChannelIconSet(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ChannelIconSet
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed channel_icon_set: "+err.Error())
	}
	if s.deps == nil || s.deps.State == nil {
		return s.sendError(client, errCodeUnavailable, "state backend unavailable")
	}
	if _, ok := s.deps.State.GetChannel(msg.ChannelID); !ok {
		return s.sendError(client, errCodeNotFound, "channel not found")
	}

	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.granted(permissions.PermissionKeyChannelModify) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyChannelModify))
	}

	raw, ext, err := decodeImage(msg.DataBase64)
	if msg.CopyFromChannelID != 0 && msg.DataBase64 == "" {
		// (271) icon library: reuse another channel's stored icon.
		raw = nil
		ext = ""
		for _, e := range imageExts {
			b, rerr := os.ReadFile(filepath.Join(s.iconDir(), strconv.FormatInt(msg.CopyFromChannelID, 10)+e))
			if rerr == nil {
				raw, ext = b, e
				break
			}
		}
		if raw == nil {
			return s.sendError(client, errCodeNotFound, "source channel has no icon")
		}
	} else if err != nil {
		return s.sendError(client, errCodeMalformed, err.Error())
	}
	if err := os.MkdirAll(s.iconDir(), 0o750); err != nil {
		return s.sendError(client, errCodeUnavailable, "icon storage unavailable")
	}
	name := strconv.FormatInt(msg.ChannelID, 10)
	for _, e := range imageExts {
		_ = os.Remove(filepath.Join(s.iconDir(), name+e))
	}
	if err := os.WriteFile(filepath.Join(s.iconDir(), name+ext), raw, 0o640); err != nil {
		return s.sendError(client, errCodeUnavailable, "storing icon failed")
	}

	if ch, ok := s.deps.State.GetChannel(msg.ChannelID); ok {
		ch.HasIcon = true
	}
	s.broadcastEvent(eventChannelIconChanged, channelEvent{ChannelID: msg.ChannelID})
	return nil
}

// handleChannelIconGet returns a channel's icon (271; mirrors
// handleGroupIconGet). Ungated like the group and avatar reads: an icon is
// presentation data, and the channel list that names it is already visible.
// An empty payload means no icon, which is a normal answer, not an error.
func (s *TCPServer) handleChannelIconGet(_ context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ChannelIconGet
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed channel_icon_get: "+err.Error())
	}
	name := strconv.FormatInt(msg.ChannelID, 10)
	for ct, ext := range imageExts {
		raw, err := os.ReadFile(filepath.Join(s.iconDir(), name+ext))
		if err != nil {
			continue
		}
		return s.writeMessage(client, netproto.MsgChannelIconData, netproto.ChannelIconData{
			ChannelID:   msg.ChannelID,
			DataBase64:  base64.StdEncoding.EncodeToString(raw),
			ContentType: ct,
		})
	}
	return s.writeMessage(client, netproto.MsgChannelIconData, netproto.ChannelIconData{ChannelID: msg.ChannelID})
}

// --- server icon + banner (270) -----------------------------------------------

// brandingPath finds a stored server branding image by its base name (any
// accepted extension). The icon and the banner differ only in that name, so
// they share the lookup, the admin gate and the 256 KiB cap.
func (s *TCPServer) brandingPath(base string) (string, string, bool) {
	for ct, ext := range imageExts {
		p := filepath.Join(s.cfg.FileRoot, base+ext)
		if _, err := os.Stat(p); err == nil {
			return p, ct, true
		}
	}
	return "", "", false
}

// serverIconPath finds the stored server icon (any accepted extension).
func (s *TCPServer) serverIconPath() (string, string, bool) {
	return s.brandingPath("server_icon")
}

// storeBranding writes one admin-only branding image, replacing whatever
// extension was there before so a png does not linger behind a new jpg.
func (s *TCPServer) storeBranding(ctx context.Context, client *Client, base, dataBase64, action string) error {
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.admin {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: server branding is admin-only")
	}
	raw, ext, err := decodeImage(dataBase64)
	if err != nil {
		return s.sendError(client, errCodeMalformed, err.Error())
	}
	if err := os.MkdirAll(s.cfg.FileRoot, 0o750); err != nil {
		return s.sendError(client, errCodeUnavailable, "branding storage unavailable")
	}
	for _, e := range imageExts {
		_ = os.Remove(filepath.Join(s.cfg.FileRoot, base+e))
	}
	if err := os.WriteFile(filepath.Join(s.cfg.FileRoot, base+ext), raw, 0o640); err != nil {
		return s.sendError(client, errCodeUnavailable, "branding write failed")
	}
	s.audit(ctx, client.UniqueID, action, "", ext)
	return nil
}

// handleServerBannerSet stores the server banner (admin only, 270).
func (s *TCPServer) handleServerBannerSet(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ServerBannerSet
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed server_banner_set: "+err.Error())
	}
	if err := s.storeBranding(ctx, client, "server_banner", msg.DataBase64, "server_banner_set"); err != nil {
		return err
	}
	// Unlike the icon, the banner is a wide masthead every connected client
	// paints immediately, so announce it instead of waiting for a reconnect.
	s.broadcastEvent(eventServerBannerChanged, map[string]any{"by": client.UniqueID})
	return nil
}

// handleServerBannerGet returns the server banner (empty payload when unset).
func (s *TCPServer) handleServerBannerGet(_ context.Context, client *Client, f *netproto.Frame) error {
	if err := netproto.Decode(f, &netproto.ServerBannerGet{}); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed server_banner_get: "+err.Error())
	}
	p, ct, ok := s.brandingPath("server_banner")
	if !ok {
		return s.writeMessage(client, netproto.MsgServerBannerDat, netproto.ServerBannerData{})
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return s.writeMessage(client, netproto.MsgServerBannerDat, netproto.ServerBannerData{})
	}
	return s.writeMessage(client, netproto.MsgServerBannerDat, netproto.ServerBannerData{
		DataBase64:  base64.StdEncoding.EncodeToString(raw),
		ContentType: ct,
	})
}

// handleServerIconSet stores the server icon (admin only, 270).
func (s *TCPServer) handleServerIconSet(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ServerIconSet
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed server_icon_set: "+err.Error())
	}
	return s.storeBranding(ctx, client, "server_icon", msg.DataBase64, "server_icon_set")
}

// handleServerIconGet returns the server icon (empty payload when unset).
func (s *TCPServer) handleServerIconGet(_ context.Context, client *Client, f *netproto.Frame) error {
	if err := netproto.Decode(f, &netproto.ServerIconGet{}); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed server_icon_get: "+err.Error())
	}
	p, ct, ok := s.serverIconPath()
	if !ok {
		return s.writeMessage(client, netproto.MsgServerIconData, netproto.ServerIconData{})
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return s.writeMessage(client, netproto.MsgServerIconData, netproto.ServerIconData{})
	}
	return s.writeMessage(client, netproto.MsgServerIconData, netproto.ServerIconData{
		DataBase64:  base64.StdEncoding.EncodeToString(raw),
		ContentType: ct,
	})
}

// handleTokenUse redeems a privilege token: the grant (server group or
// admin) is applied by the store, the permission cache is invalidated, and
// the client is notified.
func (s *TCPServer) handleTokenUse(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.TokenUse
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed token_use: "+err.Error())
	}
	if s.deps == nil || s.deps.Tokens == nil {
		return s.sendError(client, errCodeUnavailable, "token backend unavailable")
	}
	if msg.Token == "" {
		return s.sendError(client, errCodeMalformed, "empty token")
	}
	// Redemption writes server-group membership or the admin flag against a
	// users row; a guest has none, so redeeming would either fail on the FK
	// or grant nothing that survives the session (174).
	if client.UserID == 0 {
		return s.sendError(client, errCodePermissionDenied, "guests cannot redeem privilege tokens: register or log in first")
	}

	groupID, err := s.deps.Tokens.UseToken(ctx, msg.Token, client.UserID)
	if err != nil {
		if errors.Is(err, store.ErrTokenNotFound) {
			return s.sendError(client, errCodeNotFound, "unknown token")
		}
		if errors.Is(err, store.ErrTokenExhausted) {
			return s.sendError(client, errCodeMalformed, "token has no uses left")
		}
		s.logger.Warn("token use failed",
			zap.String("client_id", client.ID),
			zap.Error(err),
		)
		return s.sendError(client, errCodeUnavailable, "token use failed")
	}

	// Invalidate the cached permissions so the grant takes effect now.
	if s.deps.Perms != nil {
		var channelID int64
		if s.deps.State != nil {
			if sc, ok := s.deps.State.GetClient(client.ID); ok {
				channelID = sc.ChannelID
			}
		}
		s.deps.Perms.Invalidate(client.UserID, channelID)
	}

	s.logger.Info("privilege token used",
		zap.String("client_id", client.ID),
		zap.Int64("group_id", groupID),
	)
	s.audit(ctx, client.UniqueID, "token_use", fmt.Sprintf("group:%d", groupID), "")
	if s.deps.Broadcast != nil {
		payload, err := eventEnvelope(eventTokenUsed, struct {
			ClientID string `json:"client_id"`
			GroupID  int64  `json:"group_id"`
		}{ClientID: client.ID, GroupID: groupID})
		if err == nil {
			_ = s.deps.Broadcast.BroadcastToClient(client.ID, payload)
		}
	}
	return nil
}

// --- token management (174) --------------------------------------------------

// serverGroupNameResolver returns a memoized group ID -> name lookup. Group
// 0 is the admin-granting token and has no group, so it resolves to "".
func (s *TCPServer) serverGroupNameResolver(ctx context.Context) func(int64) string {
	cache := make(map[int64]string)
	return func(groupID int64) string {
		if groupID == 0 || s.deps == nil || s.deps.Groups == nil {
			return ""
		}
		if n, ok := cache[groupID]; ok {
			return n
		}
		n := ""
		if g, err := s.deps.Groups.GetGroup(ctx, "server", groupID); err == nil && g != nil {
			n = g.Name
		}
		cache[groupID] = n
		return n
	}
}

// tokensResponse builds the privilege-token list with group display names.
func (s *TCPServer) tokensResponse(ctx context.Context) (netproto.Tokens, error) {
	rows, err := s.deps.Tokens.ListTokens(ctx)
	if err != nil {
		return netproto.Tokens{}, err
	}
	groupName := s.serverGroupNameResolver(ctx)
	resp := netproto.Tokens{Entries: []netproto.TokenEntry{}}
	for _, t := range rows {
		resp.Entries = append(resp.Entries, netproto.TokenEntry{
			Token:       t.Key,
			GroupID:     t.GroupID,
			GroupName:   groupName(t.GroupID),
			ChannelID:   t.ChannelID,
			Description: t.Description,
			CreatedAt:   t.CreatedAt.Unix(),
			UsedBy:      t.UsedBy,
		})
	}
	return resp, nil
}

// tokenAdminAllowed reports whether the caller holds the given
// b_virtualserver_token_* key (174).
func (s *TCPServer) tokenAdminAllowed(ctx context.Context, client *Client, key permissions.PermissionKey) (*permChecker, bool) {
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return nil, false
	}
	return pc, pc.granted(key)
}

// handleTokenList returns the privilege tokens (174).
func (s *TCPServer) handleTokenList(ctx context.Context, client *Client, f *netproto.Frame) error {
	if err := netproto.Decode(f, &netproto.TokenList{}); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed token_list: "+err.Error())
	}
	if s.deps == nil || s.deps.Tokens == nil {
		return s.sendError(client, errCodeUnavailable, "token backend unavailable")
	}
	if _, ok := s.tokenAdminAllowed(ctx, client, permissions.PermissionKeyVirtualserverTokenList); !ok {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyVirtualserverTokenList))
	}
	resp, err := s.tokensResponse(ctx)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "token list failed")
	}
	return s.writeMessage(client, netproto.MsgTokens, resp)
}

// handleTokenAdd mints a privilege token and replies with the refreshed list
// so the manager cannot drift (174). The key itself is generated by the
// store; the wire message carries no token field to honor.
func (s *TCPServer) handleTokenAdd(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.TokenAdd
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed token_add: "+err.Error())
	}
	if s.deps == nil || s.deps.Tokens == nil {
		return s.sendError(client, errCodeUnavailable, "token backend unavailable")
	}
	pc, ok := s.tokenAdminAllowed(ctx, client, permissions.PermissionKeyVirtualserverTokenAdd)
	if !ok {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyVirtualserverTokenAdd))
	}
	// A group-less token grants server admin on redemption, so minting one is
	// an admin act in itself and must not ride b_virtualserver_token_add.
	if msg.GroupID == 0 && !pc.admin {
		return s.sendError(client, errCodePermissionDenied, "only a server admin may create an admin token")
	}
	if msg.GroupID != 0 && s.deps.Groups != nil {
		g, gerr := s.deps.Groups.GetGroup(ctx, "server", msg.GroupID)
		if gerr != nil {
			return s.sendError(client, errCodeUnavailable, "group lookup failed")
		}
		if g == nil {
			return s.sendError(client, errCodeNotFound, "server group not found")
		}
	}

	// token_type 1 marks a channel-scoped key; 0 is a plain server group key.
	tokenType := 0
	if msg.ChannelID != 0 {
		tokenType = 1
	}
	_, err := s.deps.Tokens.CreateTokenWithMeta(ctx, tokenType, msg.GroupID, msg.ChannelID, msg.Description, 1)
	if err != nil {
		s.logger.Warn("token create failed",
			zap.String("client_id", client.ID),
			zap.Error(err),
		)
		return s.sendError(client, errCodeUnavailable, "token create failed")
	}
	s.audit(ctx, client.UniqueID, "token_add", strconv.FormatInt(msg.GroupID, 10),
		fmt.Sprintf("channel=%d type=%d", msg.ChannelID, tokenType))

	resp, err := s.tokensResponse(ctx)
	if err != nil {
		s.logger.Warn("token created but refreshed list failed", zap.Error(err))
		return s.sendError(client, errCodeUnavailable, "token created; token list refresh failed — re-list tokens")
	}
	return s.writeMessage(client, netproto.MsgTokens, resp)
}

// handleTokenDelete revokes a token and replies with the refreshed list (174).
func (s *TCPServer) handleTokenDelete(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.TokenDelete
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed token_delete: "+err.Error())
	}
	if s.deps == nil || s.deps.Tokens == nil {
		return s.sendError(client, errCodeUnavailable, "token backend unavailable")
	}
	if _, ok := s.tokenAdminAllowed(ctx, client, permissions.PermissionKeyVirtualserverTokenDelete); !ok {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyVirtualserverTokenDelete))
	}
	if msg.Token == "" {
		return s.sendError(client, errCodeMalformed, "empty token")
	}
	if err := s.deps.Tokens.DeleteToken(ctx, msg.Token); err != nil {
		if errors.Is(err, store.ErrTokenNotFound) {
			return s.sendError(client, errCodeNotFound, "unknown token")
		}
		return s.sendError(client, errCodeUnavailable, "token delete failed")
	}
	s.audit(ctx, client.UniqueID, "token_delete", msg.Token, "")

	resp, err := s.tokensResponse(ctx)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "token list failed")
	}
	return s.writeMessage(client, netproto.MsgTokens, resp)
}

// handleClientInfoQuery returns the connection info of an online client.
// Self queries always return full data (including own IP/port). For other
// clients, IP and port are only included when the requester is admin or
// holds b_client_remoteaddress_view (deny-on-unset; IP is sensitive).
func (s *TCPServer) handleClientInfoQuery(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ClientInfoQuery
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed client_info_query: "+err.Error())
	}

	target, ok := s.clientByID(msg.ClientID)
	if !ok || !target.isAuthed() {
		return s.sendError(client, errCodeNotFound, "client not found")
	}

	resp := netproto.ClientInfoResponse{
		ClientID: target.ID,
		PingMs:   -1,
	}
	{
		target.mu.RLock()
		resp.UniqueID = target.UniqueID
		resp.Nickname = target.Username
		target.mu.RUnlock()
	}
	if s.deps != nil && s.deps.State != nil {
		if sc, ok := s.deps.State.GetClient(target.ID); ok {
			resp.ChannelID = sc.ChannelID
			resp.ConnectedAt = sc.ConnectedAt.Unix()
		}
	}
	st := target.stats()
	resp.IdleSeconds = int64(time.Since(st.lastActive).Seconds())
	if st.rttKnown {
		resp.PingMs = st.rttNs / int64(time.Millisecond)
	}
	resp.BytesIn = st.bytesIn
	resp.BytesOut = st.bytesOut

	// IP/port gating: self, admin, or b_client_remoteaddress_view.
	showAddr := target.ID == client.ID
	if !showAddr {
		pc, err := s.permCheckerFor(ctx, client)
		if err != nil {
			return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
		}
		showAddr = pc.granted(permissions.PermissionKeyClientRemoteAddressView)
	}
	if showAddr {
		if host, portStr, err := net.SplitHostPort(target.Conn.RemoteAddr().String()); err == nil {
			resp.IP = host
			resp.Port, _ = strconv.Atoi(portStr)
		}
	}

	return s.writeMessage(client, netproto.MsgClientInfoResponse, resp)
}

// handleComplaint files a complaint against a user. The store enforces the
// open-complaint limit per reporter.
func (s *TCPServer) handleComplaint(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.Complaint
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed complaint: "+err.Error())
	}
	if s.deps == nil || s.deps.Complaints == nil || s.deps.Auth == nil {
		return s.sendError(client, errCodeUnavailable, "complaint backend unavailable")
	}
	if msg.Reason == "" {
		return s.sendError(client, errCodeMalformed, "reason must not be empty")
	}

	if _, err := s.deps.Auth.LookupUser(ctx, msg.TargetUniqueID); err != nil {
		return s.sendError(client, errCodeNotFound, "target user not found")
	}
	if err := s.deps.Complaints.AddComplaint(ctx, client.UniqueID, msg.TargetUniqueID, msg.Reason); err != nil {
		if errors.Is(err, store.ErrComplaintLimit) {
			return s.sendError(client, errCodeMalformed, "too many open complaints")
		}
		return s.sendError(client, errCodeUnavailable, "filing complaint failed")
	}
	return nil
}

// --- complaint review (173) --------------------------------------------------

// nicknameResolver returns a memoized unique ID -> nickname lookup: the
// online client first, then the users row. Complaint rows store only unique
// IDs, which are unreadable in an admin list (173). Unknown IDs resolve to
// "" so the client can fall back to the unique ID.
func (s *TCPServer) nicknameResolver(ctx context.Context) func(string) string {
	cache := make(map[string]string)
	return func(uniqueID string) string {
		if uniqueID == "" {
			return ""
		}
		if n, ok := cache[uniqueID]; ok {
			return n
		}
		n := ""
		if s.deps != nil && s.deps.State != nil {
			if sc, ok := s.deps.State.GetClientByUniqueID(uniqueID); ok {
				n = sc.Nickname
			}
		}
		if n == "" && s.deps != nil && s.deps.Auth != nil {
			if u, err := s.deps.Auth.LookupUser(ctx, uniqueID); err == nil && u != nil {
				n = u.Nickname
			}
		}
		cache[uniqueID] = n
		return n
	}
}

// complaintsResponse builds the complaint list with display nicknames.
func (s *TCPServer) complaintsResponse(ctx context.Context) (netproto.Complaints, error) {
	rows, err := s.deps.Complaints.ListComplaints(ctx)
	if err != nil {
		return netproto.Complaints{}, err
	}
	nick := s.nicknameResolver(ctx)
	resp := netproto.Complaints{Entries: []netproto.ComplaintEntry{}}
	for _, c := range rows {
		resp.Entries = append(resp.Entries, netproto.ComplaintEntry{
			TargetUniqueID: c.Target,
			TargetNickname: nick(c.Target),
			FromUniqueID:   c.Reporter,
			FromNickname:   nick(c.Reporter),
			Reason:         c.Reason,
			CreatedAt:      c.CreatedAt.Unix(),
		})
	}
	return resp, nil
}

// complaintAdminAllowed gates complaint review. Complaints are moderation
// evidence naming both parties, so they ride the same gate as the ban list
// rather than a key of their own (173).
func (s *TCPServer) complaintAdminAllowed(ctx context.Context, client *Client) bool {
	_, ok := s.banAdminAllowed(ctx, client)
	return ok
}

// handleComplaintList returns every filed complaint (173).
func (s *TCPServer) handleComplaintList(ctx context.Context, client *Client, f *netproto.Frame) error {
	if err := netproto.Decode(f, &netproto.ComplaintList{}); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed complaint_list: "+err.Error())
	}
	if s.deps == nil || s.deps.Complaints == nil {
		return s.sendError(client, errCodeUnavailable, "complaint backend unavailable")
	}
	if !s.complaintAdminAllowed(ctx, client) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyClientBan))
	}
	resp, err := s.complaintsResponse(ctx)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "complaint list failed")
	}
	return s.writeMessage(client, netproto.MsgComplaints, resp)
}

// handleComplaintClear resolves complaints against a target and replies with
// the refreshed list so the admin view cannot drift (173).
func (s *TCPServer) handleComplaintClear(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ComplaintClear
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed complaint_clear: "+err.Error())
	}
	if s.deps == nil || s.deps.Complaints == nil {
		return s.sendError(client, errCodeUnavailable, "complaint backend unavailable")
	}
	if !s.complaintAdminAllowed(ctx, client) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyClientBan))
	}
	if msg.TargetUniqueID == "" {
		return s.sendError(client, errCodeMalformed, "target_unique_id must not be empty")
	}

	n, err := s.deps.Complaints.DeleteComplaintsAgainst(ctx, msg.TargetUniqueID, msg.FromUniqueID)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "complaint clear failed")
	}
	if n == 0 {
		return s.sendError(client, errCodeNotFound, "no matching complaint")
	}
	detail := fmt.Sprintf("count=%d", n)
	if msg.FromUniqueID != "" {
		detail = fmt.Sprintf("from=%s count=%d", msg.FromUniqueID, n)
	}
	s.audit(ctx, client.UniqueID, "complaint_clear", msg.TargetUniqueID, detail)

	resp, err := s.complaintsResponse(ctx)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "complaint list failed")
	}
	return s.writeMessage(client, netproto.MsgComplaints, resp)
}

// handlePermissionsQuery returns the caller's resolved permission set: one
// entry per key present in any tier, resolved through the tier hierarchy.
func (s *TCPServer) handlePermissionsQuery(ctx context.Context, client *Client, f *netproto.Frame) error {
	if err := netproto.Decode(f, &netproto.PermissionsQuery{}); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed permissions_query: "+err.Error())
	}
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}

	resp := netproto.PermissionsResponse{}
	seen := make(map[permissions.PermissionKey]bool)
	for tier := permissions.Tier(0); tier <= permissions.TierChannel; tier++ {
		set, ok := pc.tp.Get(tier)
		if !ok || set == nil {
			continue
		}
		for _, key := range set.Keys() {
			if seen[key] {
				continue
			}
			seen[key] = true
			p, sourceTier, err := pc.resolver.Resolve(pc.tp, key)
			if err != nil {
				continue
			}
			resp.Entries = append(resp.Entries, netproto.PermissionEntry{
				Key:        string(p.Key),
				Value:      p.Value,
				Grant:      p.Grant,
				Skip:       p.Skip,
				Negate:     p.Negate,
				SourceTier: sourceTier.String(),
				Inherited:  sourceTier != tier,
			})
		}
	}
	for _, conflict := range permissions.DetectConflicts(pc.tp) {
		resp.Conflicts = append(resp.Conflicts, netproto.PermissionConflict{
			Key: string(conflict.Key), WinningTier: conflict.WinningTier.String(),
			ShadowedTier: conflict.ShadowedTier.String(), Message: conflict.Message,
		})
	}
	return s.writeMessage(client, netproto.MsgPermissionsResponse, resp)
}

// handleScreenShare relays the client's screen-share state to the other
// members of its channel. The video track itself is routed like camera
// video (one video track per peer; see the video SFU docs). Gated by the
// video publish permission.
func (s *TCPServer) handleScreenShare(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ScreenShare
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed screen_share: "+err.Error())
	}
	if s.deps == nil || s.deps.State == nil || s.deps.Broadcast == nil {
		return s.sendError(client, errCodeUnavailable, "state backend unavailable")
	}

	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.videoPublishAllowed() {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyClientVideoPublish))
	}
	if msg.Active && msg.MaxHeight > 720 && !pc.allowedByDefault(permissions.PermissionKeyClientScreenShare1080p) {
		return s.sendError(client, errCodePermissionDenied, "1080p screen sharing is not permitted; use a 720p preset")
	}

	sc, ok := s.deps.State.GetClient(client.ID)
	if !ok || sc.ChannelID == 0 {
		return s.sendError(client, errCodeNotFound, "not in a channel")
	}
	payload, err := eventEnvelope(eventScreenshareChanged, struct {
		ClientID  string `json:"client_id"`
		Active    bool   `json:"active"`
		MaxHeight int    `json:"max_height,omitempty"`
	}{ClientID: client.ID, Active: msg.Active, MaxHeight: msg.MaxHeight})
	if err != nil {
		return err
	}
	s.deps.Broadcast.BroadcastToChannel(sc.ChannelID, payload)
	return nil
}

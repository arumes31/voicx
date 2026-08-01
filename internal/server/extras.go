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
	eventAvatarChanged      = "avatar_changed"
	eventChannelIconChanged = "channel_icon_changed"
	eventScreenshareChanged = "screenshare_changed"
	eventTokenUsed          = "token_used"
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
func (s *TCPServer) handleAvatarSet(_ context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.AvatarSet
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed avatar_set: "+err.Error())
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

// --- server icon (270) --------------------------------------------------------

// serverIconPath finds the stored server icon (any accepted extension).
func (s *TCPServer) serverIconPath() (string, string, bool) {
	for ct, ext := range imageExts {
		p := filepath.Join(s.cfg.FileRoot, "server_icon"+ext)
		if _, err := os.Stat(p); err == nil {
			return p, ct, true
		}
	}
	return "", "", false
}

// handleServerIconSet stores the server icon (admin only, 270).
func (s *TCPServer) handleServerIconSet(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ServerIconSet
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed server_icon_set: "+err.Error())
	}
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.admin {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: server icon is admin-only")
	}
	raw, ext, err := decodeImage(msg.DataBase64)
	if err != nil {
		return s.sendError(client, errCodeMalformed, err.Error())
	}
	if err := os.MkdirAll(s.cfg.FileRoot, 0o750); err != nil {
		return s.sendError(client, errCodeUnavailable, "icon storage unavailable")
	}
	for _, e := range imageExts {
		_ = os.Remove(filepath.Join(s.cfg.FileRoot, "server_icon"+e))
	}
	if err := os.WriteFile(filepath.Join(s.cfg.FileRoot, "server_icon"+ext), raw, 0o640); err != nil {
		return s.sendError(client, errCodeUnavailable, "icon write failed")
	}
	s.audit(ctx, client.UniqueID, "server_icon_set", "", ext)
	return nil
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
			p, _, err := pc.resolver.Resolve(pc.tp, key)
			if err != nil {
				continue
			}
			resp.Entries = append(resp.Entries, netproto.PermissionEntry{
				Key:    string(p.Key),
				Value:  p.Value,
				Grant:  p.Grant,
				Skip:   p.Skip,
				Negate: p.Negate,
			})
		}
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

	sc, ok := s.deps.State.GetClient(client.ID)
	if !ok || sc.ChannelID == 0 {
		return s.sendError(client, errCodeNotFound, "not in a channel")
	}
	payload, err := eventEnvelope(eventScreenshareChanged, struct {
		ClientID string `json:"client_id"`
		Active   bool   `json:"active"`
	}{ClientID: client.ID, Active: msg.Active})
	if err != nil {
		return err
	}
	s.deps.Broadcast.BroadcastToChannel(sc.ChannelID, payload)
	return nil
}

// control.go implements the Control service (232) on top of the ServerQuery
// backend, so gRPC and the query port cannot drift apart.
package grpcserver

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"voicx/internal/auth"
	"voicx/internal/query"
	voicxv1 "voicx/v1"
)

// controlService serves administration RPCs. The file-transfer RPCs are
// deliberately left to UnimplementedControlServer (codes.Unimplemented): the
// transfer tokens they would issue are minted by the control channel after a
// per-client permission check, and a bot API that hands out its own tokens
// would be a second, unchecked path to the file port.
type controlService struct {
	voicxv1.UnimplementedControlServer
	backend      query.Backend
	logger       *zap.Logger
	authenticate func(context.Context, string, string, string) (bool, error)
}

// Authenticate validates credentials. It is the one RPC that carries its own
// credentials; every other RPC repeats them in the authorization metadata
// header, so there is no session token to hand back and the field stays empty.
func (c *controlService) Authenticate(ctx context.Context, req *voicxv1.AuthenticateRequest) (*voicxv1.AuthenticateResponse, error) {
	ok, err := c.authenticate(ctx, remoteIPFromContext(ctx), req.GetUsername(), req.GetPassword())
	if err != nil {
		c.logger.Warn("grpc authenticate error", zap.Error(err))
		return nil, status.Error(codes.Internal, "internal error")
	}
	if !ok {
		return &voicxv1.AuthenticateResponse{Success: false, Error: "invalid credentials"}, nil
	}
	return &voicxv1.AuthenticateResponse{Success: true, UserId: req.GetUsername()}, nil
}

// ListChannels returns the channel tree, optionally rooted at one channel.
func (c *controlService) ListChannels(ctx context.Context, req *voicxv1.ListChannelsRequest) (*voicxv1.ListChannelsResponse, error) {
	var root int64
	if v := req.GetRootChannelId(); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "root_channel_id must be numeric")
		}
		root = parsed
	}
	all := c.backend.ListChannels(ctx)
	keep := subtree(all, root)
	resp := &voicxv1.ListChannelsResponse{}
	for _, ch := range all {
		if !keep[ch.ChannelID] {
			continue
		}
		resp.Channels = append(resp.Channels, &voicxv1.Channel{
			Id:             strconv.FormatInt(ch.ChannelID, 10),
			Name:           ch.Name,
			ParentId:       formatInt(ch.ParentID),
			MaxClients:     int32(ch.MaxClients),
			Permanent:      ch.Type == 2,
			CurrentClients: int32(ch.ClientCount),
		})
	}
	return resp, nil
}

// subtree returns the ids reachable from root (0 = the whole tree). The list
// is parent-ordered, so one pass suffices.
func subtree(all []query.ChannelInfo, root int64) map[int64]bool {
	keep := make(map[int64]bool, len(all))
	if root == 0 {
		for _, ch := range all {
			keep[ch.ChannelID] = true
		}
		return keep
	}
	keep[root] = true
	for _, ch := range all {
		if keep[ch.ParentID] {
			keep[ch.ChannelID] = true
		}
	}
	return keep
}

// CreateChannel creates a channel.
func (c *controlService) CreateChannel(ctx context.Context, req *voicxv1.CreateChannelRequest) (*voicxv1.CreateChannelResponse, error) {
	if strings.TrimSpace(req.GetName()) == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	channelType := 0
	if req.GetPermanent() {
		channelType = 2
	}
	id, err := c.backend.CreateChannel(ctx, req.GetName(), "", channelType)
	if err != nil {
		return &voicxv1.CreateChannelResponse{Success: false, Error: err.Error()}, nil
	}
	return &voicxv1.CreateChannelResponse{Success: true, ChannelId: strconv.FormatInt(id, 10)}, nil
}

// DeleteChannel removes a channel.
func (c *controlService) DeleteChannel(ctx context.Context, req *voicxv1.DeleteChannelRequest) (*voicxv1.DeleteChannelResponse, error) {
	id, err := strconv.ParseInt(req.GetChannelId(), 10, 64)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "channel_id must be numeric")
	}
	if err := c.backend.DeleteChannel(ctx, id); err != nil {
		return &voicxv1.DeleteChannelResponse{Success: false, Error: err.Error()}, nil
	}
	return &voicxv1.DeleteChannelResponse{Success: true}, nil
}

// protoPermFor maps the coarse proto permission enum onto the permission keys
// the resolver evaluates. The enum is a summary: only permissions it can name
// are reported.
var protoPermFor = map[string]voicxv1.Permission{
	"i_channel_join_power":             voicxv1.Permission_PERMISSION_JOIN,
	"i_client_talk_power":              voicxv1.Permission_PERMISSION_SPEAK,
	"b_client_video_publish":           voicxv1.Permission_PERMISSION_VIDEO,
	"b_client_use_channel_command":     voicxv1.Permission_PERMISSION_CHAT,
	"i_client_kick_from_channel_power": voicxv1.Permission_PERMISSION_KICK,
	"b_client_ban":                     voicxv1.Permission_PERMISSION_BAN,
	"i_client_move_power":              voicxv1.Permission_PERMISSION_MOVE,
	"b_channel_create_child":           voicxv1.Permission_PERMISSION_CREATE_CHANNEL,
	"b_channel_delete":                 voicxv1.Permission_PERMISSION_DELETE_CHANNEL,
	"i_ft_file_upload_power":           voicxv1.Permission_PERMISSION_TRANSFER_FILE,
}

// QueryPermissions summarises a user's resolved permissions in a channel.
func (c *controlService) QueryPermissions(ctx context.Context, req *voicxv1.QueryPermissionsRequest) (*voicxv1.QueryPermissionsResponse, error) {
	if req.GetUserId() == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id is required")
	}
	var channelID int64
	if v := req.GetChannelId(); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "channel_id must be numeric")
		}
		channelID = parsed
	}
	lines, err := c.backend.PermOverview(ctx, req.GetUserId(), channelID)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return nil, status.Error(codes.NotFound, "user not found")
		}
		c.logger.Warn("grpc permission overview error", zap.Error(err))
		return nil, status.Error(codes.Internal, "internal error")
	}
	resp := &voicxv1.QueryPermissionsResponse{}
	for _, line := range lines {
		perm, ok := protoPermFor[line.Key]
		if !ok || perm == voicxv1.Permission_PERMISSION_UNSPECIFIED {
			continue
		}
		if line.Value > 0 {
			resp.Granted = append(resp.Granted, perm)
		} else {
			resp.Denied = append(resp.Denied, perm)
		}
	}
	return resp, nil
}

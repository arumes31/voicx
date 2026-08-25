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
	"voicx/internal/channels"
	"voicx/internal/query"
	"voicx/internal/safecast"
	voicxv1 "voicx/v1"
)

const (
	maxDeleteReasonBytes      = 512
	maxInvalidChannelWarnings = 8
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
// credentials; every other RPC repeats them in the authorization metadata, so
// no session token is minted.
func (c *controlService) Authenticate(ctx context.Context, req *voicxv1.AuthenticateRequest) (*voicxv1.AuthenticateResponse, error) {
	ok, err := c.authenticate(ctx, remoteIPFromContext(ctx), req.GetUsername(), req.GetPassword())
	if err != nil {
		c.logger.Warn("grpc authenticate error", zap.Error(err))
		return nil, status.Error(codes.Internal, "internal error")
	}
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "invalid credentials")
	}
	return &voicxv1.AuthenticateResponse{UserId: req.GetUsername()}, nil
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
	all := uniqueChannels(c.backend.ListChannels(ctx))
	keep := subtreeCanonical(all, root)
	resp := &voicxv1.ListChannelsResponse{}
	warnings := 0
	for _, ch := range all {
		if !keep[ch.ChannelID] {
			continue
		}
		maxClients, err := safecast.IntToInt32(ch.MaxClients)
		if err != nil || ch.MaxClients < 0 {
			c.warnInvalidChannelRow(ch.ChannelID, "max_clients", &warnings)
			continue
		}
		currentClients, err := safecast.IntToInt32(ch.ClientCount)
		if err != nil || ch.ClientCount < 0 {
			c.warnInvalidChannelRow(ch.ChannelID, "current_clients", &warnings)
			continue
		}
		resp.Channels = append(resp.Channels, &voicxv1.Channel{
			Id:             strconv.FormatInt(ch.ChannelID, 10),
			Name:           ch.Name,
			ParentId:       formatInt(ch.ParentID),
			MaxClients:     maxClients,
			Permanent:      ch.Type == 2,
			CurrentClients: currentClients,
		})
	}
	return resp, nil
}

// warnInvalidChannelRow emits bounded, non-sensitive diagnostics for malformed
// backend rows. The value itself is not logged; only its channel ID and field
// name are useful to an operator.
func (c *controlService) warnInvalidChannelRow(channelID int64, field string, warnings *int) {
	if *warnings >= maxInvalidChannelWarnings {
		return
	}
	*warnings++
	if c.logger != nil {
		c.logger.Warn("skipping invalid channel row", zap.Int64("channel_id", channelID), zap.String("field", field))
	}
}

// uniqueChannels preserves first-occurrence order and treats a duplicate
// channel ID as malformed backend data whose later row is ignored. This keeps
// traversal and the wire response deterministic.
func uniqueChannels(all []query.ChannelInfo) []query.ChannelInfo {
	seen := make(map[int64]struct{}, len(all))
	unique := make([]query.ChannelInfo, 0, len(all))
	for _, channel := range all {
		if _, duplicate := seen[channel.ChannelID]; duplicate {
			continue
		}
		seen[channel.ChannelID] = struct{}{}
		unique = append(unique, channel)
	}
	return unique
}

// subtree returns the ids reachable from root (0 = the whole tree). Input
// order is not significant: rows may be returned child-before-parent. The
// first occurrence of a duplicate channel ID wins.
func subtree(all []query.ChannelInfo, root int64) map[int64]bool {
	return subtreeCanonical(uniqueChannels(all), root)
}

func subtreeCanonical(all []query.ChannelInfo, root int64) map[int64]bool {
	keep := make(map[int64]bool, len(all))
	if root == 0 {
		for _, ch := range all {
			keep[ch.ChannelID] = true
		}
		return keep
	}
	children := make(map[int64][]int64, len(all))
	foundRoot := false
	for _, ch := range all {
		children[ch.ParentID] = append(children[ch.ParentID], ch.ChannelID)
		if ch.ChannelID == root {
			foundRoot = true
		}
	}
	if !foundRoot {
		return keep
	}
	stack := []int64{root}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if keep[id] {
			continue
		}
		keep[id] = true
		stack = append(stack, children[id]...)
	}
	return keep
}

// CreateChannel creates a channel.
func (c *controlService) CreateChannel(ctx context.Context, req *voicxv1.CreateChannelRequest) (*voicxv1.CreateChannelResponse, error) {
	name := strings.TrimSpace(req.GetName())
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	var parentID int64
	if value := req.GetParentId(); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed <= 0 {
			return nil, status.Error(codes.InvalidArgument, "parent_id must be a positive integer")
		}
		parentID = parsed
	}
	maxClients, err := safecast.Int32ToInt(req.GetMaxClients())
	if err != nil || maxClients < 0 {
		return nil, status.Error(codes.InvalidArgument, "max_clients must not be negative")
	}
	channelType := 0
	if req.GetPermanent() {
		channelType = 2
	}
	id, err := c.backend.CreateChannel(ctx, query.ChannelCreateParams{
		Name:       name,
		ParentID:   parentID,
		MaxClients: maxClients,
		Type:       channelType,
	})
	if err != nil {
		return nil, grpcBackendStatus(c.logger, "create channel", err)
	}
	return &voicxv1.CreateChannelResponse{Success: true, ChannelId: strconv.FormatInt(id, 10)}, nil
}

// DeleteChannel removes a channel.
func (c *controlService) DeleteChannel(ctx context.Context, req *voicxv1.DeleteChannelRequest) (*voicxv1.DeleteChannelResponse, error) {
	id, err := strconv.ParseInt(req.GetChannelId(), 10, 64)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "channel_id must be numeric")
	}
	reason := req.GetReason()
	if len(reason) > maxDeleteReasonBytes {
		return nil, status.Error(codes.InvalidArgument, "reason is too long")
	}
	if err := c.backend.DeleteChannel(ctx, id, reason); err != nil {
		return nil, grpcBackendStatus(c.logger, "delete channel", err)
	}
	return &voicxv1.DeleteChannelResponse{Success: true}, nil
}

// grpcBackendStatus exposes only stable transport semantics. Backend details
// remain in structured server logs for unknown failures and never become a
// bot-visible error string.
func grpcBackendStatus(logger *zap.Logger, operation string, err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	case errors.Is(err, auth.ErrUserNotFound), errors.Is(err, channels.ErrChannelNotFound):
		return status.Error(codes.NotFound, "not found")
	case errors.Is(err, channels.ErrInvalidSpec), errors.Is(err, channels.ErrInvalidMove):
		return status.Error(codes.FailedPrecondition, "request cannot be applied")
	default:
		if logger != nil {
			logger.Warn("grpc backend operation failed", zap.String("operation", operation), zap.Error(err))
		}
		return status.Error(codes.Internal, "internal error")
	}
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
	uniqueID := req.GetUserId()
	if uniqueID == "" {
		var ok bool
		uniqueID, ok = callerIdentity(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "missing authenticated caller")
		}
	}
	var channelID int64
	if v := req.GetChannelId(); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "channel_id must be numeric")
		}
		channelID = parsed
	}
	lines, isAdmin, err := c.backend.PermOverview(ctx, uniqueID, channelID)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return nil, status.Error(codes.NotFound, "user not found")
		}
		c.logger.Warn("grpc permission overview error", zap.Error(err))
		return nil, status.Error(codes.Internal, "internal error")
	}
	resp := &voicxv1.QueryPermissionsResponse{IsAdmin: isAdmin}
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

// events.go implements Events.Subscribe on top of the shared event bus (232).
package grpcserver

import (
	"encoding/json"
	"fmt"
	"strconv"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"voicx/internal/eventbus"
	voicxv1 "voicx/v1"
)

// formatInt renders a channel id as the string the proto schema uses.
func formatInt(v int64) string {
	if v == 0 {
		return ""
	}
	return strconv.FormatInt(v, 10)
}

// formatUint renders the bus sequence number as the event id.
func formatUint(v uint64) string { return strconv.FormatUint(v, 10) }

// eventsService streams bus events to gRPC subscribers.
type eventsService struct {
	voicxv1.UnimplementedEventsServer
	bus    *eventbus.Bus
	logger *zap.Logger
}

// busEvent is the union of the JSON payloads the control server broadcasts for
// the event types the proto schema can express. Fields absent from a given
// event stay zero.
type busEvent struct {
	ClientID      string `json:"client_id"`
	UniqueID      string `json:"unique_id"`
	Nickname      string `json:"nickname"`
	ChannelID     int64  `json:"channel_id"`
	FromChannelID int64  `json:"from_channel_id"`
	Name          string `json:"name"`
	ParentID      int64  `json:"parent_id"`
	ByClientID    string `json:"by_client_id"`
	Reason        string `json:"reason"`
	Ban           bool   `json:"ban"`
	Speaking      bool   `json:"speaking"`
	ExpiresAt     int64  `json:"expires_at"`
}

// busTypeFor maps a proto EventType to the internal broadcast type string.
// Types the control server does not broadcast under any of these names (chat,
// typing, presence, ...) have no proto representation and are not streamed
// here; the WebSocket stream (231) carries the full set.
var busTypeFor = map[voicxv1.EventType]string{
	voicxv1.EventType_EVENT_TYPE_USER_JOINED:     "user_joined",
	voicxv1.EventType_EVENT_TYPE_USER_LEFT:       "user_left",
	voicxv1.EventType_EVENT_TYPE_USER_SPEAKING:   "speaking_changed",
	voicxv1.EventType_EVENT_TYPE_CHANNEL_CREATED: "channel_created",
	voicxv1.EventType_EVENT_TYPE_CHANNEL_DELETED: "channel_deleted",
	voicxv1.EventType_EVENT_TYPE_USER_MOVED:      "user_moved",
	// Kicks and bans share one broadcast; the payload decides which of the
	// two proto types an event becomes.
	voicxv1.EventType_EVENT_TYPE_USER_KICKED: "kicked",
	voicxv1.EventType_EVENT_TYPE_USER_BANNED: "kicked",
}

// allBusTypes is the unfiltered subscription, in a fixed order.
var allBusTypes = []string{
	"user_joined", "user_left", "speaking_changed",
	"channel_created", "channel_deleted", "user_moved", "kicked",
}

// subscribedTypes turns the request filter into a bus type filter plus the
// proto types the caller actually asked for. A nil proto filter means "all".
func subscribedTypes(req *voicxv1.SubscribeEventsRequest) ([]string, map[voicxv1.EventType]bool, error) {
	requested := req.GetEventTypes()
	if len(requested) == 0 {
		return allBusTypes, nil, nil
	}
	wanted := make(map[voicxv1.EventType]bool, len(requested))
	seen := make(map[string]bool, len(requested))
	busTypes := make([]string, 0, len(requested))
	for _, t := range requested {
		name, ok := busTypeFor[t]
		if !ok {
			return nil, nil, status.Errorf(codes.InvalidArgument,
				"unsupported event type %d (%s)", t, t.String())
		}
		wanted[t] = true
		if !seen[name] {
			seen[name] = true
			busTypes = append(busTypes, name)
		}
	}
	return busTypes, wanted, nil
}

// Subscribe streams server events until the client goes away or the bus drops
// the subscriber for not keeping up.
func (e *eventsService) Subscribe(req *voicxv1.SubscribeEventsRequest, stream grpc.ServerStreamingServer[voicxv1.Event]) error {
	caller := callerOf(stream)
	busTypes, wanted, err := subscribedTypes(req)
	if err != nil {
		return err
	}
	sub := e.bus.Subscribe("grpc:"+caller, busTypes, 0)
	if sub == nil {
		return status.Error(codes.Unavailable, "event stream unavailable")
	}
	defer sub.Unsubscribe()

	e.logger.Info("grpc event subscriber connected", zap.String("unique_id", caller))
	defer func() {
		e.logger.Info("grpc event subscriber disconnected",
			zap.String("unique_id", caller), zap.Uint64("dropped", sub.Dropped()))
	}()

	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return nil
		case evt, ok := <-sub.C:
			if !ok {
				switch sub.CloseReason() {
				case eventbus.CloseReasonSlowConsumer:
					return status.Error(codes.ResourceExhausted, "event stream fell behind; resync required")
				case eventbus.CloseReasonBusClosed:
					return status.Error(codes.Unavailable, "event stream unavailable")
				default:
					return nil
				}
			}
			msg, err := toProto(evt)
			if err != nil {
				e.logger.Warn("dropping malformed eventbus payload",
					zap.String("event_type", boundedEventType(evt.Type)),
					zap.Uint64("sequence", evt.Seq),
					zap.Error(err),
				)
				continue
			}
			if msg == nil || (wanted != nil && !wanted[msg.Type]) {
				continue
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
	}
}

const maxLoggedEventTypeBytes = 64

// boundedEventType retains enough type context for an operator without
// allowing an unexpected producer to make a log field unbounded.
func boundedEventType(eventType string) string {
	if len(eventType) <= maxLoggedEventTypeBytes {
		return eventType
	}
	return eventType[:maxLoggedEventTypeBytes] + "…"
}

// toProto converts a bus event into the proto envelope. It returns nil, nil
// when the event has no representation in the schema, and returns an error
// only when the payload could not be decoded.
func toProto(evt eventbus.Event) (*voicxv1.Event, error) {
	var payload busEvent
	if len(evt.Data) > 0 {
		if err := json.Unmarshal(evt.Data, &payload); err != nil {
			return nil, fmt.Errorf("decode event payload: %w", err)
		}
	}
	out := &voicxv1.Event{
		Id:        formatUint(evt.Seq),
		Timestamp: evt.Time.UnixMilli(),
	}
	// user_id and actor fields carry connected client/session IDs (client_id),
	// not the persistent account identifier (unique_id). A client/session ID is
	// available on every event, so bots can correlate across event types.
	switch evt.Type {
	case "user_joined":
		out.Type = voicxv1.EventType_EVENT_TYPE_USER_JOINED
		out.Payload = &voicxv1.Event_UserJoined{UserJoined: &voicxv1.UserJoinedEvent{
			ChannelId:   formatInt(payload.ChannelID),
			UserId:      payload.ClientID,
			DisplayName: payload.Nickname,
		}}
	case "user_left":
		out.Type = voicxv1.EventType_EVENT_TYPE_USER_LEFT
		out.Payload = &voicxv1.Event_UserLeft{UserLeft: &voicxv1.UserLeftEvent{
			ChannelId: formatInt(payload.ChannelID),
			UserId:    payload.ClientID,
			Reason:    payload.Reason,
		}}
	case "user_moved":
		out.Type = voicxv1.EventType_EVENT_TYPE_USER_MOVED
		out.Payload = &voicxv1.Event_UserMoved{UserMoved: &voicxv1.UserMovedEvent{
			UserId:        payload.ClientID,
			FromChannelId: formatInt(payload.FromChannelID),
			ToChannelId:   formatInt(payload.ChannelID),
			MovedBy:       payload.ByClientID,
		}}
	case "speaking_changed":
		out.Type = voicxv1.EventType_EVENT_TYPE_USER_SPEAKING
		out.Payload = &voicxv1.Event_UserSpeaking{UserSpeaking: &voicxv1.UserSpeakingEvent{
			ChannelId: formatInt(payload.ChannelID),
			UserId:    payload.ClientID,
			Speaking:  payload.Speaking,
		}}
	case "channel_created":
		out.Type = voicxv1.EventType_EVENT_TYPE_CHANNEL_CREATED
		out.Payload = &voicxv1.Event_ChannelCreated{ChannelCreated: &voicxv1.ChannelCreatedEvent{
			ChannelId: formatInt(payload.ChannelID),
			Name:      payload.Name,
			ParentId:  formatInt(payload.ParentID),
		}}
	case "channel_deleted":
		out.Type = voicxv1.EventType_EVENT_TYPE_CHANNEL_DELETED
		out.Payload = &voicxv1.Event_ChannelDeleted{ChannelDeleted: &voicxv1.ChannelDeletedEvent{
			ChannelId: formatInt(payload.ChannelID),
			Reason:    payload.Reason,
		}}
	case "kicked":
		// One broadcast covers both: a kick that also bans is reported as a ban.
		if payload.Ban {
			out.Type = voicxv1.EventType_EVENT_TYPE_USER_BANNED
			out.Payload = &voicxv1.Event_UserBanned{UserBanned: &voicxv1.UserBannedEvent{
				UserId:    payload.ClientID,
				BannedBy:  payload.ByClientID,
				Reason:    payload.Reason,
				ExpiresAt: payload.ExpiresAt,
				ChannelId: formatInt(payload.ChannelID),
			}}
			return out, nil
		}
		out.Type = voicxv1.EventType_EVENT_TYPE_USER_KICKED
		out.Payload = &voicxv1.Event_UserKicked{UserKicked: &voicxv1.UserKickedEvent{
			ChannelId: formatInt(payload.ChannelID),
			UserId:    payload.ClientID,
			KickedBy:  payload.ByClientID,
			Reason:    payload.Reason,
		}}
	default:
		return nil, nil
	}
	return out, nil
}

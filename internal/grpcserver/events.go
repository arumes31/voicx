// events.go implements Events.Subscribe on top of the shared event bus (232).
package grpcserver

import (
	"encoding/json"
	"strconv"

	"go.uber.org/zap"
	"google.golang.org/grpc"

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
	ClientID   string `json:"client_id"`
	UniqueID   string `json:"unique_id"`
	Nickname   string `json:"nickname"`
	ChannelID  int64  `json:"channel_id"`
	Name       string `json:"name"`
	ParentID   int64  `json:"parent_id"`
	ByClientID string `json:"by_client_id"`
	Reason     string `json:"reason"`
	Ban        bool   `json:"ban"`
	Speaking   bool   `json:"speaking"`
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
func subscribedTypes(req *voicxv1.SubscribeEventsRequest) ([]string, map[voicxv1.EventType]bool) {
	wanted := map[voicxv1.EventType]bool{}
	seen := map[string]bool{}
	var busTypes []string
	for _, t := range req.GetEventTypes() {
		name, ok := busTypeFor[t]
		if !ok {
			continue
		}
		wanted[t] = true
		if !seen[name] {
			seen[name] = true
			busTypes = append(busTypes, name)
		}
	}
	if len(wanted) == 0 {
		return allBusTypes, nil
	}
	return busTypes, wanted
}

// Subscribe streams server events until the client goes away or the bus drops
// the subscriber for not keeping up.
func (e *eventsService) Subscribe(req *voicxv1.SubscribeEventsRequest, stream grpc.ServerStreamingServer[voicxv1.Event]) error {
	caller := callerOf(stream)
	busTypes, wanted := subscribedTypes(req)
	sub := e.bus.Subscribe("grpc:"+caller, busTypes, 0)
	if sub == nil {
		return nil
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
				// Evicted by the drop policy: ending the stream tells the bot
				// its view is no longer complete.
				return nil
			}
			msg := toProto(evt)
			if msg == nil || (wanted != nil && !wanted[msg.Type]) {
				continue
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
	}
}

// toProto converts a bus event into the proto envelope, or nil when the event
// has no representation in the schema.
func toProto(evt eventbus.Event) *voicxv1.Event {
	var payload busEvent
	if len(evt.Data) > 0 {
		if err := json.Unmarshal(evt.Data, &payload); err != nil {
			return nil
		}
	}
	out := &voicxv1.Event{
		Id:        formatUint(evt.Seq),
		Timestamp: evt.Time.UnixMilli(),
	}
	// user_id is the SESSION id (client_id): it is the only identifier every
	// event carries, so bots can correlate across event types.
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
			UserId:      payload.ClientID,
			ToChannelId: formatInt(payload.ChannelID),
		}}
	case "speaking_changed":
		out.Type = voicxv1.EventType_EVENT_TYPE_USER_SPEAKING
		out.Payload = &voicxv1.Event_UserSpeaking{UserSpeaking: &voicxv1.UserSpeakingEvent{
			UserId:   payload.ClientID,
			Speaking: payload.Speaking,
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
		}}
	case "kicked":
		// One broadcast covers both: a kick that also bans is reported as a ban.
		if payload.Ban {
			out.Type = voicxv1.EventType_EVENT_TYPE_USER_BANNED
			out.Payload = &voicxv1.Event_UserBanned{UserBanned: &voicxv1.UserBannedEvent{
				UserId:   payload.ClientID,
				BannedBy: payload.ByClientID,
				Reason:   payload.Reason,
			}}
			return out
		}
		out.Type = voicxv1.EventType_EVENT_TYPE_USER_KICKED
		out.Payload = &voicxv1.Event_UserKicked{UserKicked: &voicxv1.UserKickedEvent{
			UserId:   payload.ClientID,
			KickedBy: payload.ByClientID,
			Reason:   payload.Reason,
		}}
	default:
		return nil
	}
	return out
}

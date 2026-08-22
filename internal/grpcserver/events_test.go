package grpcserver

import (
	"context"
	"encoding/base64"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"voicx/internal/eventbus"
	voicxv1 "voicx/v1"
)

// TestEventsSubscribe verifies the streaming RPC delivers bus events in proto
// form (232).
func TestEventsSubscribe(t *testing.T) {
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	client := voicxv1.NewEventsClient(dialGRPC(t, startGRPC(t, &stubBackend{}, bus)))

	stream, err := client.Subscribe(authCtx(t, "admin-uid", "pw"), &voicxv1.SubscribeEventsRequest{})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	waitForSubscribers(t, bus, 1)

	bus.Publish("user_joined", []byte(`{"client_id":"c-1","nickname":"bot","channel_id":7}`))
	evt, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if evt.GetType() != voicxv1.EventType_EVENT_TYPE_USER_JOINED {
		t.Fatalf("type = %v", evt.GetType())
	}
	joined := evt.GetUserJoined()
	if joined.GetUserId() != "c-1" || joined.GetDisplayName() != "bot" || joined.GetChannelId() != "7" {
		t.Fatalf("payload = %+v", joined)
	}
	if evt.GetId() == "" || evt.GetTimestamp() == 0 {
		t.Fatalf("envelope = %+v", evt)
	}

	// Events with no proto representation are skipped, not streamed as blanks.
	bus.Publish("typing", []byte(`{"client_id":"c-1"}`))
	bus.Publish("user_left", []byte(`{"client_id":"c-1"}`))
	next, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if next.GetType() != voicxv1.EventType_EVENT_TYPE_USER_LEFT {
		t.Fatalf("second event = %v", next.GetType())
	}
}

// TestEventsSubscribeFilter verifies the event_types filter, including the
// kick/ban pair that shares one broadcast (232).
func TestEventsSubscribeFilter(t *testing.T) {
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	client := voicxv1.NewEventsClient(dialGRPC(t, startGRPC(t, &stubBackend{}, bus)))

	stream, err := client.Subscribe(authCtx(t, "admin-uid", "pw"), &voicxv1.SubscribeEventsRequest{
		EventTypes: []voicxv1.EventType{voicxv1.EventType_EVENT_TYPE_USER_BANNED},
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	waitForSubscribers(t, bus, 1)

	bus.Publish("user_joined", []byte(`{"client_id":"c-1"}`))
	bus.Publish("kicked", []byte(`{"client_id":"c-1","by_client_id":"c-2","reason":"spam"}`))
	bus.Publish("kicked", []byte(`{"client_id":"c-3","by_client_id":"c-2","ban":true,"reason":"raid"}`))

	evt, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if evt.GetType() != voicxv1.EventType_EVENT_TYPE_USER_BANNED {
		t.Fatalf("filtered stream delivered %v", evt.GetType())
	}
	banned := evt.GetUserBanned()
	if banned.GetUserId() != "c-3" || banned.GetBannedBy() != "c-2" || banned.GetReason() != "raid" {
		t.Fatalf("ban payload = %+v", banned)
	}
}

// TestEventsSubscribeStopsWhenClientLeaves verifies the subscription is
// released when the RPC ends (232).
func TestEventsSubscribeStopsWhenClientLeaves(t *testing.T) {
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	client := voicxv1.NewEventsClient(dialGRPC(t, startGRPC(t, &stubBackend{}, bus)))

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.Subscribe(withAuth(ctx), &voicxv1.SubscribeEventsRequest{})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	waitForSubscribers(t, bus, 1)
	cancel()
	_, _ = stream.Recv()
	waitForSubscribers(t, bus, 0)
}

func TestEventsSubscribeExpiresAndCanReconnect(t *testing.T) {
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	addr := startGRPCWith(t, &stubBackend{}, bus, func(server *Server) {
		server.StreamLifetime = 30 * time.Millisecond
	})
	client := voicxv1.NewEventsClient(dialGRPC(t, addr))

	stream, err := client.Subscribe(authCtx(t, "admin-uid", "pw"), &voicxv1.SubscribeEventsRequest{})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	waitForSubscribers(t, bus, 1)
	if _, err := stream.Recv(); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("expired stream Recv = %v, want DeadlineExceeded", err)
	}
	waitForSubscribers(t, bus, 0)

	// The bounded lifetime requires reconnecting rather than leaving a stale
	// authenticated stream alive indefinitely.
	reconnected, err := client.Subscribe(authCtx(t, "admin-uid", "pw"), &voicxv1.SubscribeEventsRequest{})
	if err != nil {
		t.Fatalf("reconnect subscribe: %v", err)
	}
	waitForSubscribers(t, bus, 1)
	_ = reconnected
}

func TestSubscribedTypes(t *testing.T) {
	for _, test := range []struct {
		name      string
		types     []voicxv1.EventType
		wantBus   []string
		wantTypes map[voicxv1.EventType]bool
		wantCode  codes.Code
		contains  []string
	}{
		{
			name:    "empty means all",
			wantBus: allBusTypes,
		},
		{
			name:    "duplicates collapse only bus types",
			types:   []voicxv1.EventType{voicxv1.EventType_EVENT_TYPE_USER_JOINED, voicxv1.EventType_EVENT_TYPE_USER_JOINED, voicxv1.EventType_EVENT_TYPE_USER_BANNED},
			wantBus: []string{"user_joined", "kicked"},
			wantTypes: map[voicxv1.EventType]bool{
				voicxv1.EventType_EVENT_TYPE_USER_JOINED: true,
				voicxv1.EventType_EVENT_TYPE_USER_BANNED: true,
			},
		},
		{
			name:     "unspecified is rejected",
			types:    []voicxv1.EventType{voicxv1.EventType_EVENT_TYPE_UNSPECIFIED},
			wantCode: codes.InvalidArgument,
			contains: []string{"0", "EVENT_TYPE_UNSPECIFIED"},
		},
		{
			name:     "unknown is rejected",
			types:    []voicxv1.EventType{voicxv1.EventType(999)},
			wantCode: codes.InvalidArgument,
			contains: []string{"999"},
		},
		{
			name:     "mixed valid and unknown is rejected at first unsupported",
			types:    []voicxv1.EventType{voicxv1.EventType_EVENT_TYPE_USER_JOINED, voicxv1.EventType(999), voicxv1.EventType_EVENT_TYPE_USER_LEFT},
			wantCode: codes.InvalidArgument,
			contains: []string{"999"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			gotBus, gotTypes, err := subscribedTypes(&voicxv1.SubscribeEventsRequest{EventTypes: test.types})
			if gotCode := status.Code(err); gotCode != test.wantCode {
				t.Fatalf("subscribedTypes error = %v, want code %v", err, test.wantCode)
			}
			if err != nil {
				for _, fragment := range test.contains {
					if !strings.Contains(status.Convert(err).Message(), fragment) {
						t.Fatalf("error %q does not identify %q", err, fragment)
					}
				}
				return
			}
			if !reflect.DeepEqual(gotBus, test.wantBus) || !reflect.DeepEqual(gotTypes, test.wantTypes) {
				t.Fatalf("subscribedTypes = (%v, %v), want (%v, %v)", gotBus, gotTypes, test.wantBus, test.wantTypes)
			}
		})
	}
}

func TestEventsSubscribeReportsTerminalBusReason(t *testing.T) {
	t.Run("slow consumer must resync", func(t *testing.T) {
		bus := eventbus.New(zap.NewNop())
		bus.Buffer = 1
		bus.MaxDrops = 1
		defer bus.Close()

		stream := newBlockedEventStream(t)
		svc := &eventsService{bus: bus, logger: zap.NewNop()}
		done := make(chan error, 1)
		go func() { done <- svc.Subscribe(&voicxv1.SubscribeEventsRequest{}, stream) }()
		waitForSubscribers(t, bus, 1)

		// The first event leaves Subscribe blocked in Send. The one-slot
		// subscription then fills, and the next event deterministically invokes
		// the slow-consumer eviction path.
		bus.Publish("user_joined", []byte(`{"client_id":"first"}`))
		select {
		case <-stream.sendStarted:
		case <-time.After(3 * time.Second):
			t.Fatal("Subscribe did not reach blocking Send")
		}
		bus.Publish("user_joined", []byte(`{"client_id":"queued"}`))
		bus.Publish("user_joined", []byte(`{"client_id":"dropped"}`))
		waitForSubscribers(t, bus, 0)

		close(stream.release)
		select {
		case err := <-done:
			if got := status.Code(err); got != codes.ResourceExhausted {
				t.Fatalf("slow consumer status = %v, want ResourceExhausted", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Subscribe did not finish after eviction")
		}
	})

	t.Run("bus shutdown is unavailable", func(t *testing.T) {
		bus := eventbus.New(zap.NewNop())
		stream := newBlockedEventStream(t)
		svc := &eventsService{bus: bus, logger: zap.NewNop()}
		done := make(chan error, 1)
		go func() { done <- svc.Subscribe(&voicxv1.SubscribeEventsRequest{}, stream) }()
		waitForSubscribers(t, bus, 1)
		bus.Close()
		select {
		case err := <-done:
			if got := status.Code(err); got != codes.Unavailable {
				t.Fatalf("bus shutdown status = %v, want Unavailable", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Subscribe did not finish after bus shutdown")
		}
	})
}

func TestEventsSubscribeLogsMalformedPayloadAndContinues(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &recordingEventStream{ctx: ctx, sent: make(chan *voicxv1.Event, 1)}
	svc := &eventsService{bus: bus, logger: zap.New(core)}
	done := make(chan error, 1)
	go func() { done <- svc.Subscribe(&voicxv1.SubscribeEventsRequest{}, stream) }()
	waitForSubscribers(t, bus, 1)

	// The decoder must not log the raw payload; use a marker that would make a
	// leak obvious. The type is one the subscription accepts, so the malformed
	// payload reaches the decoder.
	bus.Publish("user_joined", []byte(`{"secret_marker":`))
	bus.Publish("user_joined", []byte(`{"client_id":"valid"}`))
	select {
	case got := <-stream.sent:
		if got.GetUserJoined().GetUserId() != "valid" {
			t.Fatalf("continued event = %+v, want valid user_joined", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("valid event was not delivered after malformed payload")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Subscribe after cancellation = %v", err)
	}

	entries := logs.All()
	if len(entries) != 1 || entries[0].Message != "dropping malformed eventbus payload" {
		t.Fatalf("malformed payload logs = %+v", entries)
	}
	fields := map[string]bool{}
	for _, field := range entries[0].Context {
		fields[field.Key] = true
		if field.Key == "event_type" && len(field.String) > maxLoggedEventTypeBytes+len("…") {
			t.Fatalf("event_type log field is unbounded: %d bytes", len(field.String))
		}
		if strings.Contains(field.String, "secret_marker") {
			t.Fatalf("log field %q leaked raw payload", field.Key)
		}
	}
	if !reflect.DeepEqual(fields, map[string]bool{"event_type": true, "sequence": true, "error": true}) {
		t.Fatalf("malformed payload log fields = %v", fields)
	}
	if got := boundedEventType(strings.Repeat("event-", 20)); len(got) > maxLoggedEventTypeBytes+len("…") {
		t.Fatalf("bounded event type = %d bytes, want at most %d", len(got), maxLoggedEventTypeBytes+len("…"))
	}
}

func TestToProtoEventPayloadContracts(t *testing.T) {
	for _, test := range []struct {
		name  string
		evt   eventbus.Event
		check func(*testing.T, *voicxv1.Event)
	}{
		{
			name: "move source destination and actor",
			evt:  eventbus.Event{Type: "user_moved", Data: []byte(`{"client_id":"session-1","from_channel_id":4,"channel_id":9,"by_client_id":"admin-session"}`)},
			check: func(t *testing.T, event *voicxv1.Event) {
				moved := event.GetUserMoved()
				if moved.GetUserId() != "session-1" || moved.GetFromChannelId() != "4" || moved.GetToChannelId() != "9" || moved.GetMovedBy() != "admin-session" {
					t.Fatalf("move payload = %+v", moved)
				}
			},
		},
		{
			name: "speaking channel",
			evt:  eventbus.Event{Type: "speaking_changed", Data: []byte(`{"client_id":"session-1","channel_id":9,"speaking":true}`)},
			check: func(t *testing.T, event *voicxv1.Event) {
				speaking := event.GetUserSpeaking()
				if speaking.GetUserId() != "session-1" || speaking.GetChannelId() != "9" || !speaking.GetSpeaking() {
					t.Fatalf("speaking payload = %+v", speaking)
				}
			},
		},
		{
			name: "kicked channel",
			evt:  eventbus.Event{Type: "kicked", Data: []byte(`{"client_id":"session-1","channel_id":9,"by_client_id":"admin-session"}`)},
			check: func(t *testing.T, event *voicxv1.Event) {
				kicked := event.GetUserKicked()
				if kicked.GetUserId() != "session-1" || kicked.GetChannelId() != "9" || kicked.GetKickedBy() != "admin-session" {
					t.Fatalf("kicked payload = %+v", kicked)
				}
			},
		},
		{
			name: "ban expiry and channel",
			evt:  eventbus.Event{Type: "kicked", Data: []byte(`{"client_id":"session-1","channel_id":9,"by_client_id":"admin-session","ban":true,"expires_at":1234}`)},
			check: func(t *testing.T, event *voicxv1.Event) {
				banned := event.GetUserBanned()
				if banned.GetUserId() != "session-1" || banned.GetChannelId() != "9" || banned.GetExpiresAt() != 1234 || banned.GetBannedBy() != "admin-session" {
					t.Fatalf("banned payload = %+v", banned)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			event, err := toProto(test.evt)
			if err != nil || event == nil {
				t.Fatalf("toProto = (%+v, %v)", event, err)
			}
			test.check(t, event)
		})
	}
}

type blockedEventStream struct {
	ctx         context.Context
	sendStarted chan struct{}
	release     chan struct{}
}

func newBlockedEventStream(t *testing.T) *blockedEventStream {
	t.Helper()
	return &blockedEventStream{
		ctx:         context.Background(),
		sendStarted: make(chan struct{}, 1),
		release:     make(chan struct{}),
	}
}

func (s *blockedEventStream) Context() context.Context     { return s.ctx }
func (s *blockedEventStream) SetHeader(metadata.MD) error  { return nil }
func (s *blockedEventStream) SendHeader(metadata.MD) error { return nil }
func (s *blockedEventStream) SetTrailer(metadata.MD)       {}
func (s *blockedEventStream) SendMsg(any) error            { return nil }
func (s *blockedEventStream) RecvMsg(any) error            { return io.EOF }
func (s *blockedEventStream) Send(*voicxv1.Event) error {
	select {
	case s.sendStarted <- struct{}{}:
	default:
	}
	<-s.release
	return nil
}

type recordingEventStream struct {
	ctx  context.Context
	sent chan *voicxv1.Event
}

func (s *recordingEventStream) Context() context.Context     { return s.ctx }
func (s *recordingEventStream) SetHeader(metadata.MD) error  { return nil }
func (s *recordingEventStream) SendHeader(metadata.MD) error { return nil }
func (s *recordingEventStream) SetTrailer(metadata.MD)       {}
func (s *recordingEventStream) SendMsg(any) error            { return nil }
func (s *recordingEventStream) RecvMsg(any) error            { return io.EOF }
func (s *recordingEventStream) Send(event *voicxv1.Event) error {
	s.sent <- event
	return nil
}

// withAuth attaches admin credentials to an existing context.
func withAuth(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization",
		"Basic "+base64.StdEncoding.EncodeToString([]byte("admin-uid:pw")))
}

// waitForSubscribers waits until the bus reports n subscribers.
func waitForSubscribers(t *testing.T, bus *eventbus.Bus, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if bus.Stats().Subscribers == n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("subscriber count = %d, want %d", bus.Stats().Subscribers, n)
}

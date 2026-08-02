package grpcserver

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/metadata"

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

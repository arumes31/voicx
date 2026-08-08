package eventbus

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/net/websocket"
)

// testAuth accepts one admin account and one non-admin account.
func testAuth(_ context.Context, uniqueID, password string) (bool, bool, error) {
	switch {
	case uniqueID == "boom":
		return false, false, errors.New("backend down")
	case uniqueID == "admin-uid" && password == "pw":
		return true, true, nil
	case uniqueID == "user-uid" && password == "pw":
		return true, false, nil
	default:
		return false, false, nil
	}
}

// startWS serves the event stream and returns its ws:// URL.
func startWS(t *testing.T, bus *Bus) string {
	t.Helper()
	srv := httptest.NewServer(Handler(bus, testAuth, zap.NewNop()))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// dialWS opens an authenticated event-stream connection.
func dialWS(t *testing.T, url, user, password string) *websocket.Conn {
	t.Helper()
	origin := "http" + strings.TrimPrefix(url, "ws")
	cfg, err := websocket.NewConfig(url, origin)
	if err != nil {
		t.Fatalf("ws config: %v", err)
	}
	cfg.Header.Set("Authorization", "Basic "+
		base64.StdEncoding.EncodeToString([]byte(user+":"+password)))
	conn, err := websocket.DialConfig(cfg)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// readEvent reads one streamed event.
func readEvent(t *testing.T, conn *websocket.Conn) wsEvent {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var raw string
	if err := websocket.Message.Receive(conn, &raw); err != nil {
		t.Fatalf("receive: %v", err)
	}
	var evt wsEvent
	if err := json.Unmarshal([]byte(raw), &evt); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return evt
}

// TestWSStreamsEvents verifies an authenticated bot receives published events
// with their payload intact (231).
func TestWSStreamsEvents(t *testing.T) {
	bus := New(zap.NewNop())
	defer bus.Close()
	conn := dialWS(t, startWS(t, bus), "admin-uid", "pw")

	waitForSubscribers(t, bus, 1)
	bus.Publish("user_joined", []byte(`{"client_id":"c-1"}`))

	evt := readEvent(t, conn)
	if evt.Type != "user_joined" || evt.Seq != 1 {
		t.Fatalf("event = %+v", evt)
	}
	if string(evt.Data) != `{"client_id":"c-1"}` {
		t.Fatalf("payload = %s", evt.Data)
	}
	if evt.Time == "" {
		t.Fatal("event carries no timestamp")
	}
}

// TestWSTypeFilter verifies the ?types= filter (231).
func TestWSTypeFilter(t *testing.T) {
	bus := New(zap.NewNop())
	defer bus.Close()
	conn := dialWS(t, startWS(t, bus)+"?types=user_left", "admin-uid", "pw")

	waitForSubscribers(t, bus, 1)
	bus.Publish("user_joined", []byte(`{}`))
	bus.Publish("user_left", []byte(`{}`))

	if evt := readEvent(t, conn); evt.Type != "user_left" {
		t.Fatalf("filtered stream delivered %q", evt.Type)
	}
}

// TestWSRequiresAdminCredentials verifies the stream is not an anonymous
// presence firehose (231).
func TestWSRequiresAdminCredentials(t *testing.T) {
	bus := New(zap.NewNop())
	defer bus.Close()
	url := startWS(t, bus)
	httpURL := "http" + strings.TrimPrefix(url, "ws")

	for _, tc := range []struct {
		name       string
		user, pass string
		basic      bool
		wantStatus int
	}{
		{"no credentials", "", "", false, http.StatusUnauthorized},
		{"wrong password", "admin-uid", "nope", true, http.StatusUnauthorized},
		{"not an admin", "user-uid", "pw", true, http.StatusUnauthorized},
		{"backend error", "boom", "pw", true, http.StatusInternalServerError},
	} {
		req, err := http.NewRequest(http.MethodGet, httpURL, nil)
		if err != nil {
			t.Fatalf("%s: request: %v", tc.name, err)
		}
		if tc.basic {
			req.SetBasicAuth(tc.user, tc.pass)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.wantStatus {
			t.Fatalf("%s: status = %d, want %d", tc.name, resp.StatusCode, tc.wantStatus)
		}
	}
	if got := bus.Stats().Subscribers; got != 0 {
		t.Fatalf("refused logins left %d subscribers", got)
	}
}

// TestWSUnsubscribesOnDisconnect verifies a hung-up bot releases its slot
// (231).
func TestWSUnsubscribesOnDisconnect(t *testing.T) {
	bus := New(zap.NewNop())
	defer bus.Close()
	conn := dialWS(t, startWS(t, bus), "admin-uid", "pw")
	waitForSubscribers(t, bus, 1)
	_ = conn.Close()
	waitForSubscribers(t, bus, 0)
}

// waitForSubscribers waits until the bus reports n subscribers.
func waitForSubscribers(t *testing.T, bus *Bus, n int) {
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

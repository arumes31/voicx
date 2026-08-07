package eventbus

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/net/websocket"
)

func testWSHandlerConfig(now func() time.Time) wsHandlerConfig {
	return wsHandlerConfig{
		maxConnections:  2,
		maxAuthAttempts: 4,
		authWindow:      time.Minute,
		maxAuthBuckets:  8,
		now:             now,
	}
}

func TestWSHandlerRejectsUnsafeRequestsBeforeAuthentication(t *testing.T) {
	bus := New(zap.NewNop())
	defer bus.Close()
	var authCalls atomic.Int64
	auth := func(context.Context, string, string) (bool, bool, error) {
		authCalls.Add(1)
		return true, true, nil
	}
	handler := Handler(bus, auth, zap.NewNop())

	for _, tc := range []struct {
		name       string
		request    *http.Request
		wantStatus int
	}{
		{
			name:       "wrong method",
			request:    httptest.NewRequest(http.MethodPost, "http://localhost/events", nil),
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name: "oversized authorization",
			request: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "http://localhost/events", nil)
				r.Header.Set("Authorization", strings.Repeat("x", maxBasicHeaderSize+1))
				return r
			}(),
			wantStatus: http.StatusRequestHeaderFieldsTooLarge,
		},
		{
			name: "oversized query",
			request: httptest.NewRequest(
				http.MethodGet,
				"http://localhost/events?types="+strings.Repeat("x", maxRawQuerySize+1),
				nil,
			),
			wantStatus: http.StatusRequestURITooLong,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, tc.request)
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.wantStatus)
			}
			if recorder.Header().Get("Cache-Control") != "no-store" ||
				recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("security headers = %v", recorder.Header())
			}
		})
	}
	if authCalls.Load() != 0 {
		t.Fatalf("authentication calls = %d, want 0", authCalls.Load())
	}
}

func TestWSHandlerFailsClosedWithoutDependencies(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://localhost/events", nil)
	request.SetBasicAuth("admin-uid", "pw")
	bus := New(zap.NewNop())
	defer bus.Close()
	for _, handler := range []http.Handler{
		Handler(nil, testAuth, zap.NewNop()),
		Handler(bus, nil, zap.NewNop()),
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request.Clone(request.Context()))
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", recorder.Code)
		}
	}
}

func TestParseTypesIsBoundedAndDeduplicated(t *testing.T) {
	got, err := parseTypes(" joined, left,joined ,, left ")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "joined,left" {
		t.Fatalf("filters = %q", got)
	}
	manyTypes := make([]string, maxTypeFilters+1)
	for i := range manyTypes {
		manyTypes[i] = "type" + strconv.Itoa(i)
	}
	for _, raw := range []string{
		strings.Repeat("x", maxTypeQuerySize+1),
		strings.Repeat("x", maxTypeSize+1),
		"valid,has\ncontrol",
		strings.Join(manyTypes, ","),
		string([]byte{0xff}),
	} {
		if _, err := parseTypes(raw); err == nil {
			t.Fatalf("parseTypes(%q) succeeded", raw)
		}
	}
}

func TestRequestTypesRejectsAmbiguousAndMalformedQueries(t *testing.T) {
	for _, raw := range []string{
		"types=joined&types=left",
		"types=%zz",
	} {
		if _, err := requestTypes(raw); err == nil {
			t.Fatalf("requestTypes(%q) succeeded", raw)
		}
	}
}

func TestWSAuthenticatedPlainHTTPRequestDoesNotPanic(t *testing.T) {
	bus := New(zap.NewNop())
	defer bus.Close()
	request := httptest.NewRequest(http.MethodGet, "http://localhost/events", nil)
	request.SetBasicAuth("admin-uid", "pw")
	recorder := httptest.NewRecorder()
	Handler(bus, testAuth, zap.NewNop()).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}

func TestWSAuthLimiterBoundsAttemptsAndBuckets(t *testing.T) {
	now := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	cfg := testWSHandlerConfig(func() time.Time { return now })
	cfg.maxAuthAttempts = 2
	cfg.maxAuthBuckets = 1
	limiter := newWSAuthLimiter(cfg)

	if ok, _ := limiter.allow("127.0.0.1:1000"); !ok {
		t.Fatal("first attempt denied")
	}
	if ok, _ := limiter.allow("[::ffff:127.0.0.1]:1001"); !ok {
		t.Fatal("IPv4-mapped second attempt denied")
	}
	if ok, wait := limiter.allow("127.0.0.1:1002"); ok || wait != time.Minute {
		t.Fatalf("third attempt = %v/%v, want denied/1m", ok, wait)
	}
	if ok, _ := limiter.allow("192.0.2.1:1000"); ok {
		t.Fatal("new address admitted after bucket bound")
	}

	now = now.Add(time.Minute)
	if ok, _ := limiter.allow("192.0.2.1:1000"); !ok {
		t.Fatal("expired bucket was not pruned")
	}
}

func TestWSHandlerRateLimitsAuthentication(t *testing.T) {
	bus := New(zap.NewNop())
	defer bus.Close()
	cfg := testWSHandlerConfig(time.Now)
	cfg.maxAuthAttempts = 2
	handler := newWSHandler(bus, testAuth, zap.NewNop(), cfg)

	for attempt := 1; attempt <= 3; attempt++ {
		request := httptest.NewRequest(http.MethodGet, "http://localhost/events", nil)
		request.RemoteAddr = "192.0.2.15:4321"
		request.SetBasicAuth("admin-uid", "wrong")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		want := http.StatusUnauthorized
		if attempt == 3 {
			want = http.StatusTooManyRequests
		}
		if recorder.Code != want {
			t.Fatalf("attempt %d status = %d, want %d", attempt, recorder.Code, want)
		}
		if attempt == 3 && recorder.Header().Get("Retry-After") == "" {
			t.Fatal("rate-limited response has no Retry-After")
		}
	}
}

func TestWSRejectsCrossOriginHandshake(t *testing.T) {
	bus := New(zap.NewNop())
	defer bus.Close()
	server := httptest.NewServer(Handler(bus, testAuth, zap.NewNop()))
	defer server.Close()

	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	config, err := websocket.NewConfig(endpoint, "https://attacker.example")
	if err != nil {
		t.Fatal(err)
	}
	config.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("admin-uid:pw")))
	if conn, err := websocket.DialConfig(config); err == nil {
		_ = conn.Close()
		t.Fatal("cross-origin websocket handshake succeeded")
	}
}

func TestWSAllowsBotWithoutOrigin(t *testing.T) {
	bus := New(zap.NewNop())
	defer bus.Close()
	server := httptest.NewServer(Handler(bus, testAuth, zap.NewNop()))
	defer server.Close()

	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	authorization := base64.StdEncoding.EncodeToString([]byte("admin-uid:pw"))
	request := "GET / HTTP/1.1\r\n" +
		"Host: " + server.Listener.Addr().String() + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Authorization: Basic " + authorization + "\r\n\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read originless handshake: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("originless handshake status = %d, want 101", response.StatusCode)
	}
}

func TestWSLimitsConcurrentStreamsAndIncomingFrames(t *testing.T) {
	bus := New(zap.NewNop())
	defer bus.Close()
	cfg := testWSHandlerConfig(time.Now)
	cfg.maxConnections = 1
	cfg.maxAuthAttempts = 10
	server := httptest.NewServer(newWSHandler(bus, testAuth, zap.NewNop(), cfg))
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")

	first := dialWS(t, endpoint, "admin-uid", "pw")
	waitForSubscribers(t, bus, 1)
	config, err := websocket.NewConfig(endpoint, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	config.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("admin-uid:pw")))
	if conn, err := websocket.DialConfig(config); err == nil {
		_ = conn.Close()
		t.Fatal("second websocket stream exceeded connection limit")
	}

	if err := websocket.Message.Send(first, strings.Repeat("x", wsMaxIncomingBytes+1)); err != nil {
		t.Fatalf("send oversized frame: %v", err)
	}
	waitForSubscribers(t, bus, 0)
}

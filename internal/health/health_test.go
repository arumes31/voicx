package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// TestHealthz verifies the liveness endpoint always returns 200.
func TestHealthz(t *testing.T) {
	srv := New("127.0.0.1:0", nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("/healthz Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("/healthz X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestHealthzDisclosesVersionOnlyToLoopback(t *testing.T) {
	srv := New("127.0.0.1:0", nil, nil)
	for _, test := range []struct {
		name        string
		remoteAddr  string
		wantVersion bool
	}{
		{name: "IPv4 loopback", remoteAddr: "127.0.0.1:1234", wantVersion: true},
		{name: "IPv6 loopback", remoteAddr: "[::1]:1234", wantVersion: true},
		{name: "remote IPv4", remoteAddr: "192.0.2.10:1234"},
		{name: "remote IPv6", remoteAddr: "[2001:db8::1]:1234"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
			request.RemoteAddr = test.remoteAddr
			srv.Handler().ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", recorder.Code)
			}
			var body map[string]string
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body["status"] != "ok" {
				t.Fatalf("status body = %q, want ok", body["status"])
			}
			_, gotVersion := body["version"]
			if gotVersion != test.wantVersion {
				t.Fatalf("version present = %t, want %t", gotVersion, test.wantVersion)
			}
		})
	}
}

// TestReadyz verifies the readiness endpoint reflects the probe result.
func TestReadyz(t *testing.T) {
	probeErr := error(nil)
	srv := New("127.0.0.1:0", nil, func(context.Context) error { return probeErr })
	handler := srv.Handler()

	// Probe succeeds -> 200.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz (healthy) status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("/readyz (healthy) Content-Type = %q", got)
	}

	// Probe fails -> 503, which tells orchestrators the dependency is
	// temporarily unavailable rather than reporting an application bug.
	probeErr = errors.New("database unreachable")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz (unhealthy) status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("/readyz (unhealthy) Content-Type = %q", got)
	}
}

// TestReadyzNilProbe verifies /readyz reports ready when no probe is wired.
func TestReadyzNilProbe(t *testing.T) {
	srv := New("127.0.0.1:0", nil, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz (nil probe) status = %d, want 200", rec.Code)
	}
}

func TestHealthEndpointsRequireGET(t *testing.T) {
	srv := New("127.0.0.1:0", nil, nil)
	for _, path := range []string{"/healthz", "/readyz"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			srv.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, nil))
			if recorder.Code != http.StatusMethodNotAllowed {
				t.Fatalf("POST %s status = %d, want 405", path, recorder.Code)
			}
			if allow := recorder.Header().Get("Allow"); allow != http.MethodGet {
				t.Fatalf("POST %s Allow = %q, want GET", path, allow)
			}
		})
	}
}

func TestServerHTTPTimeouts(t *testing.T) {
	srv := New("127.0.0.1:0", nil, nil)
	if srv.srv.ReadHeaderTimeout != readTimeout || srv.srv.ReadTimeout != readTimeout {
		t.Fatalf(
			"read timeouts = %s/%s, want %s",
			srv.srv.ReadHeaderTimeout,
			srv.srv.ReadTimeout,
			readTimeout,
		)
	}
	if srv.srv.WriteTimeout != writeTimeout || srv.srv.IdleTimeout != idleTimeout {
		t.Fatalf(
			"write/idle timeouts = %s/%s, want %s/%s",
			srv.srv.WriteTimeout,
			srv.srv.IdleTimeout,
			writeTimeout,
			idleTimeout,
		)
	}
	if srv.srv.MaxHeaderBytes != maxHeaders {
		t.Fatalf("MaxHeaderBytes = %d, want %d", srv.srv.MaxHeaderBytes, maxHeaders)
	}
}

func TestHandleLocalGETRestrictsMethodAndRemoteAddress(t *testing.T) {
	srv := New("127.0.0.1:0", nil, nil)
	srv.HandleLocalGET("/metrics", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name       string
		method     string
		remoteAddr string
		wantStatus int
	}{
		{name: "IPv4 loopback", method: http.MethodGet, remoteAddr: "127.0.0.1:1234", wantStatus: http.StatusOK},
		{name: "IPv6 loopback", method: http.MethodGet, remoteAddr: "[::1]:1234", wantStatus: http.StatusOK},
		{name: "IPv6 scoped loopback", method: http.MethodGet, remoteAddr: "[::1%lo0]:1234", wantStatus: http.StatusOK},
		{name: "IPv4 mapped loopback", method: http.MethodGet, remoteAddr: "[::ffff:127.0.0.1]:1234", wantStatus: http.StatusOK},
		{name: "bare loopback", method: http.MethodGet, remoteAddr: "127.0.0.1", wantStatus: http.StatusOK},
		{name: "remote", method: http.MethodGet, remoteAddr: "192.0.2.10:1234", wantStatus: http.StatusForbidden},
		{name: "hostname is not trusted", method: http.MethodGet, remoteAddr: "localhost:1234", wantStatus: http.StatusForbidden},
		{name: "malformed remote", method: http.MethodGet, remoteAddr: "not an address", wantStatus: http.StatusForbidden},
		{name: "non GET", method: http.MethodPost, remoteAddr: "127.0.0.1:1234", wantStatus: http.StatusMethodNotAllowed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequestWithContext(t.Context(), test.method, "/metrics", nil)
			request.RemoteAddr = test.remoteAddr
			recorder := httptest.NewRecorder()
			srv.Handler().ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, test.wantStatus)
			}
		})
	}
}

func TestHandleGETAllowsExplicitRemoteAccess(t *testing.T) {
	srv := New("127.0.0.1:0", nil, nil)
	srv.HandleGET("/metrics", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
	request.RemoteAddr = "192.0.2.10:1234"
	recorder := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("remote GET status = %d, want 200", recorder.Code)
	}
}

func TestSchemaVersionHandlerMethodAndLoopbackRestriction(t *testing.T) {
	handler := SchemaVersionHandler(nil, func(context.Context) (string, error) { return "022", nil })
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/schema/version", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST status/Allow = %d/%q", recorder.Code, recorder.Header().Get("Allow"))
	}

	request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/schema/version", nil)
	request.RemoteAddr = "192.0.2.10:1234"
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("remote GET status = %d, want 403", recorder.Code)
	}

	request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/schema/version", nil)
	request.RemoteAddr = "[::1]:1234"
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("loopback GET status = %d, want 200", recorder.Code)
	}
}

func TestSchemaVersionHandlerLogsButDoesNotDiscloseProbeFailure(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	secret := errors.New("postgres password=top-secret")
	handler := SchemaVersionHandler(zap.New(core), func(context.Context) (string, error) {
		return "", secret
	})
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/schema/version", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	if body := recorder.Body.String(); strings.Contains(body, secret.Error()) || body != "service unavailable\n" {
		t.Fatalf("failure body = %q, want generic service unavailable", body)
	}
	if logs.Len() != 1 || logs.All()[0].Level != zap.WarnLevel {
		t.Fatalf("logged warnings = %+v, want exactly one warning", logs.All())
	}
}

func TestSchemaVersionHandlerTreatsNilProbeAsUnavailable(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	handler := SchemaVersionHandler(zap.New(core), nil)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/schema/version", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable || recorder.Body.String() != "service unavailable\n" {
		t.Fatalf("nil-probe status/body = %d/%q", recorder.Code, recorder.Body.String())
	}
	if logs.Len() != 1 || logs.All()[0].Level != zap.WarnLevel {
		t.Fatalf("nil-probe logs = %+v, want one warning", logs.All())
	}
}

package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHealthz verifies the liveness endpoint always returns 200.
func TestHealthz(t *testing.T) {
	srv := New("127.0.0.1:0", nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", rec.Code)
	}
}

// TestReadyz verifies the readiness endpoint reflects the probe result.
func TestReadyz(t *testing.T) {
	probeErr := error(nil)
	srv := New("127.0.0.1:0", nil, func(context.Context) error { return probeErr })
	handler := srv.Handler()

	// Probe succeeds -> 200.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz (healthy) status = %d, want 200", rec.Code)
	}

	// Probe fails -> 500.
	probeErr = errors.New("database unreachable")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("/readyz (unhealthy) status = %d, want 500", rec.Code)
	}
}

// TestReadyzNilProbe verifies /readyz reports ready when no probe is wired.
func TestReadyzNilProbe(t *testing.T) {
	srv := New("127.0.0.1:0", nil, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz (nil probe) status = %d, want 200", rec.Code)
	}
}

func TestHealthEndpointsRequireGET(t *testing.T) {
	srv := New("127.0.0.1:0", nil, nil)
	for _, path := range []string{"/healthz", "/readyz"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			srv.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, nil))
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
		{name: "remote", method: http.MethodGet, remoteAddr: "192.0.2.10:1234", wantStatus: http.StatusForbidden},
		{name: "non GET", method: http.MethodPost, remoteAddr: "127.0.0.1:1234", wantStatus: http.StatusMethodNotAllowed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, "/metrics", nil)
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
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	request.RemoteAddr = "192.0.2.10:1234"
	recorder := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("remote GET status = %d, want 200", recorder.Code)
	}
}

func TestSchemaVersionHandlerMethodAndLoopbackRestriction(t *testing.T) {
	handler := SchemaVersionHandler(func(context.Context) (string, error) { return "022", nil })
	request := httptest.NewRequest(http.MethodPost, "/api/v1/schema/version", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST status/Allow = %d/%q", recorder.Code, recorder.Header().Get("Allow"))
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/schema/version", nil)
	request.RemoteAddr = "192.0.2.10:1234"
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("remote GET status = %d, want 403", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/schema/version", nil)
	request.RemoteAddr = "[::1]:1234"
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("loopback GET status = %d, want 200", recorder.Code)
	}
}

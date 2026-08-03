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

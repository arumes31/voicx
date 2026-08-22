package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"voicx/internal/health"
)

func TestPprofEndpointsAreOptInAndLoopbackOnly(t *testing.T) {
	newServer := func(enabled bool) *health.Server {
		srv := health.New("127.0.0.1:0", nil, nil)
		registerPprofEndpoints(srv, enabled)
		return srv
	}

	disabled := newServer(false)
	disabledRequest := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/debug/pprof/", nil)
	disabledRequest.RemoteAddr = "127.0.0.1:1234"
	disabledRecorder := httptest.NewRecorder()
	disabled.Handler().ServeHTTP(disabledRecorder, disabledRequest)
	if disabledRecorder.Code != http.StatusNotFound {
		t.Fatalf("disabled pprof status = %d, want 404", disabledRecorder.Code)
	}

	enabled := newServer(true)
	for _, path := range []string{
		"/debug/pprof/",
		"/debug/pprof/goroutine?debug=1",
		"/debug/pprof/cmdline",
	} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
			request.RemoteAddr = "127.0.0.1:1234"
			recorder := httptest.NewRecorder()
			enabled.Handler().ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("local GET %s status = %d, want 200", path, recorder.Code)
			}
		})
	}

	remote := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/debug/pprof/", nil)
	remote.RemoteAddr = "192.0.2.10:1234"
	remote.Header.Set("X-Forwarded-For", "127.0.0.1")
	remote.Header.Set("Forwarded", "for=127.0.0.1")
	remoteRecorder := httptest.NewRecorder()
	enabled.Handler().ServeHTTP(remoteRecorder, remote)
	if remoteRecorder.Code != http.StatusForbidden {
		t.Fatalf("spoofed forwarded loopback status = %d, want 403", remoteRecorder.Code)
	}

	post := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/debug/pprof/", nil)
	post.RemoteAddr = "127.0.0.1:1234"
	postRecorder := httptest.NewRecorder()
	enabled.Handler().ServeHTTP(postRecorder, post)
	if postRecorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("local POST status = %d, want 405", postRecorder.Code)
	}
}

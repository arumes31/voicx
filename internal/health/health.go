// Package health provides a minimal HTTP health/readiness endpoint for the
// voicx server. It uses only the standard library: /healthz reports process
// liveness (always 200 once serving) and /readyz reports readiness by
// invoking a caller-supplied probe (typically a Postgres ping).
package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"go.uber.org/zap"

	"voicx/internal/version"
)

const (
	// readyTimeout bounds how long /readyz waits for the readiness probe.
	readyTimeout = 3 * time.Second
	readTimeout  = 5 * time.Second
	writeTimeout = 10 * time.Second
	idleTimeout  = 60 * time.Second
	maxHeaders   = 1 << 20
)

// Server exposes /healthz and /readyz over HTTP.
type Server struct {
	logger *zap.Logger
	mux    *http.ServeMux
	srv    *http.Server
}

// SchemaVersionHandler reports the newest successfully applied migration.
// The callback keeps the health package independent from a database driver.
func SchemaVersionHandler(probe func(context.Context) (string, error)) http.Handler {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()
		version, err := probe(ctx)
		if err != nil {
			http.Error(w, "schema version unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"version": version})
	})
	return localGET(handler)
}

// New constructs a Server that will listen on addr. ready is the readiness
// probe invoked by /readyz; a nil return means ready. ready may be nil, in
// which case /readyz always reports ready.
func New(addr string, logger *zap.Logger, ready func(ctx context.Context) error) *Server {
	if logger == nil {
		logger = zap.NewNop()
	}
	s := &Server{logger: logger}

	mux := http.NewServeMux()
	mux.Handle("/healthz", getOnly(http.HandlerFunc(s.handleHealthz)))
	mux.Handle("/readyz", getOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleReadyz(w, r, ready)
	})))
	s.mux = mux

	s.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: readTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaders,
	}
	return s
}

// Handle registers an additional handler on the server's mux (e.g. /metrics).
func (s *Server) Handle(pattern string, h http.Handler) {
	s.mux.Handle(pattern, h)
}

// HandleGET registers a GET-only endpoint. It is suitable for an endpoint
// that has its own authentication or is deliberately exposed remotely.
func (s *Server) HandleGET(pattern string, h http.Handler) {
	s.Handle(pattern, getOnly(h))
}

// HandleLocalGET registers a GET-only endpoint that accepts direct loopback
// callers only. Forwarded headers are intentionally ignored.
func (s *Server) HandleLocalGET(pattern string, h http.Handler) {
	s.Handle(pattern, localGET(h))
}

// Handler returns the server's HTTP handler, primarily for tests.
func (s *Server) Handler() http.Handler {
	return s.srv.Handler
}

// Start serves HTTP until Shutdown is called or the listener fails. A clean
// shutdown returns nil.
func (s *Server) Start() error {
	s.logger.Info("health listener started", zap.String("addr", s.srv.Addr))
	if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown gracefully stops the server, honoring the context deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

// handleHealthz reports liveness: the process is up and serving. It returns
// a small JSON body carrying the embedded version.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `{"status":"ok","version":%q}`+"\n", version.String())
}

// handleReadyz reports readiness: 200 when the probe succeeds, 500 otherwise.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request, ready func(ctx context.Context) error) {
	if ready != nil {
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()
		if err := ready(ctx); err != nil {
			s.logger.Warn("readiness probe failed", zap.Error(err))
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("not ready\n"))
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func getOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func localGET(next http.Handler) http.Handler {
	return getOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !remoteIsLoopback(r.RemoteAddr) {
			http.Error(w, "endpoint is available on loopback only", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func remoteIsLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

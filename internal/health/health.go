// Package health provides a minimal HTTP health/readiness endpoint for the
// voicx server. It uses only the standard library: /healthz reports process
// liveness (always 200 once serving) and /readyz reports readiness by
// invoking a caller-supplied probe (typically a Postgres ping).
package health

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"voicx/internal/version"
)

// readyTimeout bounds how long /readyz waits for the readiness probe.
const readyTimeout = 3 * time.Second

// Server exposes /healthz and /readyz over HTTP.
type Server struct {
	logger *zap.Logger
	mux    *http.ServeMux
	srv    *http.Server
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
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		s.handleReadyz(w, r, ready)
	})
	s.mux = mux

	s.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// Handle registers an additional handler on the server's mux (e.g. /metrics).
func (s *Server) Handle(pattern string, h http.Handler) {
	s.mux.Handle(pattern, h)
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
	fmt.Fprintf(w, `{"status":"ok","version":%q}`+"\n", version.String())
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

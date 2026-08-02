// Package grpcserver implements the gRPC API declared in proto/ (232). The
// generated stubs live in voicx/v1 (regenerate with `buf generate`).
//
// The API is a bot/administration surface, not a second client protocol: it
// talks to the ServerQuery backend, so anything it can do the query port can
// do, with the same admin-only credentials. Every RPC except Control's own
// Authenticate requires HTTP Basic credentials in the "authorization"
// metadata header, checked by the interceptors below.
package grpcserver

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strings"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"voicx/internal/eventbus"
	"voicx/internal/query"
	voicxv1 "voicx/v1"
)

// Server owns the gRPC listener and the service implementations.
type Server struct {
	Addr string

	backend query.Backend
	bus     *eventbus.Bus
	logger  *zap.Logger
	grpc    *grpc.Server
}

// New constructs a gRPC server bound to the ServerQuery backend and the event
// bus.
func New(addr string, backend query.Backend, bus *eventbus.Bus, logger *zap.Logger) *Server {
	if logger == nil {
		logger = zap.NewNop()
	}
	s := &Server{Addr: addr, backend: backend, bus: bus, logger: logger}
	s.grpc = grpc.NewServer(
		grpc.UnaryInterceptor(s.unaryAuth),
		grpc.StreamInterceptor(s.streamAuth),
	)
	voicxv1.RegisterEventsServer(s.grpc, &eventsService{bus: bus, logger: logger})
	voicxv1.RegisterControlServer(s.grpc, &controlService{backend: backend, logger: logger})
	return s
}

// Start serves until ctx is cancelled or Stop is called.
func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("grpc listen on %s: %w", s.Addr, err)
	}
	s.logger.Info("gRPC listener started", zap.String("addr", s.Addr))

	go func() {
		<-ctx.Done()
		s.grpc.GracefulStop()
	}()
	if err := s.grpc.Serve(ln); err != nil && !strings.Contains(err.Error(), "use of closed") {
		return fmt.Errorf("grpc serve: %w", err)
	}
	return nil
}

// Stop stops the server without waiting for in-flight streams. Event
// subscribers are long-lived, so a graceful stop would block on them.
func (s *Server) Stop() { s.grpc.Stop() }

// authExempt lists the RPCs that carry their own credentials.
var authExempt = map[string]bool{
	"/voicx.v1.Control/Authenticate": true,
}

// authenticate validates the "authorization: Basic <base64>" metadata header
// against the same admin-only credentials as ServerQuery.
func (s *Server) authenticate(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "missing metadata")
	}
	values := md.Get("authorization")
	if len(values) == 0 {
		return "", status.Error(codes.Unauthenticated, "missing authorization metadata")
	}
	uniqueID, password, err := parseBasic(values[0])
	if err != nil {
		return "", status.Error(codes.Unauthenticated, err.Error())
	}
	valid, admin, err := s.backend.Authenticate(ctx, uniqueID, password)
	if err != nil {
		s.logger.Warn("grpc auth error", zap.Error(err))
		return "", status.Error(codes.Internal, "internal error")
	}
	if !valid || !admin {
		// Non-admins are refused like bad credentials: the API is admin-only
		// and the distinction would confirm an account exists.
		return "", status.Error(codes.Unauthenticated, "invalid credentials")
	}
	return uniqueID, nil
}

// parseBasic decodes an HTTP Basic credential.
func parseBasic(header string) (string, string, error) {
	const prefix = "Basic "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", "", fmt.Errorf("authorization must be Basic")
	}
	raw, err := base64.StdEncoding.DecodeString(header[len(prefix):])
	if err != nil {
		return "", "", fmt.Errorf("malformed Basic credential")
	}
	user, password, ok := strings.Cut(string(raw), ":")
	if !ok {
		return "", "", fmt.Errorf("malformed Basic credential")
	}
	return user, password, nil
}

// unaryAuth gates unary RPCs.
func (s *Server) unaryAuth(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if authExempt[info.FullMethod] {
		return handler(ctx, req)
	}
	if _, err := s.authenticate(ctx); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

// streamAuth gates streaming RPCs.
func (s *Server) streamAuth(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if authExempt[info.FullMethod] {
		return handler(srv, ss)
	}
	uniqueID, err := s.authenticate(ss.Context())
	if err != nil {
		return err
	}
	return handler(srv, &identifiedStream{ServerStream: ss, uniqueID: uniqueID})
}

// identifiedStream carries the authenticated caller to the handler, which uses
// it to name the subscriber in bus logs and metrics.
type identifiedStream struct {
	grpc.ServerStream
	uniqueID string
}

// callerOf returns the authenticated caller of a stream, if any.
func callerOf(ss grpc.ServerStream) string {
	if is, ok := ss.(*identifiedStream); ok {
		return is.uniqueID
	}
	return "unknown"
}

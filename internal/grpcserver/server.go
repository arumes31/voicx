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
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"voicx/internal/auth"
	"voicx/internal/eventbus"
	"voicx/internal/query"
	voicxv1 "voicx/v1"
)

// Server owns the gRPC listener and the service implementations.
type Server struct {
	Addr string
	// StreamLifetime bounds authenticated streaming RPCs. Set before Start;
	// zero uses the production default.
	StreamLifetime time.Duration
	// ShutdownTimeout bounds the graceful shutdown initiated when Start's
	// context is cancelled. Set it before Start; zero uses the production
	// default.
	ShutdownTimeout time.Duration

	backend query.Backend
	bus     *eventbus.Bus
	logger  *zap.Logger
	grpc    *grpc.Server
	limiter LoginLimiter
	addrErr error
	listen  func(network, address string) (net.Listener, error)

	shutdownOnce sync.Once
	forceOnce    sync.Once
	shutdownDone chan struct{}
	shutdownMu   sync.Mutex
	shutdownErr  error
	draining     atomic.Bool

	watchMu   sync.Mutex
	watchDone chan struct{}
}

const (
	maxBasicAuthorizationBytes = 1024
	maxHeaderListBytes         = 4 * 1024
	maxReceiveMessageBytes     = 1 << 20
	defaultStreamLifetime      = time.Hour
	defaultShutdownTimeout     = 30 * time.Second
	serverKeepaliveTime        = 2 * time.Minute
	serverKeepaliveTimeout     = 20 * time.Second
	minimumClientPingInterval  = 5 * time.Minute
)

// LoginLimiter is the ServerQuery brute-force limiter shared by every admin
// transport. Callers pass a transport-specific scope, never a raw IP alone.
type LoginLimiter interface {
	ReserveLoginAttempt(scopes ...string) (*auth.LoginAttempt, bool)
}

type authFailureRecorder interface {
	RecordAuthFailure(transport, reason string)
}

// New constructs a gRPC server bound to the ServerQuery backend and the event
// bus.
func New(addr string, backend query.Backend, bus *eventbus.Bus, logger *zap.Logger, limiter LoginLimiter) *Server {
	if logger == nil {
		logger = zap.NewNop()
	}
	s := &Server{
		Addr:            addr,
		StreamLifetime:  defaultStreamLifetime,
		ShutdownTimeout: defaultShutdownTimeout,
		backend:         backend,
		bus:             bus,
		logger:          logger,
		limiter:         limiter,
		shutdownDone:    make(chan struct{}),
		listen:          net.Listen,
	}
	s.addrErr = validateLoopbackAddr(addr)
	s.grpc = grpc.NewServer(
		grpc.MaxRecvMsgSize(maxReceiveMessageBytes),
		grpc.MaxHeaderListSize(maxHeaderListBytes),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    serverKeepaliveTime,
			Timeout: serverKeepaliveTimeout,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             minimumClientPingInterval,
			PermitWithoutStream: false,
		}),
		grpc.ChainUnaryInterceptor(s.unaryRecovery, s.unaryAuth),
		grpc.ChainStreamInterceptor(s.streamRecovery, s.streamAuth),
	)
	voicxv1.RegisterEventsServer(s.grpc, &eventsService{bus: bus, logger: logger})
	voicxv1.RegisterControlServer(s.grpc, &controlService{backend: backend, logger: logger, authenticate: s.authenticateAdmin})
	return s
}

// Start serves until the listener exits or ctx is cancelled. It waits for the
// cancellation watcher before returning so no background shutdown survives a
// completed Start call.
func (s *Server) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-s.shutdownDone:
		return s.shutdownResult()
	default:
	}
	if s.addrErr != nil {
		return s.addrErr
	}
	ln, err := s.listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("grpc listen on %s: %w", s.Addr, err)
	}
	s.logger.Info("gRPC listener started", zap.String("addr", s.Addr))

	serveDone := make(chan struct{})
	watchDone := make(chan struct{})
	s.watchMu.Lock()
	s.watchDone = watchDone
	s.watchMu.Unlock()
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownTimeout())
			defer cancel()
			_ = s.Shutdown(shutdownCtx)
		case <-serveDone:
		}
	}()
	err = s.grpc.Serve(ln)
	if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout())
		shutdownErr := s.Shutdown(shutdownCtx)
		cancel()
		close(serveDone)
		<-watchDone
		return errors.Join(fmt.Errorf("grpc serve: %w", err), shutdownErr)
	}
	close(serveDone)
	<-watchDone
	return nil
}

func validateLoopbackAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid grpc address %q: %w", addr, err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("grpc address %q must be loopback-only because the listener is plaintext", addr)
	}
	return nil
}

// Shutdown stops the server exactly once. It first lets active RPCs drain. If
// a caller's context expires, it forces the stop and every later caller sees
// the same terminal error.
func (s *Server) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.shutdownOnce.Do(func() {
		s.draining.Store(true)
		go func() {
			s.grpc.GracefulStop()
			close(s.shutdownDone)
		}()
	})

	select {
	case <-s.shutdownDone:
		return s.shutdownResult()
	case <-ctx.Done():
		// A graceful shutdown may have completed concurrently with the caller's
		// deadline. Do not force a healthy completed shutdown in that race.
		select {
		case <-s.shutdownDone:
			return s.shutdownResult()
		default:
		}
		s.forceOnce.Do(func() {
			s.setShutdownError(ctx.Err())
			s.logger.Warn("gRPC graceful shutdown timed out; forcing stop",
				zap.String("reason", ctx.Err().Error()))
			s.grpc.Stop()
		})
		<-s.shutdownDone
		return s.shutdownResult()
	}
}

func (s *Server) shutdownResult() error {
	s.shutdownMu.Lock()
	defer s.shutdownMu.Unlock()
	return s.shutdownErr
}

func (s *Server) setShutdownError(err error) {
	s.shutdownMu.Lock()
	s.shutdownErr = err
	s.shutdownMu.Unlock()
}

func (s *Server) shutdownTimeout() time.Duration {
	if s.ShutdownTimeout <= 0 {
		return defaultShutdownTimeout
	}
	return s.ShutdownTimeout
}

// authExempt lists the RPCs that carry their own credentials.
var authExempt = map[string]bool{
	"/voicx.v1.Control/Authenticate": true,
}

// authenticate validates the "authorization: Basic <base64>" metadata header
// against the same admin-only credentials as ServerQuery.
func (s *Server) authenticate(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		s.recordAuthFailure("malformed_metadata")
		return "", status.Error(codes.Unauthenticated, "missing metadata")
	}
	values := md.Get("authorization")
	if len(values) == 0 {
		s.recordAuthFailure("malformed_metadata")
		return "", status.Error(codes.Unauthenticated, "missing authorization metadata")
	}
	if len(values) != 1 {
		s.recordAuthFailure("malformed_metadata")
		return "", status.Error(codes.Unauthenticated, "exactly one authorization value is required")
	}
	uniqueID, password, err := parseBasic(values[0])
	if err != nil {
		s.recordAuthFailure("malformed_metadata")
		return "", status.Error(codes.Unauthenticated, err.Error())
	}
	valid, err := s.authenticateAdmin(ctx, remoteIPFromContext(ctx), uniqueID, password)
	if err != nil {
		s.logger.Warn("grpc auth error", zap.Error(err))
		return "", status.Error(codes.Internal, "internal error")
	}
	if !valid {
		// Non-admins are refused like bad credentials: the API is admin-only
		// and the distinction would confirm an account exists.
		return "", status.Error(codes.Unauthenticated, "invalid credentials")
	}
	return uniqueID, nil
}

func (s *Server) authenticateAdmin(ctx context.Context, _ string, uniqueID, password string) (bool, error) {
	if s.limiter == nil {
		return false, fmt.Errorf("login limiter unavailable")
	}
	// The listener is loopback-only, so using its source IP would let one
	// local process lock out all administrators. Loopback gRPC is therefore
	// limited by the exact attempted principal, without a shared IP scope.
	principalScope := auth.LoginFailureScope("", uniqueID)
	attempt, allowed := s.limiter.ReserveLoginAttempt(principalScope)
	if !allowed {
		s.recordAuthFailure("locked_out")
		return false, nil
	}
	defer attempt.Cancel()
	valid, admin, err := s.backend.Authenticate(ctx, uniqueID, password)
	if err != nil {
		return false, err
	}
	if !valid || !admin {
		attempt.Fail()
		s.recordAuthFailure("invalid_credentials")
		return false, nil
	}
	attempt.Succeed(principalScope)
	return true, nil
}

func (s *Server) recordAuthFailure(reason string) {
	if recorder, ok := s.limiter.(authFailureRecorder); ok {
		recorder.RecordAuthFailure("grpc", reason)
	}
}

func remoteIPFromContext(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return "unknown"
	}
	host, _, err := net.SplitHostPort(p.Addr.String())
	if err != nil {
		return p.Addr.String()
	}
	return host
}

// parseBasic decodes an HTTP Basic credential.
func parseBasic(header string) (string, string, error) {
	const prefix = "Basic "
	if len(header) > maxBasicAuthorizationBytes {
		return "", "", fmt.Errorf("authorization metadata is too long")
	}
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
	if s.draining.Load() {
		return nil, status.Error(codes.Unavailable, "server shutting down")
	}
	if authExempt[info.FullMethod] {
		return handler(ctx, req)
	}
	uniqueID, err := s.authenticate(ctx)
	if err != nil {
		return nil, err
	}
	return handler(withCallerIdentity(ctx, uniqueID), req)
}

// callerIdentityKey is private and typed so only this package's
// authentication interceptor can establish an authenticated unary caller.
type callerIdentityKey struct{}

// withCallerIdentity records the authenticated principal for downstream unary
// handlers without making transport metadata part of application logic.
func withCallerIdentity(ctx context.Context, uniqueID string) context.Context {
	return context.WithValue(ctx, callerIdentityKey{}, uniqueID)
}

// callerIdentity returns the authenticated unary caller, if the interceptor
// established one. Empty values are deliberately rejected to fail closed.
func callerIdentity(ctx context.Context) (string, bool) {
	uniqueID, ok := ctx.Value(callerIdentityKey{}).(string)
	return uniqueID, ok && uniqueID != ""
}

// unaryRecovery is outermost so a panic in authentication, a handler, or a
// later interceptor cannot take down the serving process or expose request
// data. Keep the log fields fixed: RPC metadata and panic values may contain
// credentials or other user-controlled data.
func (s *Server) unaryRecovery(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (response any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			s.logRPCPanic("unary", info.FullMethod, recovered)
			response = nil
			err = status.Error(codes.Internal, "internal error")
		}
	}()
	return handler(ctx, req)
}

// streamAuth gates streaming RPCs.
func (s *Server) streamAuth(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if s.draining.Load() {
		return status.Error(codes.Unavailable, "server shutting down")
	}
	if authExempt[info.FullMethod] {
		return handler(srv, ss)
	}
	uniqueID, err := s.authenticate(ss.Context())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ss.Context(), s.streamLifetime())
	defer cancel()
	err = handler(srv, &identifiedStream{
		ServerStream: &contextServerStream{ServerStream: ss, ctx: ctx},
		uniqueID:     uniqueID,
	})
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "stream lifetime expired; reconnect")
	}
	return err
}

// streamRecovery is outermost for the same reason as unaryRecovery.
func (s *Server) streamRecovery(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			s.logRPCPanic("stream", info.FullMethod, recovered)
			err = status.Error(codes.Internal, "internal error")
		}
	}()
	return handler(srv, ss)
}

func (s *Server) logRPCPanic(kind, fullMethod string, recovered any) {
	s.logger.Error("grpc handler panic",
		zap.String("rpc_kind", kind),
		zap.String("full_method", fullMethod),
		zap.String("panic_type", fmt.Sprintf("%T", recovered)))
}

func (s *Server) streamLifetime() time.Duration {
	if s.StreamLifetime <= 0 {
		return defaultStreamLifetime
	}
	return s.StreamLifetime
}

// contextServerStream applies the bounded stream context to handlers. gRPC's
// documented interceptor pattern is to wrap ServerStream and override Context.
type contextServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *contextServerStream) Context() context.Context { return s.ctx }

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

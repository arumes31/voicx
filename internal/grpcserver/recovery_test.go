package grpcserver

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"voicx/internal/eventbus"
)

const (
	panicUnaryMethod  = "/grpcserver.test.Panic/UnaryPanic"
	panicStreamMethod = "/grpcserver.test.Panic/StreamPanic"
)

type panicRPCService interface {
	UnaryPanic(context.Context, *emptypb.Empty) (*emptypb.Empty, error)
	StreamPanic(*emptypb.Empty, grpc.ServerStream) error
}

type panicSecret struct{ value string }

type panicRPCServer struct {
	secret string

	panicUnary  atomic.Bool
	panicStream atomic.Bool
	streamCalls atomic.Int32
}

func (s *panicRPCServer) UnaryPanic(context.Context, *emptypb.Empty) (*emptypb.Empty, error) {
	if s.panicUnary.Load() {
		panic(panicSecret{value: s.secret})
	}
	return &emptypb.Empty{}, nil
}

func (s *panicRPCServer) StreamPanic(*emptypb.Empty, grpc.ServerStream) error {
	s.streamCalls.Add(1)
	if s.panicStream.Load() {
		panic(panicSecret{value: s.secret})
	}
	return nil
}

var panicServiceDescription = grpc.ServiceDesc{
	ServiceName: "grpcserver.test.Panic",
	HandlerType: (*panicRPCService)(nil),
	Methods: []grpc.MethodDesc{{
		MethodName: "UnaryPanic",
		Handler:    panicUnaryHandler,
	}},
	Streams: []grpc.StreamDesc{{
		StreamName:    "StreamPanic",
		Handler:       panicStreamHandler,
		ServerStreams: true,
	}},
}

func panicUnaryHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(emptypb.Empty)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(panicRPCService).UnaryPanic(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: panicUnaryMethod}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(panicRPCService).UnaryPanic(ctx, req.(*emptypb.Empty))
	}
	return interceptor(ctx, in, info, handler)
}

func panicStreamHandler(srv any, stream grpc.ServerStream) error {
	in := new(emptypb.Empty)
	if err := stream.RecvMsg(in); err != nil {
		return err
	}
	return srv.(panicRPCService).StreamPanic(in, stream)
}

func TestGRPCRecoveryContainsUnaryAndAuthenticationPanics(t *testing.T) {
	secret := "do-not-log-unary-secret"
	core, observed := observer.New(zap.ErrorLevel)
	logger := zap.New(core)
	backend := &stubBackend{}
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	srv, addr := newGRPCServerWithLogger(t, backend, bus, logger, nil)
	panicServer := &panicRPCServer{secret: secret}
	panicServer.panicUnary.Store(true)
	srv.grpc.RegisterService(&panicServiceDescription, panicServer)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()
	waitForGRPCListener(t, addr)
	t.Cleanup(func() {
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
		if err := <-errCh; err != nil {
			t.Errorf("Start: %v", err)
		}
	})

	conn := dialGRPC(t, addr)
	var response emptypb.Empty
	err := conn.Invoke(authCtx(t, "admin-uid", "pw"), panicUnaryMethod, &emptypb.Empty{}, &response)
	assertInternalPanicResponse(t, err)
	assertSafePanicLog(t, observed, "unary", panicUnaryMethod, secret)

	panicServer.panicUnary.Store(false)
	if err := conn.Invoke(authCtx(t, "admin-uid", "pw"), panicUnaryMethod, &emptypb.Empty{}, &response); err != nil {
		t.Fatalf("normal unary RPC after panic: %v", err)
	}

	observed.TakeAll()
	backend.authenticateFn = func(context.Context, string, string) (bool, bool, error) {
		panic(panicSecret{value: secret})
	}
	err = conn.Invoke(authCtx(t, "admin-uid", "pw"), panicUnaryMethod, &emptypb.Empty{}, &response)
	assertInternalPanicResponse(t, err)
	assertSafePanicLog(t, observed, "unary", panicUnaryMethod, secret)
	backend.authenticateFn = nil
	if err := conn.Invoke(authCtx(t, "admin-uid", "pw"), panicUnaryMethod, &emptypb.Empty{}, &response); err != nil {
		t.Fatalf("normal unary RPC after authentication panic: %v", err)
	}
}

func TestGRPCRecoveryContainsStreamPanicsAfterAuthentication(t *testing.T) {
	secret := "do-not-log-stream-secret"
	core, observed := observer.New(zap.ErrorLevel)
	logger := zap.New(core)
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	backend := &stubBackend{}
	srv, addr := newGRPCServerWithLogger(t, backend, bus, logger, nil)
	panicServer := &panicRPCServer{secret: secret}
	panicServer.panicStream.Store(true)
	srv.grpc.RegisterService(&panicServiceDescription, panicServer)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()
	waitForGRPCListener(t, addr)
	t.Cleanup(func() {
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
		if err := <-errCh; err != nil {
			t.Errorf("Start: %v", err)
		}
	})

	conn := dialGRPC(t, addr)
	if err := callPanicStream(context.Background(), conn); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unauthenticated stream = %v, want Unauthenticated", err)
	}
	if got := panicServer.streamCalls.Load(); got != 0 {
		t.Fatalf("unauthenticated stream invoked handler %d times", got)
	}

	err := callPanicStream(authCtx(t, "admin-uid", "pw"), conn)
	assertInternalPanicResponse(t, err)
	assertSafePanicLog(t, observed, "stream", panicStreamMethod, secret)

	panicServer.panicStream.Store(false)
	if err := callPanicStream(authCtx(t, "admin-uid", "pw"), conn); err != nil && err != io.EOF {
		t.Fatalf("normal stream RPC after panic: %v", err)
	}

	observed.TakeAll()
	backend.authenticateFn = func(context.Context, string, string) (bool, bool, error) {
		panic(panicSecret{value: secret})
	}
	err = callPanicStream(authCtx(t, "admin-uid", "pw"), conn)
	assertInternalPanicResponse(t, err)
	assertSafePanicLog(t, observed, "stream", panicStreamMethod, secret)
	if got := panicServer.streamCalls.Load(); got != 2 {
		t.Fatalf("stream authentication panic invoked handler %d times, want 2", got)
	}
	backend.authenticateFn = nil
	if err := callPanicStream(authCtx(t, "admin-uid", "pw"), conn); err != nil && err != io.EOF {
		t.Fatalf("normal stream RPC after authentication panic: %v", err)
	}
}

func callPanicStream(ctx context.Context, conn *grpc.ClientConn) error {
	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, panicStreamMethod)
	if err != nil {
		return err
	}
	if err := stream.SendMsg(&emptypb.Empty{}); err != nil {
		return err
	}
	if err := stream.CloseSend(); err != nil {
		return err
	}
	return stream.RecvMsg(new(emptypb.Empty))
}

func assertInternalPanicResponse(t *testing.T, err error) {
	t.Helper()
	if status.Code(err) != codes.Internal || status.Convert(err).Message() != "internal error" {
		t.Fatalf("panic response = %v, want Internal/internal error", err)
	}
}

func assertSafePanicLog(t *testing.T, observed *observer.ObservedLogs, kind, method, secret string) {
	t.Helper()
	entries := observed.FilterLevelExact(zap.ErrorLevel).All()
	if len(entries) != 1 {
		t.Fatalf("panic error logs = %d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.Message != "grpc handler panic" || strings.Contains(entry.Message, secret) {
		t.Fatalf("panic log message = %q", entry.Message)
	}
	fields := entry.ContextMap()
	if len(fields) != 3 || fields["rpc_kind"] != kind || fields["full_method"] != method || fields["panic_type"] != "grpcserver.panicSecret" {
		t.Fatalf("panic log fields = %#v", fields)
	}
	if strings.Contains(entry.ContextMap()["panic_type"].(string), secret) {
		t.Fatalf("panic type leaked secret: %#v", fields)
	}
}

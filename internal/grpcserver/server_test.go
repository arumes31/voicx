package grpcserver

import (
	"context"
	"encoding/base64"
	"errors"
	"math"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"voicx/internal/auth"
	"voicx/internal/eventbus"
	"voicx/internal/metrics"
	"voicx/internal/query"
	voicxv1 "voicx/v1"
)

// stubBackend implements query.Backend far enough for the RPCs served here.
// Everything else panics, which keeps an accidental new call site visible.
type stubBackend struct {
	query.Backend
	channels       []query.ChannelInfo
	created        query.ChannelCreateParams
	deleted        int64
	deleteReason   string
	authenticateFn func(context.Context, string, string) (bool, bool, error)
	listChannelsFn func(context.Context) []query.ChannelInfo
}

type countingLoginLimiter struct{ calls int }

func (l *countingLoginLimiter) ReserveLoginAttempt(...string) (*auth.LoginAttempt, bool) {
	l.calls++
	panic("draining interceptor invoked login limiter")
}

func (s *stubBackend) Authenticate(ctx context.Context, uniqueID, password string) (bool, bool, error) {
	if s.authenticateFn != nil {
		return s.authenticateFn(ctx, uniqueID, password)
	}
	switch {
	case uniqueID == "boom":
		return false, false, errors.New("backend down")
	case uniqueID == "admin-uid" && password == "pw":
		return true, true, nil
	case uniqueID == "other-admin" && password == "pw":
		return true, true, nil
	case uniqueID == "user-uid" && password == "pw":
		return true, false, nil
	default:
		return false, false, nil
	}
}

func (s *stubBackend) ListChannels(ctx context.Context) []query.ChannelInfo {
	if s.listChannelsFn != nil {
		return s.listChannelsFn(ctx)
	}
	return s.channels
}

func (s *stubBackend) CreateChannel(_ context.Context, params query.ChannelCreateParams) (int64, error) {
	s.created = params
	return 42, nil
}

func (s *stubBackend) DeleteChannel(_ context.Context, id int64, reason string) error {
	s.deleted = id
	s.deleteReason = reason
	return nil
}

func (s *stubBackend) PermOverview(_ context.Context, uniqueID string, _ int64) ([]query.PermLine, bool, error) {
	if uniqueID != "user-uid" {
		if uniqueID == "perm-boom" {
			return nil, false, errors.New("database detail must not leak")
		}
		return nil, false, auth.ErrUserNotFound
	}
	return []query.PermLine{
		{Key: "i_client_talk_power", Value: 50},
		{Key: "b_client_ban", Value: 0},
		{Key: "i_client_needed_talk_power", Value: 10},
	}, false, nil
}

// startGRPC starts a server on an ephemeral port and returns its address.
func startGRPC(t *testing.T, backend query.Backend, bus *eventbus.Bus) string {
	return startGRPCWith(t, backend, bus, nil)
}

func startGRPCWith(t *testing.T, backend query.Backend, bus *eventbus.Bus, mutate func(*Server)) string {
	t.Helper()
	srv, addr, cancel, errCh := startGRPCServer(t, backend, bus, mutate)
	t.Cleanup(func() {
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			t.Errorf("grpc shutdown: %v", err)
		}
		if err := <-errCh; err != nil {
			t.Errorf("grpc start: %v", err)
		}
	})
	return addr
}

func startGRPCServer(t *testing.T, backend query.Backend, bus *eventbus.Bus, mutate func(*Server)) (*Server, string, context.CancelFunc, <-chan error) {
	t.Helper()
	srv, addr := newGRPCServer(t, backend, bus, mutate)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()
	waitForGRPCListener(t, addr)
	return srv, addr, cancel, errCh
}

func newGRPCServer(t *testing.T, backend query.Backend, bus *eventbus.Bus, mutate func(*Server)) (*Server, string) {
	return newGRPCServerWithLogger(t, backend, bus, zap.NewNop(), mutate)
}

func newGRPCServerWithLogger(t *testing.T, backend query.Backend, bus *eventbus.Bus, logger *zap.Logger, mutate func(*Server)) (*Server, string) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	limiter := query.New("127.0.0.1:0", logger, backend)
	srv := New(addr, backend, bus, logger, limiter)
	if mutate != nil {
		mutate(srv)
	}
	return srv, addr
}

func waitForGRPCListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("gRPC listener %s did not become ready", addr)
}

// dialGRPC opens a client connection.
func dialGRPC(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// authCtx returns a context carrying Basic credentials.
func authCtx(t *testing.T, user, password string) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return metadata.AppendToOutgoingContext(ctx, "authorization",
		"Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+password)))
}

// TestControlRPCs verifies the administration RPCs run against the query
// backend (232).
func TestControlRPCs(t *testing.T) {
	backend := &stubBackend{channels: []query.ChannelInfo{
		{ChannelID: 1, Name: "Lobby", Type: 2, ClientCount: 2},
		{ChannelID: 2, ParentID: 1, Name: "Sub", MaxClients: 8},
		{ChannelID: 3, Name: "Other"},
	}}
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	client := voicxv1.NewControlClient(dialGRPC(t, startGRPC(t, backend, bus)))

	auth, err := client.Authenticate(context.Background(), &voicxv1.AuthenticateRequest{
		Username: "admin-uid", Password: "pw",
	})
	if err != nil || auth.GetUserId() != "admin-uid" {
		t.Fatalf("Authenticate = %+v (err %v)", auth, err)
	}
	// A non-admin login is indistinguishable from invalid credentials.
	_, err = client.Authenticate(context.Background(), &voicxv1.AuthenticateRequest{
		Username: "user-uid", Password: "pw",
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("non-admin Authenticate = %v, want Unauthenticated", err)
	}

	ctx := authCtx(t, "admin-uid", "pw")
	list, err := client.ListChannels(ctx, &voicxv1.ListChannelsRequest{})
	if err != nil || len(list.GetChannels()) != 3 {
		t.Fatalf("ListChannels = %+v (err %v)", list, err)
	}
	if list.GetChannels()[0].GetId() != "1" || !list.GetChannels()[0].GetPermanent() {
		t.Fatalf("channel row = %+v", list.GetChannels()[0])
	}

	// root_channel_id narrows the tree to that subtree.
	sub, err := client.ListChannels(ctx, &voicxv1.ListChannelsRequest{RootChannelId: "1"})
	if err != nil || len(sub.GetChannels()) != 2 {
		t.Fatalf("subtree = %+v (err %v)", sub, err)
	}

	created, err := client.CreateChannel(ctx, &voicxv1.CreateChannelRequest{
		Name: "New", ParentId: "7", MaxClients: 11, Permanent: true,
	})
	if err != nil || created.GetChannelId() != "42" || backend.created != (query.ChannelCreateParams{
		Name: "New", ParentID: 7, MaxClients: 11, Type: 2,
	}) {
		t.Fatalf("CreateChannel = %+v (err %v)", created, err)
	}
	if _, err := client.CreateChannel(ctx, &voicxv1.CreateChannelRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("CreateChannel without a name = %v", err)
	}

	if _, err := client.DeleteChannel(ctx, &voicxv1.DeleteChannelRequest{ChannelId: "2", Reason: "removed"}); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}
	if backend.deleted != 2 || backend.deleteReason != "removed" {
		t.Fatalf("deleted channel = %d reason %q", backend.deleted, backend.deleteReason)
	}

	perms, err := client.QueryPermissions(ctx, &voicxv1.QueryPermissionsRequest{UserId: "user-uid"})
	if err != nil {
		t.Fatalf("QueryPermissions: %v", err)
	}
	if len(perms.GetGranted()) != 1 || perms.GetGranted()[0] != voicxv1.Permission_PERMISSION_SPEAK {
		t.Fatalf("granted = %v", perms.GetGranted())
	}
	if len(perms.GetDenied()) != 1 || perms.GetDenied()[0] != voicxv1.Permission_PERMISSION_BAN {
		t.Fatalf("denied = %v", perms.GetDenied())
	}
	if _, err := client.QueryPermissions(ctx, &voicxv1.QueryPermissionsRequest{UserId: "missing"}); status.Code(err) != codes.NotFound || status.Convert(err).Message() != "user not found" {
		t.Fatalf("missing-user QueryPermissions = %v", err)
	}
	if _, err := client.QueryPermissions(ctx, &voicxv1.QueryPermissionsRequest{UserId: "perm-boom"}); status.Code(err) != codes.Internal || status.Convert(err).Message() != "internal error" {
		t.Fatalf("backend-error QueryPermissions = %v", err)
	}
}

func TestSubtreeTraversesShuffledDescendants(t *testing.T) {
	all := []query.ChannelInfo{
		{ChannelID: 3, ParentID: 2, Name: "grandchild"},
		{ChannelID: 2, ParentID: 1, Name: "child"},
		{ChannelID: 1, Name: "root"},
		{ChannelID: 4, Name: "other"},
	}
	keep := subtree(all, 1)
	for _, id := range []int64{1, 2, 3} {
		if !keep[id] {
			t.Fatalf("subtree omitted reachable channel %d: %v", id, keep)
		}
	}
	if keep[4] {
		t.Fatalf("subtree included unrelated channel: %v", keep)
	}
	if got := subtree(all, 99); len(got) != 0 {
		t.Fatalf("nonexistent root = %v, want no channels", got)
	}
}

func TestSubtreeTraversalMatrixAndDuplicatePolicy(t *testing.T) {
	all := []query.ChannelInfo{
		{ChannelID: 4, ParentID: 3, Name: "grandchild"},
		{ChannelID: 2, ParentID: 1, Name: "child"},
		{ChannelID: 1, ParentID: 2, Name: "root-in-cycle"},
		{ChannelID: 3, ParentID: 2, Name: "child-before-parent"},
		{ChannelID: 2, ParentID: 99, Name: "duplicate-ignored"},
		{ChannelID: 9, Name: "unrelated"},
	}
	if got := subtree(all, 0); len(got) != 5 {
		t.Fatalf("root 0 keeps %d IDs, want 5 canonical IDs: %v", len(got), got)
	}
	for _, id := range []int64{1, 2, 3, 4} {
		if !subtree(all, 1)[id] {
			t.Fatalf("cyclic/shuffled subtree omitted %d", id)
		}
	}
	if subtree(all, 1)[9] {
		t.Fatalf("subtree included unrelated ID: %v", subtree(all, 1))
	}
	if got := subtree(all, 99); len(got) != 0 {
		t.Fatalf("duplicate row changed nonexistent-root semantics: %v", got)
	}
	canonical := uniqueChannels(all)
	if len(canonical) != 5 || canonical[1].Name != "child" {
		t.Fatalf("duplicate policy did not retain first occurrence: %+v", canonical)
	}
}

func TestListChannelsPreservesCanonicalInputOrder(t *testing.T) {
	backend := &stubBackend{channels: []query.ChannelInfo{
		{ChannelID: 4, ParentID: 3, Name: "grandchild"},
		{ChannelID: 2, ParentID: 1, Name: "child"},
		{ChannelID: 1, ParentID: 2, Name: "root-in-cycle"},
		{ChannelID: 3, ParentID: 2, Name: "child-before-parent"},
		{ChannelID: 2, ParentID: 99, Name: "duplicate-ignored"},
		{ChannelID: 9, Name: "unrelated"},
	}}
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	client := voicxv1.NewControlClient(dialGRPC(t, startGRPC(t, backend, bus)))
	ctx := authCtx(t, "admin-uid", "pw")
	for _, test := range []struct {
		root string
		want []string
	}{
		{root: "", want: []string{"4", "2", "1", "3", "9"}},
		{root: "1", want: []string{"4", "2", "1", "3"}},
		{root: "99", want: nil},
	} {
		response, err := client.ListChannels(ctx, &voicxv1.ListChannelsRequest{RootChannelId: test.root})
		if err != nil {
			t.Fatalf("ListChannels(root=%q): %v", test.root, err)
		}
		got := make([]string, 0, len(response.GetChannels()))
		for _, channel := range response.GetChannels() {
			got = append(got, channel.GetId())
		}
		if strings.Join(got, ",") != strings.Join(test.want, ",") {
			t.Fatalf("ListChannels(root=%q) IDs = %v, want %v", test.root, got, test.want)
		}
	}
}

func TestCreateChannelTrimsNameBeforeBackend(t *testing.T) {
	backend := &stubBackend{}
	service := &controlService{backend: backend, logger: zap.NewNop()}
	response, err := service.CreateChannel(context.Background(), &voicxv1.CreateChannelRequest{Name: "  Lobby  "})
	if err != nil || !response.GetSuccess() || backend.created.Name != "Lobby" {
		t.Fatalf("CreateChannel = %+v, backend name = %q, err = %v", response, backend.created.Name, err)
	}
}

func TestListChannelsSkipsInvalidCounts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		channel query.ChannelInfo
	}{
		{name: "negative max clients", channel: query.ChannelInfo{ChannelID: 1, MaxClients: -1}},
		{name: "negative current clients", channel: query.ChannelInfo{ChannelID: 1, ClientCount: -1}},
	}
	if strconv.IntSize == 64 {
		tooLarge := int(int64(math.MaxInt32) + 1)
		tests = append(tests,
			struct {
				name    string
				channel query.ChannelInfo
			}{name: "max clients overflow", channel: query.ChannelInfo{ChannelID: 1, MaxClients: tooLarge}},
			struct {
				name    string
				channel query.ChannelInfo
			}{name: "current clients overflow", channel: query.ChannelInfo{ChannelID: 1, ClientCount: tooLarge}},
		)
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &controlService{backend: &stubBackend{channels: []query.ChannelInfo{test.channel}}, logger: zap.NewNop()}
			response, err := service.ListChannels(context.Background(), &voicxv1.ListChannelsRequest{})
			if err != nil || len(response.GetChannels()) != 0 {
				t.Fatalf("ListChannels() = %+v, %v; want an empty successful response", response, err)
			}
		})
	}
}

func TestSubscribedTypesRejectsAllUnknownFilter(t *testing.T) {
	all, wanted, err := subscribedTypes(&voicxv1.SubscribeEventsRequest{})
	if err != nil || len(all) != len(allBusTypes) || wanted != nil {
		t.Fatalf("empty filter = %v, %v, %v", all, wanted, err)
	}
	_, _, err = subscribedTypes(&voicxv1.SubscribeEventsRequest{EventTypes: []voicxv1.EventType{voicxv1.EventType(999)}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown-only filter error = %v", err)
	}
}

func TestGRPCAddressMustBeLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:12338", "localhost:12338", "[::1]:12338"} {
		if err := validateLoopbackAddr(addr); err != nil {
			t.Errorf("validateLoopbackAddr(%q) = %v", addr, err)
		}
	}
	for _, addr := range []string{":12338", "0.0.0.0:12338", "192.0.2.1:12338"} {
		if err := validateLoopbackAddr(addr); err == nil {
			t.Errorf("validateLoopbackAddr(%q) accepted public bind", addr)
		}
	}
}

func TestDrainingInterceptorsRejectBeforeAuthenticationAndHandlers(t *testing.T) {
	authCalls := 0
	backend := &stubBackend{authenticateFn: func(context.Context, string, string) (bool, bool, error) {
		authCalls++
		panic("draining interceptor invoked backend authentication")
	}}
	limiter := &countingLoginLimiter{}
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	srv := New("127.0.0.1:12338", backend, bus, zap.NewNop(), limiter)
	srv.draining.Store(true)
	handlerCalls := 0

	_, err := srv.unaryAuth(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/grpcserver.test/Ping"}, func(context.Context, any) (any, error) {
		handlerCalls++
		panic("draining interceptor invoked unary handler")
	})
	if status.Code(err) != codes.Unavailable || status.Convert(err).Message() != "server shutting down" {
		t.Fatalf("draining unary = %v, want Unavailable", err)
	}
	err = srv.streamAuth(nil, nil, &grpc.StreamServerInfo{FullMethod: "/grpcserver.test/Ping"}, func(any, grpc.ServerStream) error {
		handlerCalls++
		panic("draining interceptor invoked stream handler")
	})
	if status.Code(err) != codes.Unavailable || status.Convert(err).Message() != "server shutting down" {
		t.Fatalf("draining stream = %v, want Unavailable", err)
	}
	if authCalls != 0 || limiter.calls != 0 || handlerCalls != 0 {
		t.Fatalf("draining callbacks: auth=%d limiter=%d handler=%d, want zero", authCalls, limiter.calls, handlerCalls)
	}
}

func TestGRPCLoginFailuresArePrincipalScoped(t *testing.T) {
	backend := &stubBackend{}
	limiter := query.New("127.0.0.1:0", zap.NewNop(), backend)
	limiter.MaxLoginFailures = 2
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	server := New("127.0.0.1:12338", backend, bus, zap.NewNop(), limiter)

	for range 2 {
		ok, err := server.authenticateAdmin(context.Background(), "127.0.0.1", "admin-uid", "wrong")
		if err != nil || ok {
			t.Fatalf("failed login = %v, %v", ok, err)
		}
	}

	ok, err := server.authenticateAdmin(context.Background(), "127.0.0.1", "other-admin", "pw")
	if err != nil || !ok {
		t.Fatalf("other principal was locked out: %v, %v", ok, err)
	}
	ok, err = server.authenticateAdmin(context.Background(), "127.0.0.1", "admin-uid", "pw")
	if err != nil || ok {
		t.Fatalf("locked principal was accepted: %v, %v", ok, err)
	}
}

func TestGRPCUnknownCredentialsMatchWrongPasswordAndAreMetered(t *testing.T) {
	backend := &stubBackend{}
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	control := voicxv1.NewControlClient(dialGRPC(t, startGRPC(t, backend, bus)))

	var firstMessage string
	for _, credentials := range [][2]string{{"admin-uid", "wrong"}, {"unknown-principal", "wrong"}} {
		_, err := control.ListChannels(authCtx(t, credentials[0], credentials[1]), &voicxv1.ListChannelsRequest{})
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("credentials %q: %v, want Unauthenticated", credentials[0], err)
		}
		if firstMessage == "" {
			firstMessage = status.Convert(err).Message()
		} else if got := status.Convert(err).Message(); got != firstMessage {
			t.Fatalf("wrong and unknown credential responses differ: %q != %q", got, firstMessage)
		}
	}

	m := metrics.New()
	limiter := query.New("127.0.0.1:0", zap.NewNop(), backend)
	limiter.SetMetrics(m)
	server := New("127.0.0.1:12338", backend, bus, zap.NewNop(), limiter)
	for _, principal := range []string{"admin-uid", "unknown-principal"} {
		ok, err := server.authenticateAdmin(context.Background(), "127.0.0.1", principal, "wrong")
		if err != nil || ok {
			t.Fatalf("authenticateAdmin(%q) = %t, %v", principal, ok, err)
		}
	}
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() == "voicx_auth_failures_total" && len(family.GetMetric()) == 1 &&
			family.GetMetric()[0].GetCounter().GetValue() == 2 {
			return
		}
	}
	t.Fatal("gRPC unknown credentials did not emit the bounded invalid-credential metric")
}

func TestParseBasicRejectsOversizedMetadataBeforeDecoding(t *testing.T) {
	_, _, err := parseBasic("Basic " + strings.Repeat("A", 1024))
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("parseBasic(oversized) error = %v", err)
	}
}

func TestGRPCRejectsOversizedAndMultipleAuthorizationMetadata(t *testing.T) {
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	control := voicxv1.NewControlClient(dialGRPC(t, startGRPC(t, &stubBackend{}, bus)))

	valid := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin-uid:pw"))
	multiple := metadata.AppendToOutgoingContext(context.Background(),
		"authorization", valid,
		"authorization", valid,
	)
	if _, err := control.ListChannels(multiple, &voicxv1.ListChannelsRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("multiple authorization values = %v, want Unauthenticated", err)
	}

	overlong := metadata.AppendToOutgoingContext(context.Background(),
		"authorization", "Basic "+strings.Repeat("A", maxHeaderListBytes*2),
	)
	if _, err := control.ListChannels(overlong, &voicxv1.ListChannelsRequest{}); err == nil {
		t.Fatal("oversized authorization metadata was accepted")
	}
}

func TestGRPCTransportLimitsRejectOversizedRequestsAndMetadataBeforeBackend(t *testing.T) {
	backend := &stubBackend{}
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	control := voicxv1.NewControlClient(dialGRPC(t, startGRPC(t, backend, bus)))

	_, err := control.CreateChannel(authCtx(t, "admin-uid", "pw"), &voicxv1.CreateChannelRequest{
		Name: strings.Repeat("x", maxReceiveMessageBytes),
	})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("oversized request = %v, want ResourceExhausted", err)
	}
	if backend.created.Name != "" {
		t.Fatalf("backend created %q for an oversized request", backend.created.Name)
	}

	_, err = control.CreateChannel(authCtx(t, "admin-uid", "pw"), &voicxv1.CreateChannelRequest{
		Name:     "small",
		Metadata: map[string]string{"x-test-oversized": strings.Repeat("x", maxReceiveMessageBytes)},
	})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("oversized request metadata = %v, want ResourceExhausted", err)
	}
	if backend.created.Name != "" {
		t.Fatalf("backend created %q for oversized request metadata", backend.created.Name)
	}
}

func TestGRPCListChannelsCancellationPropagatesToBackend(t *testing.T) {
	for _, tc := range []struct {
		name     string
		newCtx   func() (context.Context, context.CancelFunc)
		wantCode codes.Code
		deadline bool
	}{
		{
			name: "cancel",
			newCtx: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			wantCode: codes.Canceled,
		},
		{
			name: "deadline",
			newCtx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 250*time.Millisecond)
			},
			wantCode: codes.DeadlineExceeded,
			deadline: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered := make(chan struct{})
			type observation struct {
				err            error
				deadline       time.Time
				hasDeadline    bool
				expiredAtEntry bool
			}
			seen := make(chan observation, 1)
			backend := &stubBackend{listChannelsFn: func(ctx context.Context) []query.ChannelInfo {
				deadline, hasDeadline := ctx.Deadline()
				expiredAtEntry := hasDeadline && !time.Now().Before(deadline)
				close(entered)
				<-ctx.Done()
				seen <- observation{
					err:            ctx.Err(),
					deadline:       deadline,
					hasDeadline:    hasDeadline,
					expiredAtEntry: expiredAtEntry,
				}
				return nil
			}}
			bus := eventbus.New(zap.NewNop())
			defer bus.Close()
			control := voicxv1.NewControlClient(dialGRPC(t, startGRPC(t, backend, bus)))

			ctx, cancel := tc.newCtx()
			defer cancel()
			callerDeadline, callerHasDeadline := ctx.Deadline()
			ctx = withAuth(ctx)
			done := make(chan error, 1)
			go func() {
				_, err := control.ListChannels(ctx, &voicxv1.ListChannelsRequest{})
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("backend was not entered")
			}
			if !tc.deadline {
				cancel()
			}
			var err error
			select {
			case err = <-done:
			case <-time.After(time.Second):
				t.Fatal("RPC did not finish after context completion")
			}
			if status.Code(err) != tc.wantCode {
				t.Fatalf("ListChannels = %v, want %v", err, tc.wantCode)
			}
			var observed observation
			select {
			case observed = <-seen:
			case <-time.After(time.Second):
				t.Fatal("backend did not observe context completion")
			}
			if !tc.deadline {
				if !errors.Is(observed.err, context.Canceled) {
					t.Fatalf("backend context = %v, want Canceled", observed.err)
				}
				return
			}
			if !callerHasDeadline || !observed.hasDeadline || observed.expiredAtEntry {
				t.Fatalf("backend deadline propagation = caller:%v backend:%v expired:%t", callerHasDeadline, observed.hasDeadline, observed.expiredAtEntry)
			}
			if delta := observed.deadline.Sub(callerDeadline); delta < -50*time.Millisecond || delta > 50*time.Millisecond {
				t.Fatalf("backend deadline drift = %v, want within 50ms", delta)
			}
			if !errors.Is(observed.err, context.Canceled) && !errors.Is(observed.err, context.DeadlineExceeded) {
				t.Fatalf("backend deadline completion = %v, want Canceled or DeadlineExceeded", observed.err)
			}
		})
	}
}

// TestUnauthenticatedRPCsAreRefused verifies the metadata credential gate
// (232).
func TestUnauthenticatedRPCsAreRefused(t *testing.T) {
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	conn := dialGRPC(t, startGRPC(t, &stubBackend{}, bus))
	control := voicxv1.NewControlClient(conn)

	for _, tc := range []struct {
		name string
		ctx  context.Context
		want codes.Code
	}{
		{"no metadata", context.Background(), codes.Unauthenticated},
		{"wrong password", authCtx(t, "admin-uid", "nope"), codes.Unauthenticated},
		{"not an admin", authCtx(t, "user-uid", "pw"), codes.Unauthenticated},
		{"backend error", authCtx(t, "boom", "pw"), codes.Internal},
	} {
		_, err := control.ListChannels(tc.ctx, &voicxv1.ListChannelsRequest{})
		if status.Code(err) != tc.want {
			t.Fatalf("%s: code = %v, want %v", tc.name, status.Code(err), tc.want)
		}
	}

	// Streams are gated by the same rule.
	stream, err := voicxv1.NewEventsClient(conn).Subscribe(context.Background(), &voicxv1.SubscribeEventsRequest{})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unauthenticated Subscribe = %v", status.Code(err))
	}
}

// TestFileTransferRPCsAreUnimplemented pins the deliberate refusal to mint
// transfer tokens from the bot API (232).
func TestFileTransferRPCsAreUnimplemented(t *testing.T) {
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	client := voicxv1.NewControlClient(dialGRPC(t, startGRPC(t, &stubBackend{}, bus)))
	_, err := client.StartFileTransfer(authCtx(t, "admin-uid", "pw"), &voicxv1.StartFileTransferRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("StartFileTransfer = %v", status.Code(err))
	}
}

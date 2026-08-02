package grpcserver

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"voicx/internal/eventbus"
	"voicx/internal/query"
	voicxv1 "voicx/v1"
)

// stubBackend implements query.Backend far enough for the RPCs served here.
// Everything else panics, which keeps an accidental new call site visible.
type stubBackend struct {
	query.Backend
	channels []query.ChannelInfo
	created  string
	deleted  int64
}

func (s *stubBackend) Authenticate(_ context.Context, uniqueID, password string) (bool, bool, error) {
	switch {
	case uniqueID == "boom":
		return false, false, errors.New("backend down")
	case uniqueID == "admin-uid" && password == "pw":
		return true, true, nil
	case uniqueID == "user-uid" && password == "pw":
		return true, false, nil
	default:
		return false, false, nil
	}
}

func (s *stubBackend) ListChannels(context.Context) []query.ChannelInfo { return s.channels }

func (s *stubBackend) CreateChannel(_ context.Context, name, _ string, _ int) (int64, error) {
	s.created = name
	return 42, nil
}

func (s *stubBackend) DeleteChannel(_ context.Context, id int64) error {
	s.deleted = id
	return nil
}

func (s *stubBackend) PermOverview(_ context.Context, uniqueID string, _ int64) ([]query.PermLine, error) {
	if uniqueID != "user-uid" {
		return nil, errors.New("user not found")
	}
	return []query.PermLine{
		{Key: "i_client_talk_power", Value: 50},
		{Key: "b_client_ban", Value: 0},
		{Key: "i_client_needed_talk_power", Value: 10},
	}, nil
}

// startGRPC starts a server on an ephemeral port and returns its address.
func startGRPC(t *testing.T, backend query.Backend, bus *eventbus.Bus) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	srv := New(addr, backend, bus, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		srv.Stop()
		<-errCh
	})
	return addr
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
	if err != nil || !auth.GetSuccess() || auth.GetUserId() != "admin-uid" {
		t.Fatalf("Authenticate = %+v (err %v)", auth, err)
	}
	// A non-admin login is reported as a failure, not an error.
	denied, err := client.Authenticate(context.Background(), &voicxv1.AuthenticateRequest{
		Username: "user-uid", Password: "pw",
	})
	if err != nil || denied.GetSuccess() {
		t.Fatalf("non-admin Authenticate = %+v (err %v)", denied, err)
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

	created, err := client.CreateChannel(ctx, &voicxv1.CreateChannelRequest{Name: "New", Permanent: true})
	if err != nil || created.GetChannelId() != "42" || backend.created != "New" {
		t.Fatalf("CreateChannel = %+v (err %v)", created, err)
	}
	if _, err := client.CreateChannel(ctx, &voicxv1.CreateChannelRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("CreateChannel without a name = %v", err)
	}

	if _, err := client.DeleteChannel(ctx, &voicxv1.DeleteChannelRequest{ChannelId: "2"}); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}
	if backend.deleted != 2 {
		t.Fatalf("deleted channel = %d", backend.deleted)
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

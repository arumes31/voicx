package grpcserver

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"voicx/internal/auth"
	"voicx/internal/channels"
	"voicx/internal/eventbus"
	"voicx/internal/query"
	voicxv1 "voicx/v1"
)

func TestGeneratedServiceContracts(t *testing.T) {
	for _, test := range []struct {
		name string
		file protoreflect.FileDescriptor
	}{
		{name: "Chat", file: voicxv1.File_chat_proto},
		{name: "Signaling", file: voicxv1.File_signaling_proto},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := test.file.Services().ByName(protoreflect.Name(test.name))
			if service == nil {
				t.Fatalf("%s service descriptor is missing", test.name)
			}
			options, ok := service.Options().(*descriptorpb.ServiceOptions)
			if !ok || !options.GetDeprecated() {
				t.Fatalf("%s deprecated option = %v, want true", test.name, options)
			}
		})
	}

	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	server := New("127.0.0.1:12338", &stubBackend{}, bus, zap.NewNop(), nil)
	services := server.grpc.GetServiceInfo()
	got := make([]string, 0, len(services))
	for name := range services {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"voicx.v1.Control", "voicx.v1.Events"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("registered gRPC services = %v, want %v", got, want)
	}
}

func TestGeneratedAuthenticationReservationContracts(t *testing.T) {
	request := (&voicxv1.AuthenticateRequest{}).ProtoReflect().Descriptor()
	response := (&voicxv1.AuthenticateResponse{}).ProtoReflect().Descriptor()
	if request.Fields().ByName("token") != nil {
		t.Fatal("AuthenticateRequest still exposes dead token field")
	}
	if !request.ReservedRanges().Has(3) || !hasReservedName(request, "token") {
		t.Fatal("AuthenticateRequest does not reserve token field number and name")
	}
	for _, name := range []protoreflect.Name{"success", "session_token", "display_name", "expires_at", "error"} {
		if response.Fields().ByName(name) != nil || !hasReservedName(response, name) {
			t.Fatalf("AuthenticateResponse reservation for %q is missing", name)
		}
	}
	for _, number := range []protoreflect.FieldNumber{1, 2, 4, 5, 6} {
		if !response.ReservedRanges().Has(number) {
			t.Fatalf("AuthenticateResponse does not reserve field number %d", number)
		}
	}
	userID := response.Fields().ByName("user_id")
	if userID == nil || userID.Number() != 3 {
		t.Fatalf("AuthenticateResponse user_id descriptor = %v, want field 3", userID)
	}
}

func TestUserBannedEventChannelFieldContract(t *testing.T) {
	descriptor := (&voicxv1.UserBannedEvent{}).ProtoReflect().Descriptor()
	field := descriptor.Fields().ByName("channel_id")
	if field == nil || field.Number() != 5 || field.Kind() != protoreflect.StringKind {
		t.Fatalf("UserBannedEvent channel_id descriptor = %v, want string field 5", field)
	}
}

func hasReservedName(descriptor protoreflect.MessageDescriptor, want protoreflect.Name) bool {
	for i := 0; i < descriptor.ReservedNames().Len(); i++ {
		if descriptor.ReservedNames().Get(i) == want {
			return true
		}
	}
	return false
}

type permissionBackend struct {
	query.Backend
	lookups []string
}

func (b *permissionBackend) Authenticate(_ context.Context, uniqueID, password string) (bool, bool, error) {
	return uniqueID == "caller" && password == "pw", uniqueID == "caller" && password == "pw", nil
}

func (b *permissionBackend) PermOverview(_ context.Context, uniqueID string, _ int64) ([]query.PermLine, bool, error) {
	b.lookups = append(b.lookups, uniqueID)
	switch uniqueID {
	case "caller":
		return []query.PermLine{{Key: "i_client_talk_power", Value: 1}}, true, nil
	case "target":
		return []query.PermLine{{Key: "b_client_ban", Value: 0}}, false, nil
	default:
		return nil, false, auth.ErrUserNotFound
	}
}

func TestQueryPermissionsUsesAuthenticatedCallerAndAdminFlag(t *testing.T) {
	backend := &permissionBackend{}
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	client := voicxv1.NewControlClient(dialGRPC(t, startGRPC(t, backend, bus)))
	ctx := authCtx(t, "caller", "pw")

	caller, err := client.QueryPermissions(ctx, &voicxv1.QueryPermissionsRequest{ChannelId: "7"})
	if err != nil {
		t.Fatalf("QueryPermissions for caller: %v", err)
	}
	if !caller.GetIsAdmin() || !reflect.DeepEqual(caller.GetGranted(), []voicxv1.Permission{voicxv1.Permission_PERMISSION_SPEAK}) {
		t.Fatalf("caller permissions = %+v, want administrator speaking permission", caller)
	}
	target, err := client.QueryPermissions(ctx, &voicxv1.QueryPermissionsRequest{ChannelId: "7", UserId: "target"})
	if err != nil {
		t.Fatalf("QueryPermissions for explicit target: %v", err)
	}
	if target.GetIsAdmin() || !reflect.DeepEqual(target.GetDenied(), []voicxv1.Permission{voicxv1.Permission_PERMISSION_BAN}) {
		t.Fatalf("target permissions = %+v, want non-admin denied ban", target)
	}
	if !reflect.DeepEqual(backend.lookups, []string{"caller", "target"}) {
		t.Fatalf("permission lookup identities = %v, want caller then explicit target", backend.lookups)
	}

	service := &controlService{backend: backend, logger: zap.NewNop()}
	if _, err := service.QueryPermissions(context.Background(), &voicxv1.QueryPermissionsRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("QueryPermissions without injected caller = %v, want Unauthenticated", err)
	}
}

func TestControlChannelValidationAndBackendStatus(t *testing.T) {
	service := &controlService{backend: &stubBackend{}, logger: zap.NewNop()}
	for _, request := range []*voicxv1.CreateChannelRequest{
		{Name: "channel", ParentId: "0"},
		{Name: "channel", ParentId: "-1"},
		{Name: "channel", ParentId: "not-a-number"},
		{Name: "channel", MaxClients: -1},
	} {
		if _, err := service.CreateChannel(context.Background(), request); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("CreateChannel(%+v) = %v, want InvalidArgument", request, err)
		}
	}
	if _, err := service.DeleteChannel(context.Background(), &voicxv1.DeleteChannelRequest{
		ChannelId: "1", Reason: strings.Repeat("x", maxDeleteReasonBytes+1),
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("DeleteChannel oversized reason = %v, want InvalidArgument", err)
	}

	for _, test := range []struct {
		name string
		err  error
		want codes.Code
		text string
	}{
		{name: "canceled", err: context.Canceled, want: codes.Canceled, text: "request canceled"},
		{name: "deadline", err: context.DeadlineExceeded, want: codes.DeadlineExceeded, text: "deadline exceeded"},
		{name: "missing user", err: auth.ErrUserNotFound, want: codes.NotFound, text: "not found"},
		{name: "missing channel", err: channels.ErrChannelNotFound, want: codes.NotFound, text: "not found"},
		{name: "invalid specification", err: channels.ErrInvalidSpec, want: codes.FailedPrecondition, text: "request cannot be applied"},
		{name: "invalid move", err: channels.ErrInvalidMove, want: codes.FailedPrecondition, text: "request cannot be applied"},
		{name: "unknown", err: errors.New("database detail must not leak"), want: codes.Internal, text: "internal error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			mapped := grpcBackendStatus(zap.NewNop(), "test", test.err)
			if got := status.Code(mapped); got != test.want {
				t.Fatalf("grpcBackendStatus(%v) = %v, want %v", test.err, got, test.want)
			}
			if got := status.Convert(mapped).Message(); got != test.text {
				t.Fatalf("grpcBackendStatus(%v) message = %q, want %q", test.err, got, test.text)
			}
		})
	}
}

func TestListChannelsSkipsOnlyMalformedRows(t *testing.T) {
	service := &controlService{backend: &stubBackend{channels: []query.ChannelInfo{
		{ChannelID: 1, Name: "valid", MaxClients: 3, ClientCount: 1},
		{ChannelID: 2, Name: "bad max", MaxClients: -1},
		{ChannelID: 3, Name: "bad count", ClientCount: -1},
	}}, logger: zap.NewNop()}
	response, err := service.ListChannels(context.Background(), &voicxv1.ListChannelsRequest{})
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	if len(response.GetChannels()) != 1 || response.GetChannels()[0].GetId() != "1" {
		t.Fatalf("ListChannels retained %+v, want only valid channel 1", response.GetChannels())
	}
}

func TestAllFileTransferRPCsAreUnimplemented(t *testing.T) {
	bus := eventbus.New(zap.NewNop())
	defer bus.Close()
	client := voicxv1.NewControlClient(dialGRPC(t, startGRPC(t, &stubBackend{}, bus)))
	ctx := authCtx(t, "admin-uid", "pw")
	for _, test := range []struct {
		name string
		call func() error
	}{
		{name: "start", call: func() error {
			_, err := client.StartFileTransfer(ctx, &voicxv1.StartFileTransferRequest{})
			return err
		}},
		{name: "status", call: func() error {
			_, err := client.GetFileTransferStatus(ctx, &voicxv1.GetFileTransferStatusRequest{})
			return err
		}},
		{name: "cancel", call: func() error {
			_, err := client.CancelFileTransfer(ctx, &voicxv1.CancelFileTransferRequest{})
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); status.Code(err) != codes.Unimplemented {
				t.Fatalf("%s file transfer RPC = %v, want Unimplemented", test.name, err)
			}
		})
	}
}

func TestChannelDeletedEventIncludesDeleteReason(t *testing.T) {
	event, err := toProto(eventbus.Event{
		Type: "channel_deleted",
		Data: []byte(`{"channel_id":7,"reason":"retired"}`),
	})
	if err != nil || event == nil || event.GetChannelDeleted().GetReason() != "retired" {
		t.Fatalf("channel deletion event = %+v, want reason", event)
	}
}

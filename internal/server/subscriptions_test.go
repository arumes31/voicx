package server

import (
	"context"
	"net"
	"reflect"
	"strings"
	"testing"

	"voicx/internal/netproto"
	"voicx/internal/permissions"
	"voicx/internal/state"
)

func TestSubscriptionSetIsAuthoritativeDeduplicatedAndSorted(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	conn, clientID := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = conn.Close() }()
	client, ok := env.srv.clientByID(clientID)
	if !ok {
		t.Fatal("authenticated client is not registered")
	}
	env.state.AddChannel(&state.Channel{ChannelID: 7, Name: "seven"})
	env.state.AddChannel(&state.Channel{ChannelID: 3, Name: "three"})
	env.state.Subscribe(clientID, []int64{7, 3, 7})
	if got, want := env.srv.subscriptionSet(client), []int64{3, 7}; !reflect.DeepEqual(got, want) {
		t.Fatalf("subscription set = %v, want %v", got, want)
	}
	env.state.Unsubscribe(clientID, []int64{3})
	if got, want := env.srv.subscriptionSet(client), []int64{7}; !reflect.DeepEqual(got, want) {
		t.Fatalf("subscription set after unsubscribe = %v, want %v", got, want)
	}
}

func TestChannelSubscribeRejectsMissingKeyAndOver64Targets(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = conn.Close() }()
	ids := make([]int64, maxSubscribeTargets+1)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	send(t, conn, netproto.MsgChannelSubscribe, netproto.ChannelSubscribe{Subscribe: true, ChannelIDs: ids})
	if got := readError(t, conn); got.Code != errCodeMalformed {
		t.Fatalf("65 targets error = %+v, want malformed", got)
	}
	env.state.AddChannel(&state.Channel{ChannelID: 99, Name: "target"})
	send(t, conn, netproto.MsgChannelSubscribe, netproto.ChannelSubscribe{Subscribe: true, ChannelIDs: []int64{99}})
	if got := readError(t, conn); got.Code != errCodePermissionDenied {
		t.Fatalf("missing encryption key error = %+v, want permission denied", got)
	}
}

func TestChannelSubscribeDeliversKeysHonorsCurrentChannelAndRevokes(t *testing.T) {
	tp := subscriptionPermissions()
	allowed := map[int64]bool{40: true}
	env := startTestEnvDeps(t, &tp, nil, func(deps *Deps) {
		deps.Perms.(*fakePerms).loadForClientFn = func(_ context.Context, _ int64, channelID int64) (permissions.TieredPermissions, error) {
			if allowed[channelID] {
				return tp, nil
			}
			return permissions.NewTieredPermissions(), nil
		}
	})
	defer env.stop()
	env.state.AddChannel(&state.Channel{ChannelID: 40, Name: "forty"})
	env.state.AddChannel(&state.Channel{ChannelID: 41, Name: "forty-one"})
	pub, _ := testX25519(t)
	conn, authResp := dialSubscriptionClient(t, env.addr, "user-uid", pub)
	defer func() { _ = conn.Close() }()

	// A mixed request must accept the entitled channel, refuse denied and absent
	// targets on the wire, and still deliver the accepted channel's key.
	send(t, conn, netproto.MsgChannelSubscribe, netproto.ChannelSubscribe{Subscribe: true, ChannelIDs: []int64{40, 41, 404}})
	reply, keys, errs := readSubscriptionReply(t, conn)
	if !reflect.DeepEqual(reply.ChannelIDs, []int64{40}) || !reflect.DeepEqual(env.state.Subscriptions(authResp.ClientID), []int64{40}) {
		t.Fatalf("subscription reply/state = %v/%v, want [40]", reply.ChannelIDs, env.state.Subscriptions(authResp.ClientID))
	}
	if len(keys) != 1 || keys[0].ChannelID != 40 {
		t.Fatalf("delivered keys = %+v, want exactly channel 40", keys)
	}
	if len(errs) != 1 || errs[0].Code != errCodePermissionDenied || !strings.Contains(errs[0].Message, "41") || !strings.Contains(errs[0].Message, "404") {
		t.Fatalf("mixed-target refusal = %+v, want permission-denied naming 41 and 404", errs)
	}
	if got := env.deps.ScopeKeys.(*fakeScopeKeys).countFor(40); got != 1 {
		t.Fatalf("scope keys for accepted target = %d, want 1", got)
	}
	if err := env.state.JoinChannel(authResp.ClientID, 40); err != nil {
		t.Fatal(err)
	}
	env.state.Unsubscribe(authResp.ClientID, []int64{40})
	send(t, conn, netproto.MsgChannelSubscribe, netproto.ChannelSubscribe{Subscribe: true, ChannelIDs: []int64{40}})
	reply, keys, errs = readSubscriptionReply(t, conn)
	if !reflect.DeepEqual(reply.ChannelIDs, []int64{40}) || len(env.state.Subscriptions(authResp.ClientID)) != 0 {
		t.Fatalf("current channel reply/state = %v/%v, want implicit [40] only", reply.ChannelIDs, env.state.Subscriptions(authResp.ClientID))
	}
	if len(keys) != 0 || len(errs) != 0 {
		t.Fatalf("current channel request delivered unexpected keys/errors = %+v/%+v", keys, errs)
	}
	// An explicit unsubscribe cannot remove the implicit current-channel entry.
	send(t, conn, netproto.MsgChannelSubscribe, netproto.ChannelSubscribe{Subscribe: false, ChannelIDs: []int64{40}})
	reply, keys, errs = readSubscriptionReply(t, conn)
	if !reflect.DeepEqual(reply.ChannelIDs, []int64{40}) || len(keys) != 0 || len(errs) != 0 {
		t.Fatalf("current channel unsubscribe = %+v, keys/errors=%+v/%+v", reply, keys, errs)
	}
	if err := env.state.LeaveChannel(authResp.ClientID); err != nil {
		t.Fatal(err)
	}
	env.state.Subscribe(authResp.ClientID, []int64{40})
	allowed[40] = false
	if got := env.srv.channelSubscribers(context.Background(), 40); len(got) != 0 {
		t.Fatalf("revoked subscribers = %v, want none", got)
	}
	if got := env.state.Subscriptions(authResp.ClientID); len(got) != 0 {
		t.Fatalf("revoked subscription remains = %v", got)
	}
	if pushed, _, pushedErrs := readSubscriptionReply(t, conn); len(pushed.ChannelIDs) != 0 || len(pushedErrs) != 0 {
		t.Fatalf("revocation state = %+v, errors=%+v, want empty state without error", pushed, pushedErrs)
	}
}

func TestChannelSubscribeAllows64AndRejects65thStandingSubscription(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	pub, _ := testX25519(t)
	conn, authResp := dialSubscriptionClient(t, env.addr, "admin-uid", pub)
	defer func() { _ = conn.Close() }()
	ids := make([]int64, maxSubscriptions)
	for i := range ids {
		ids[i] = int64(1000 + i)
		env.state.AddChannel(&state.Channel{ChannelID: ids[i], Name: "target"})
	}
	// A full legal request is accepted on the wire, not merely via a seeded
	// state fixture. It also exercises the 64-key delivery fan-out.
	send(t, conn, netproto.MsgChannelSubscribe, netproto.ChannelSubscribe{Subscribe: true, ChannelIDs: ids})
	reply, keys, errs := readSubscriptionReply(t, conn)
	if len(reply.ChannelIDs) != maxSubscriptions || len(keys) != maxSubscriptions || len(errs) != 0 {
		t.Fatalf("64 target request reply/keys/errors = %+v/%d/%+v", reply, len(keys), errs)
	}
	env.state.AddChannel(&state.Channel{ChannelID: 2000, Name: "overflow"})
	send(t, conn, netproto.MsgChannelSubscribe, netproto.ChannelSubscribe{Subscribe: true, ChannelIDs: []int64{2000}})
	if got := readError(t, conn); got.Code != errCodeMalformed {
		t.Fatalf("65th standing subscription error = %+v, want malformed", got)
	}
	if got := env.state.Subscriptions(authResp.ClientID); len(got) != maxSubscriptions {
		t.Fatalf("standing subscriptions = %d, want %d", len(got), maxSubscriptions)
	}
}

// dialSubscriptionClient consumes the authentication-time authoritative
// subscription state. Every later state frame can therefore be associated
// with the request under test, rather than being an auth-time leftover.
func dialSubscriptionClient(t *testing.T, addr, uniqueID string, pub [32]byte) (net.Conn, netproto.AuthResponse) {
	t.Helper()
	conn, response := dialAuthedX25519(t, addr, uniqueID, pub)
	initial, _, errs := readSubscriptionReply(t, conn)
	if len(initial.ChannelIDs) != 0 || len(errs) != 0 {
		_ = conn.Close()
		t.Fatalf("initial subscription state = %+v, errors=%+v, want empty", initial, errs)
	}
	return conn, response
}

// readSubscriptionReply reads the key/error frames that precede an
// authoritative subscription state. The protocol deliberately replies with
// a state even when selected targets were refused, so callers need both.
func readSubscriptionReply(t *testing.T, conn net.Conn) (netproto.SubscriptionState, []netproto.ChannelKey, []netproto.Error) {
	t.Helper()
	var keys []netproto.ChannelKey
	var errs []netproto.Error
	for {
		frame := readFrame(t, conn)
		switch netproto.MessageType(frame.Type) {
		case netproto.MsgChannelKey:
			var key netproto.ChannelKey
			if err := netproto.Decode(frame, &key); err != nil {
				t.Fatalf("decode channel key: %v", err)
			}
			keys = append(keys, key)
		case netproto.MsgError:
			var responseErr netproto.Error
			if err := netproto.Decode(frame, &responseErr); err != nil {
				t.Fatalf("decode subscription error: %v", err)
			}
			errs = append(errs, responseErr)
		case netproto.MsgSubscriptionState:
			var state netproto.SubscriptionState
			if err := netproto.Decode(frame, &state); err != nil {
				t.Fatalf("decode subscription state: %v", err)
			}
			return state, keys, errs
		}
	}
}

func subscriptionPermissions() permissions.TieredPermissions {
	tp := permissions.NewTieredPermissions()
	set := permissions.NewPermissionSet()
	set.Set(&permissions.Permission{Key: permissions.PermissionKeyChannelSubscribePower, Type: permissions.PermissionTypeInteger, Value: 10, Grant: 10})
	set.Set(&permissions.Permission{Key: permissions.PermissionKeyChannelNeededSubscribePower, Type: permissions.PermissionTypeInteger, Value: 5, Grant: 5})
	tp.Set(permissions.TierServerGroup, set)
	return tp
}

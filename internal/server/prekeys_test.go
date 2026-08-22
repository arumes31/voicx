package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"voicx/internal/e2ee"
	"voicx/internal/netproto"
	"voicx/internal/store"
)

func TestPreKeyPublishAndOneTimeQuery(t *testing.T) {
	keys := newFakePreKeys()
	env := startTestEnvDeps(t, nil, nil, func(deps *Deps) { deps.PreKeys = keys })
	defer env.stop()
	publisher, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = publisher.Close() }()
	send(t, publisher, netproto.MsgPreKeyPublish, validPreKeyPublish(t, testOneTimePreKey(t, 9)))
	waitFor(t, "prekey publish", func() bool { return keys.publishCount() == 1 })

	requester, _ := dialAuthed(t, env.addr, "admin-uid")
	defer func() { _ = requester.Close() }()
	for i := 0; i < 2; i++ {
		send(t, requester, netproto.MsgPreKeyQuery, netproto.PreKeyQuery{UniqueID: "user-uid"})
		f := readOfType(t, requester, netproto.MsgPreKeyBundle)
		var got netproto.PreKeyBundle
		if err := netproto.Decode(f, &got); err != nil {
			t.Fatalf("decode prekey response %d: %v", i, err)
		}
		if got.UniqueID != "user-uid" || got.SignedPreKeyID != 7 {
			t.Fatalf("prekey response %d = %+v", i, got)
		}
		if i == 0 && got.OneTimeKeyID != 9 {
			t.Fatalf("first query one-time id = %d, want 9", got.OneTimeKeyID)
		}
		if i == 1 && got.OneTimeKeyID != 0 {
			t.Fatalf("second query reused one-time id %d", got.OneTimeKeyID)
		}
	}
	if !containsAction(env.groups.auditActions(), "e2ee_prekeys_publish") {
		t.Fatalf("audit actions = %v, want e2ee_prekeys_publish", env.groups.auditActions())
	}
}

func TestPreKeyHandlersMapMalformedAndMissingBundleErrors(t *testing.T) {
	keys := newFakePreKeys()
	env := startTestEnvDeps(t, nil, nil, func(deps *Deps) { deps.PreKeys = keys })
	defer env.stop()
	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = conn.Close() }()
	send(t, conn, netproto.MsgPreKeyPublish, netproto.PreKeyPublish{})
	if got := readError(t, conn); got.Code != errCodeMalformed {
		t.Fatalf("empty publish error = %+v, want malformed", got)
	}
	send(t, conn, netproto.MsgPreKeyQuery, netproto.PreKeyQuery{UniqueID: "missing"})
	if got := readError(t, conn); got.Code != errCodeNotFound {
		t.Fatalf("missing query error = %+v, want not found", got)
	}
	if got := keys.consumeCount(); got != 0 {
		t.Fatalf("missing user consumed a prekey %d times", got)
	}
}

func TestPreKeyQueryRateLimitPreventsAdditionalConsumption(t *testing.T) {
	keys := newFakePreKeys()
	env := startTestEnvDeps(t, nil, nil, func(deps *Deps) { deps.PreKeys = keys })
	defer env.stop()
	env.srv.chatRate = newChatRateLimiter(1, time.Hour)
	conn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer func() { _ = conn.Close() }()

	// The first lookup reaches the store and misses; the second must be stopped
	// before it can consume another one-time key.
	send(t, conn, netproto.MsgPreKeyQuery, netproto.PreKeyQuery{UniqueID: "user-uid"})
	if got := readError(t, conn); got.Code != errCodeNotFound {
		t.Fatalf("first rate-limit query = %+v, want not found", got)
	}
	if got := keys.consumeCount(); got != 1 {
		t.Fatalf("first query consume count = %d, want 1", got)
	}
	send(t, conn, netproto.MsgPreKeyQuery, netproto.PreKeyQuery{UniqueID: "user-uid"})
	if got := readError(t, conn); got.Code != errCodeMalformed {
		t.Fatalf("rate-limited query = %+v, want malformed", got)
	}
	if got := keys.consumeCount(); got != 1 {
		t.Fatalf("rate-limited query consumed a prekey: count = %d, want 1", got)
	}
}

func TestPreKeyHandlersMapDependencyErrors(t *testing.T) {
	t.Run("identity lookup", func(t *testing.T) {
		keys := newFakePreKeys()
		keys.identityErr = errors.New("identity backend offline")
		env := startTestEnvDeps(t, nil, nil, func(deps *Deps) { deps.PreKeys = keys })
		defer env.stop()
		conn, _ := dialAuthed(t, env.addr, "user-uid")
		defer func() { _ = conn.Close() }()
		send(t, conn, netproto.MsgPreKeyPublish, validPreKeyPublish(t))
		if got := readError(t, conn); got.Code != errCodeUnavailable {
			t.Fatalf("identity lookup error = %+v, want unavailable", got)
		}
		if got := keys.publishCount(); got != 0 {
			t.Fatalf("failed identity lookup published %d bundles", got)
		}
	})

	t.Run("publish", func(t *testing.T) {
		keys := newFakePreKeys()
		keys.publishErr = errors.New("publish backend offline")
		env := startTestEnvDeps(t, nil, nil, func(deps *Deps) { deps.PreKeys = keys })
		defer env.stop()
		conn, _ := dialAuthed(t, env.addr, "user-uid")
		defer func() { _ = conn.Close() }()
		send(t, conn, netproto.MsgPreKeyPublish, validPreKeyPublish(t))
		if got := readError(t, conn); got.Code != errCodeUnavailable {
			t.Fatalf("publish error = %+v, want unavailable", got)
		}
		if got := keys.publishCount(); got != 1 {
			t.Fatalf("publish call count = %d, want 1", got)
		}
	})

	t.Run("consume", func(t *testing.T) {
		keys := newFakePreKeys()
		keys.consumeErr = errors.New("consume backend offline")
		env := startTestEnvDeps(t, nil, nil, func(deps *Deps) { deps.PreKeys = keys })
		defer env.stop()
		conn, _ := dialAuthed(t, env.addr, "admin-uid")
		defer func() { _ = conn.Close() }()
		send(t, conn, netproto.MsgPreKeyQuery, netproto.PreKeyQuery{UniqueID: "user-uid"})
		if got := readError(t, conn); got.Code != errCodeUnavailable {
			t.Fatalf("consume error = %+v, want unavailable", got)
		}
		if got := keys.consumeCount(); got != 1 {
			t.Fatalf("consume call count = %d, want 1", got)
		}
	})
}

func TestPreKeyPublishAuditsIdentityChange(t *testing.T) {
	keys := newFakePreKeys()
	env := startTestEnvDeps(t, nil, nil, func(deps *Deps) { deps.PreKeys = keys })
	defer env.stop()
	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = conn.Close() }()

	send(t, conn, netproto.MsgPreKeyPublish, validPreKeyPublish(t))
	waitFor(t, "initial prekey publish", func() bool { return keys.publishCount() == 1 })
	send(t, conn, netproto.MsgPreKeyPublish, validPreKeyPublish(t))
	waitFor(t, "replacement prekey publish", func() bool { return keys.publishCount() == 2 })
	if !containsAction(env.groups.auditActions(), "e2ee_identity_changed") {
		t.Fatalf("audit actions = %v, want e2ee_identity_changed", env.groups.auditActions())
	}
}

func TestPreKeyPublishRejectsInvalidDuplicateAndTooManyOneTimeKeys(t *testing.T) {
	keys := newFakePreKeys()
	env := startTestEnvDeps(t, nil, nil, func(deps *Deps) { deps.PreKeys = keys })
	defer env.stop()
	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = conn.Close() }()

	valid := testOneTimePreKey(t, 1)
	tooMany := make([]netproto.OneTimePreKey, maxPublishedOneTimePreKeys+1)
	for i := range tooMany {
		tooMany[i] = testOneTimePreKey(t, uint32(i+1))
	}
	for _, tc := range []struct {
		name string
		keys []netproto.OneTimePreKey
	}{
		{name: "zero key ID", keys: []netproto.OneTimePreKey{{KeyID: 0, PublicKey: valid.PublicKey}}},
		{name: "wrong public key size", keys: []netproto.OneTimePreKey{{KeyID: 2, PublicKey: []byte{1}}}},
		{name: "duplicate IDs", keys: []netproto.OneTimePreKey{valid, {KeyID: valid.KeyID, PublicKey: testOneTimePreKey(t, 2).PublicKey}}},
		{name: "too many", keys: tooMany},
	} {
		t.Run(tc.name, func(t *testing.T) {
			send(t, conn, netproto.MsgPreKeyPublish, validPreKeyPublish(t, tc.keys...))
			if got := readError(t, conn); got.Code != errCodeMalformed {
				t.Fatalf("%s error = %+v, want malformed", tc.name, got)
			}
		})
	}
	if got := keys.identityCount(); got != 0 {
		t.Fatalf("invalid one-time keys reached identity lookup %d times", got)
	}
	if got := keys.publishCount(); got != 0 {
		t.Fatalf("invalid one-time keys published %d bundles", got)
	}
}

func TestPreKeyHandlersDenyUnregisteredClientsWithoutStoreAccess(t *testing.T) {
	keys := newFakePreKeys()
	env := startTestEnvDeps(t, nil, nil, func(deps *Deps) { deps.PreKeys = keys })
	defer env.stop()

	for _, tc := range []struct {
		name   string
		invoke preKeyHandler
		frame  *netproto.Frame
	}{
		{
			name:   "publish",
			invoke: env.srv.handlePreKeyPublish,
			frame:  mustEncodePreKeyFrame(t, netproto.MsgPreKeyPublish, validPreKeyPublish(t)),
		},
		{
			name:   "query",
			invoke: env.srv.handlePreKeyQuery,
			frame:  mustEncodePreKeyFrame(t, netproto.MsgPreKeyQuery, netproto.PreKeyQuery{UniqueID: "user-uid"}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := invokePreKeyHandler(t, tc.invoke, tc.frame); got.Code != errCodePermissionDenied {
				t.Fatalf("unregistered %s error = %+v, want permission denied", tc.name, got)
			}
		})
	}
	if got := keys.identityCount(); got != 0 {
		t.Fatalf("unregistered client reached identity store %d times", got)
	}
	if got := keys.publishCount(); got != 0 {
		t.Fatalf("unregistered client published %d bundles", got)
	}
	if got := keys.consumeCount(); got != 0 {
		t.Fatalf("unregistered client consumed %d bundles", got)
	}
}

func validPreKeyPublish(t *testing.T, oneTime ...netproto.OneTimePreKey) netproto.PreKeyPublish {
	t.Helper()
	identity, err := e2ee.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	signed, err := e2ee.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	_, signing, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bundle := e2ee.NewPreKeyBundle(identity, signing, 7, signed, 0, e2ee.KeyPair{})
	return netproto.PreKeyPublish{
		IdentityDH: identity.Public, SigningPublic: bundle.SigningPublic, SignedPreKeyID: bundle.SignedPreKeyID,
		SignedPreKey: bundle.SignedPreKey, Signature: bundle.Signature, OneTimePreKeys: oneTime,
	}
}

func testOneTimePreKey(t *testing.T, id uint32) netproto.OneTimePreKey {
	t.Helper()
	key, err := e2ee.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	return netproto.OneTimePreKey{KeyID: id, PublicKey: key.Public}
}

func mustEncodePreKeyFrame(t *testing.T, messageType netproto.MessageType, msg any) *netproto.Frame {
	t.Helper()
	frame, err := netproto.Encode(messageType, msg)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

type preKeyHandler func(context.Context, *Client, *netproto.Frame) error

func invokePreKeyHandler(t *testing.T, invoke preKeyHandler, frame *netproto.Frame) netproto.Error {
	t.Helper()
	serverConn, peerConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = peerConn.Close()
	})
	client := &Client{ID: "unregistered", Conn: serverConn, UniqueID: "guest"}
	done := make(chan error, 1)
	go func() { done <- invoke(context.Background(), client, frame) }()
	response := readFrame(t, peerConn)
	if err := <-done; err != nil {
		t.Fatalf("handler returned %v", err)
	}
	if netproto.MessageType(response.Type) != netproto.MsgError {
		t.Fatalf("handler response type = %s, want Error", netproto.MessageType(response.Type))
	}
	var got netproto.Error
	if err := netproto.Decode(response, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

type fakePreKeys struct {
	mu          sync.Mutex
	bundles     map[int64]*store.PreKeyBundle
	identityErr error
	publishErr  error
	consumeErr  error
	identityN   int
	publishN    int
	consumeN    int
}

func newFakePreKeys() *fakePreKeys { return &fakePreKeys{bundles: map[int64]*store.PreKeyBundle{}} }

func (f *fakePreKeys) PublishPreKeyBundle(_ context.Context, userID int64, bundle store.PreKeyBundle, oneTime []store.PreKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publishN++
	if f.publishErr != nil {
		return f.publishErr
	}
	stored := bundle
	if len(oneTime) > 0 {
		key := oneTime[0]
		stored.OneTimePreKey = &key
	}
	f.bundles[userID] = &stored
	return nil
}

func (f *fakePreKeys) ConsumePreKeyBundle(_ context.Context, userID int64) (*store.PreKeyBundle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.consumeN++
	if f.consumeErr != nil {
		return nil, f.consumeErr
	}
	stored := f.bundles[userID]
	if stored == nil {
		return nil, store.ErrNoPreKeyBundle
	}
	out := *stored
	if stored.OneTimePreKey != nil {
		key := *stored.OneTimePreKey
		out.OneTimePreKey = &key
		stored.OneTimePreKey = nil
	}
	return &out, nil
}

func (f *fakePreKeys) PreKeyIdentity(_ context.Context, userID int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.identityN++
	if f.identityErr != nil {
		return nil, f.identityErr
	}
	if bundle := f.bundles[userID]; bundle != nil {
		return bundle.IdentityDH, nil
	}
	return nil, store.ErrNoPreKeyBundle
}

func (f *fakePreKeys) identityCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.identityN
}

func (f *fakePreKeys) publishCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.publishN
}

func (f *fakePreKeys) consumeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.consumeN
}

var _ PreKeyStore = (*fakePreKeys)(nil)

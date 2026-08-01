package main

import (
	"context"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"

	"voicx/internal/auth"
	"voicx/internal/broadcast"
	"voicx/internal/config"
	"voicx/internal/permissions"
	"voicx/internal/server"
	"voicx/internal/state"
)

// fakeAuth implements server.AuthBackend for the smoke test.
type fakeAuth struct{}

func (fakeAuth) AuthenticatePassword(_ context.Context, uniqueID, password string) (bool, error) {
	return uniqueID == "lt-uid" && password == "pw", nil
}

func (fakeAuth) AuthenticateChallenge(context.Context, string, []byte, []byte) (bool, error) {
	return false, nil
}

func (fakeAuth) AuthenticateNickname(context.Context, string, string) (*auth.User, error) {
	return nil, auth.ErrUserNotFound
}

func (fakeAuth) LookupUser(_ context.Context, uniqueID string) (*auth.User, error) {
	return &auth.User{ID: 1, UniqueID: uniqueID, Nickname: "loadtest"}, nil
}

func (fakeAuth) LookupUserByPublicKey(context.Context, string) (*auth.User, error) {
	return nil, auth.ErrUserNotFound
}

func (fakeAuth) LookupActiveBan(context.Context, string, string) (*auth.Ban, error) {
	return nil, nil
}

func (fakeAuth) BindPublicKey(context.Context, int64, string) error {
	return nil
}

func (fakeAuth) SetE2EPublicKey(context.Context, int64, string) error {
	return nil
}

func (fakeAuth) GetE2EPublicKey(context.Context, string) (string, error) {
	return "", auth.ErrUserNotFound
}

// fakePerms implements server.PermLoader with an empty permission set.
type fakePerms struct{}

func (fakePerms) LoadForClient(context.Context, int64, int64) (permissions.TieredPermissions, error) {
	return permissions.NewTieredPermissions(), nil
}

func (fakePerms) Invalidate(int64, int64) {}

func (fakePerms) InvalidateAll() {}

func (fakePerms) LoadGroupPermissions(context.Context, int64) (permissions.PermissionSet, error) {
	return permissions.NewPermissionSet(), nil
}

// TestLoadtestSmoke runs the simulator against a real in-process server and
// verifies clients connect, authenticate, and send chat.
func TestLoadtestSmoke(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	logger := zap.NewNop()
	sm := state.New(logger)
	bc := broadcast.New(logger, sm)
	defer bc.Close()

	srv := server.New(&config.Config{TCPAddr: addr, ChatAllowPlaintext: true}, logger, &server.Deps{
		Auth:      fakeAuth{},
		State:     sm,
		Broadcast: bc,
		Perms:     fakePerms{},
		Resolver:  permissions.NewResolver(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	// Wait for the listener.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	var st stats
	opts := options{
		addr:     addr,
		clients:  3,
		duration: 2600 * time.Millisecond,
		ramp:     100 * time.Millisecond,
		uniqueID: "lt-uid",
		password: "pw",
		channel:  0,
	}
	if err := run(context.Background(), opts, &st); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := st.connectsOK.Load(); got != 3 {
		t.Errorf("connectsOK = %d, want 3", got)
	}
	if got := st.authOK.Load(); got != 3 {
		t.Errorf("authOK = %d, want 3", got)
	}
	if got := st.connectsFail.Load() + st.authFail.Load(); got != 0 {
		t.Errorf("failures = %d, want 0", got)
	}
	if got := st.chatSent.Load(); got == 0 {
		t.Error("no chat sent")
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("server start error: %v", err)
	}
	_ = srv.Shutdown()
}

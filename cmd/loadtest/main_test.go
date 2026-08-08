package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"voicx/internal/auth"
	"voicx/internal/broadcast"
	"voicx/internal/config"
	"voicx/internal/permissions"
	"voicx/internal/server"
	"voicx/internal/state"
	"voicx/internal/tlscert"
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

func TestReadRTPIdentifiers(t *testing.T) {
	sequence, timestamp, ssrc, err := readRTPIdentifiers(bytes.NewReader([]byte{
		0x01, 0x02,
		0x03, 0x04, 0x05, 0x06,
		0x07, 0x08, 0x09, 0x0a,
	}))
	if err != nil {
		t.Fatalf("readRTPIdentifiers: %v", err)
	}
	if sequence != 0x0102 || timestamp != 0x03040506 || ssrc != 0x0708090a {
		t.Fatalf("identifiers = (%#x, %#x, %#x)", sequence, timestamp, ssrc)
	}
}

func TestControlTLSConfigRestrictsInsecureMode(t *testing.T) {
	if _, _, err := controlTLSConfig(options{addr: "192.0.2.1:12333", tlsInsecure: true}); err == nil {
		t.Fatal("remote -tls-insecure address accepted")
	}

	cfg, enabled, err := controlTLSConfig(options{addr: "127.0.0.1:12333", tlsInsecure: true})
	if err != nil {
		t.Fatalf("loopback -tls-insecure: %v", err)
	}
	if !enabled || !cfg.InsecureSkipVerify || cfg.MinVersion != tls.VersionTLS13 {
		t.Fatalf("unexpected loopback TLS config: %+v", cfg)
	}
}

func TestPinnedTLSConfigVerifiesExactCertificate(t *testing.T) {
	cert, fingerprint, err := tlscert.Ensure(t.TempDir(), "", "", []string{"localhost"})
	if err != nil {
		t.Fatalf("generate certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}

	cfg, err := pinnedTLSConfig(fingerprint)
	if err != nil {
		t.Fatalf("pinnedTLSConfig: %v", err)
	}
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	if err := cfg.VerifyConnection(state); err != nil {
		t.Fatalf("matching certificate rejected: %v", err)
	}

	wrong := strings.Repeat("00:", 31) + "00"
	cfg, err = pinnedTLSConfig(wrong)
	if err != nil {
		t.Fatalf("wrong pin syntax: %v", err)
	}
	if err := cfg.VerifyConnection(state); err == nil {
		t.Fatal("mismatched certificate accepted")
	}
}

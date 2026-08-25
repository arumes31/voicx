package redisx

import (
	"context"
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"voicx/internal/tlscert"
)

// TestNew verifies the constructor wires the address and password into the
// underlying client options.
func TestNew(t *testing.T) {
	c, err := New(Options{
		Addr:         "localhost:6390",
		Password:     "secret",
		DialTimeout:  4 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c == nil {
		t.Fatal("New returned nil")
	}
	opts := c.Raw().Options()
	if opts.Addr != "localhost:6390" {
		t.Errorf("Addr = %q, want %q", opts.Addr, "localhost:6390")
	}
	if opts.Password != "secret" {
		t.Errorf("Password = %q, want %q", opts.Password, "secret")
	}
	if opts.DialTimeout != 4*time.Second || opts.ReadTimeout != 2*time.Second || opts.WriteTimeout != time.Second {
		t.Errorf("timeouts = %s/%s/%s", opts.DialTimeout, opts.ReadTimeout, opts.WriteTimeout)
	}
	if !opts.ContextTimeoutEnabled {
		t.Error("ContextTimeoutEnabled = false, want true")
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestPingUnreachable verifies Ping returns an error (rather than panicking
// or hanging) when no Redis server is reachable, which is what the caller
// relies on for graceful degradation.
func TestPingUnreachable(t *testing.T) {
	// Port 1 on loopback refuses connections immediately.
	c, err := New(Options{Addr: "127.0.0.1:1"}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err == nil {
		t.Fatal("Ping against unreachable Redis returned nil error")
	}
}

func TestNewTLSOptionsVerifyAndDeriveServerName(t *testing.T) {
	c, err := New(Options{Addr: "redis.example.test:6380", TLSEnabled: true}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close() }()
	config := c.Raw().Options().TLSConfig
	if config == nil || config.MinVersion != tls.VersionTLS12 || config.ServerName != "redis.example.test" {
		t.Fatalf("TLS config = %#v, want TLS 1.2 minimum and derived server name", config)
	}
	if config.InsecureSkipVerify {
		t.Fatal("Redis TLS verification is disabled")
	}
}

func TestNewTLSRejectsHostlessAddressWithoutExplicitServerName(t *testing.T) {
	if _, err := New(Options{Addr: ":6379", TLSEnabled: true}, nil); err == nil || !strings.Contains(err.Error(), "set redis_tls_server_name") {
		t.Fatalf("New hostless TLS address error = %v, want actionable redis_tls_server_name guidance", err)
	}
}

func TestNewTLSCustomCAAndBadMaterial(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := tlscert.Ensure(dir, "", "", nil); err != nil {
		t.Fatalf("generate test certificate: %v", err)
	}
	caFile := filepath.Join(dir, "cert.pem")
	c, err := New(Options{Addr: "127.0.0.1:6380", TLSEnabled: true, TLSCAFile: caFile}, nil)
	if err != nil {
		t.Fatalf("New valid CA: %v", err)
	}
	if c.Raw().Options().TLSConfig.RootCAs == nil {
		t.Fatal("custom CA did not configure root pool")
	}
	_ = c.Close()

	for _, test := range []struct {
		name string
		path string
	}{
		{name: "unreadable", path: filepath.Join(dir, "missing.pem")},
		{name: "malformed", path: filepath.Join(dir, "bad.pem")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "malformed" {
				if err := os.WriteFile(test.path, []byte("not PEM"), 0o600); err != nil {
					t.Fatalf("write malformed CA: %v", err)
				}
			}
			if _, err := New(Options{Addr: "127.0.0.1:6380", TLSEnabled: true, TLSCAFile: test.path}, nil); err == nil {
				t.Fatal("bad TLS material was accepted")
			}
		})
	}
}

func TestNewRejectsTLSOnlyFieldsWhenTLSDisabled(t *testing.T) {
	if _, err := New(Options{Addr: "127.0.0.1:6379", TLSServerName: "redis.example.test"}, nil); err == nil {
		t.Fatal("TLS-only option was accepted with TLS disabled")
	}
}

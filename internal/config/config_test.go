package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadFromEnv verifies that VOICX_* environment variables override the
// built-in defaults and are reflected in the returned Config.
func TestLoadFromEnv(t *testing.T) {
	t.Setenv("VOICX_SERVER_NAME", "env-server")
	t.Setenv("VOICX_LOG_LEVEL", "warn")
	t.Setenv("VOICX_DEV_MODE", "false")
	t.Setenv("VOICX_TCP_ADDR", ":11111")
	t.Setenv("VOICX_UDP_ADDR", ":22222")
	t.Setenv("VOICX_GRPC_ADDR", ":33333")
	t.Setenv("VOICX_DATABASE_URL", "postgres://user:pass@db:5432/x")
	t.Setenv("VOICX_REDIS_ADDR", "redis:6379")
	t.Setenv("VOICX_REDIS_PASSWORD", "secret")
	t.Setenv("VOICX_MAX_CLIENTS", "512")

	// Ensure no config.yaml is picked up from the working directory by
	// running from a clean temp dir.
	dir := t.TempDir()
	t.Setenv("PWD", dir)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cases := []struct {
		name string
		got  any
		want any
	}{
		{"ServerName", cfg.ServerName, "env-server"},
		{"LogLevel", cfg.LogLevel, "warn"},
		{"DevMode", cfg.DevMode, false},
		{"TCPAddr", cfg.TCPAddr, ":11111"},
		{"UDPAddr", cfg.UDPAddr, ":22222"},
		{"GRPCAddr", cfg.GRPCAddr, ":33333"},
		{"DatabaseURL", cfg.DatabaseURL, "postgres://user:pass@db:5432/x"},
		{"RedisAddr", cfg.RedisAddr, "redis:6379"},
		{"RedisPassword", cfg.RedisPassword, "secret"},
		{"MaxClients", cfg.MaxClients, 512},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestLoadDefaults verifies that Load returns sensible defaults when no
// environment variables or config file are present.
func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Clear any VOICX_* env vars that might leak from the test environment.
	for _, kv := range os.Environ() {
		if len(kv) >= 6 && kv[:6] == "VOICX_" {
			if eq := indexOf(kv, '='); eq >= 0 {
				t.Setenv(kv[:eq], "")
			}
		}
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.ServerName != "voicx" {
		t.Errorf("ServerName = %q, want %q", cfg.ServerName, "voicx")
	}
	if cfg.TCPAddr != DefaultTCPAddr {
		t.Errorf("TCPAddr = %q, want %q", cfg.TCPAddr, DefaultTCPAddr)
	}
	if cfg.UDPAddr != DefaultUDPAddr {
		t.Errorf("UDPAddr = %q, want %q", cfg.UDPAddr, DefaultUDPAddr)
	}
	if cfg.GRPCAddr != DefaultGRPCAddr {
		t.Errorf("GRPCAddr = %q, want %q", cfg.GRPCAddr, DefaultGRPCAddr)
	}
	if cfg.HealthAddr != DefaultHealthAddr || cfg.QueryAddr != DefaultQueryAddr ||
		cfg.QuerySSHAddr != DefaultQuerySSHAddr || cfg.FileAddr != DefaultFileAddr {
		t.Errorf("remaining listener defaults = health %q, query %q, query SSH %q, file %q",
			cfg.HealthAddr, cfg.QueryAddr, cfg.QuerySSHAddr, cfg.FileAddr)
	}
	if cfg.MaxClients != 1024 {
		t.Errorf("MaxClients = %d, want 1024", cfg.MaxClients)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
	if cfg.DevMode != true {
		t.Errorf("DevMode = %v, want true", cfg.DevMode)
	}
	if len(cfg.WebRTC.ICEServers) == 0 {
		t.Error("WebRTC.ICEServers is empty, want default STUN server")
	}
}

func TestLoadRejectsInvalidFileQuietHours(t *testing.T) {
	for _, tc := range []struct {
		name, env, value string
	}{
		{"start below range", "VOICX_FILE_QUIET_HOURS_START", "-1"},
		{"start above range", "VOICX_FILE_QUIET_HOURS_START", "24"},
		{"end below range", "VOICX_FILE_QUIET_HOURS_END", "-1"},
		{"end above range", "VOICX_FILE_QUIET_HOURS_END", "24"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			wd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chdir(wd) })
			if err := os.Chdir(dir); err != nil {
				t.Fatal(err)
			}
			t.Setenv(tc.env, tc.value)
			if _, err := Load(); err == nil {
				t.Fatalf("Load accepted %s=%s", tc.env, tc.value)
			}
		})
	}
}

// TestLoadFromYAML verifies that a config.yaml in the working directory
// overrides the built-in defaults.
func TestLoadFromYAML(t *testing.T) {
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	yaml := []byte(`server_name: yaml-server
tcp_addr: ":20011"
udp_addr: ":19987"
max_clients: 256
dev_mode: false
log_level: "error"
webrtc:
  enable_av1: true
  ice_servers:
    - "stun:stun.example.com:19302"
`)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.ServerName != "yaml-server" {
		t.Errorf("ServerName = %q, want %q", cfg.ServerName, "yaml-server")
	}
	if cfg.TCPAddr != ":20011" {
		t.Errorf("TCPAddr = %q, want %q", cfg.TCPAddr, ":20011")
	}
	if cfg.UDPAddr != ":19987" {
		t.Errorf("UDPAddr = %q, want %q", cfg.UDPAddr, ":19987")
	}
	if cfg.MaxClients != 256 {
		t.Errorf("MaxClients = %d, want 256", cfg.MaxClients)
	}
	if cfg.DevMode != false {
		t.Errorf("DevMode = %v, want false", cfg.DevMode)
	}
	if cfg.LogLevel != "error" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "error")
	}
	if !cfg.WebRTC.EnableAV1 {
		t.Error("WebRTC.EnableAV1 = false, want true")
	}
	if len(cfg.WebRTC.ICEServers) != 1 || cfg.WebRTC.ICEServers[0] != "stun:stun.example.com:19302" {
		t.Errorf("WebRTC.ICEServers = %v, want [stun:stun.example.com:19302]", cfg.WebRTC.ICEServers)
	}
}

// TestSummary verifies that Summary returns a non-empty, informative string.
func TestSummary(t *testing.T) {
	cfg := &Config{
		ServerName:  "voicx",
		LogLevel:    "info",
		DevMode:     true,
		TCPAddr:     DefaultTCPAddr,
		UDPAddr:     DefaultUDPAddr,
		GRPCAddr:    DefaultGRPCAddr,
		DatabaseURL: "postgres://localhost",
		RedisAddr:   "localhost:6379",
		MaxClients:  1024,
		WebRTC: WebRTCConfig{
			ICEServers: []string{"stun:stun.l.google.com:19302"},
			EnableAV1:  false,
		},
	}
	s := cfg.Summary()
	if s == "" {
		t.Error("Summary() returned empty string")
	}
	if !contains(s, "name=voicx") {
		t.Errorf("Summary() = %q, want it to contain name=voicx", s)
	}
}

// indexOf returns the index of the first occurrence of b in s, or -1.
func indexOf(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// contains reports whether substr is in s.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || indexOfString(s, substr) >= 0)
}

func indexOfString(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

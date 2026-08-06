package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoadFromEnv verifies that VOICX_* environment variables override the
// built-in defaults and are reflected in the returned Config.
func TestLoadFromEnv(t *testing.T) {
	t.Setenv("VOICX_SERVER_NAME", "env-server")
	t.Setenv("VOICX_LOG_LEVEL", "warn")
	t.Setenv("VOICX_DEV_MODE", "false")
	t.Setenv("VOICX_TCP_ADDR", ":11111")
	t.Setenv("VOICX_UDP_ADDR", ":22222")
	t.Setenv("VOICX_GRPC_ADDR", "127.0.0.1:33333")
	t.Setenv("VOICX_QUERY_ADDR", ":44444")
	t.Setenv("VOICX_QUERY_ALLOW_REMOTE", "true")
	t.Setenv("VOICX_METRICS_ALLOW_REMOTE", "true")
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
		{"GRPCAddr", cfg.GRPCAddr, "127.0.0.1:33333"},
		{"QueryAddr", cfg.QueryAddr, ":44444"},
		{"QueryAllowRemote", cfg.QueryAllowRemote, true},
		{"MetricsAllowRemote", cfg.MetricsAllowRemote, true},
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
	if cfg.QueryAllowRemote || cfg.MetricsAllowRemote {
		t.Errorf(
			"remote admin/metrics opt-ins = %t/%t, want false/false",
			cfg.QueryAllowRemote,
			cfg.MetricsAllowRemote,
		)
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

func TestRepositorySampleConfigLoads(t *testing.T) {
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repository root: %v", err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(repositoryRoot); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "VOICX_") {
			if eq := strings.IndexByte(kv, '='); eq >= 0 {
				t.Setenv(kv[:eq], "")
			}
		}
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load repository config.yaml: %v", err)
	}
	if cfg.ChatMaxLength != 4096 || cfg.QueryAddr != DefaultQueryAddr || !cfg.FileTLSEnabled {
		t.Fatalf(
			"sample drift: chat_max_length=%d query_addr=%q file_tls_enabled=%t",
			cfg.ChatMaxLength,
			cfg.QueryAddr,
			cfg.FileTLSEnabled,
		)
	}
}

func TestValidateRejectsUnsafeValues(t *testing.T) {
	base := loadDefaultConfig(t)
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "invalid log level", mutate: func(c *Config) { c.LogLevel = "verbose" }, wantErr: "log_level"},
		{name: "malformed listener", mutate: func(c *Config) { c.TCPAddr = "localhost" }, wantErr: "tcp_addr"},
		{name: "invalid listener port", mutate: func(c *Config) { c.UDPAddr = ":70000" }, wantErr: "udp_addr"},
		{name: "remote plaintext grpc", mutate: func(c *Config) { c.GRPCAddr = ":12338" }, wantErr: "grpc_addr"},
		{
			name:    "remote query without opt in",
			mutate:  func(c *Config) { c.QueryAddr = ":12335" },
			wantErr: "query_allow_remote",
		},
		{name: "zero clients", mutate: func(c *Config) { c.MaxClients = 0 }, wantErr: "max_clients"},
		{
			name:    "zero client timeout",
			mutate:  func(c *Config) { c.ClientTimeoutSeconds = 0 },
			wantErr: "client_timeout_seconds",
		},
		{name: "negative file limit", mutate: func(c *Config) { c.FileMaxSizeMB = -1 }, wantErr: "file_max_size_mb"},
		{
			name:    "idle pool exceeds open",
			mutate:  func(c *Config) { c.DBMaxIdleConns = c.DBMaxOpenConns + 1 },
			wantErr: "must not exceed",
		},
		{name: "zero open pool", mutate: func(c *Config) { c.DBMaxOpenConns = 0 }, wantErr: "db_max_open_conns"},
		{name: "zero chat length", mutate: func(c *Config) { c.ChatMaxLength = 0 }, wantErr: "chat_max_length"},
		{
			name:    "zero chat rate window",
			mutate:  func(c *Config) { c.ChatRateWindowSeconds = 0 },
			wantErr: "chat_rate_window_seconds",
		},
		{
			name:    "negative chat rotation",
			mutate:  func(c *Config) { c.ChatKeyRotateMinSecs = -1 },
			wantErr: "chat_key_rotate_min_seconds",
		},
		{
			name:    "zero search cap",
			mutate:  func(c *Config) { c.ChatSearchMaxMessages = 0 },
			wantErr: "chat_search_max_messages",
		},
		{name: "zero turn ttl", mutate: func(c *Config) { c.TURN.CredentialsTTL = 0 }, wantErr: "turn.credentials_ttl"},
		{
			name:    "excessive turn ttl",
			mutate:  func(c *Config) { c.TURN.CredentialsTTL = 31 * 24 * time.Hour },
			wantErr: "turn.credentials_ttl",
		},
		{
			name:    "invalid ice scheme",
			mutate:  func(c *Config) { c.WebRTC.ICEServers = []string{"https://ice.example.com"} },
			wantErr: "webrtc.ice_servers",
		},
		{
			name:    "invalid turn scheme",
			mutate:  func(c *Config) { c.TURN.URIs = []string{"stun:stun.example.com"} },
			wantErr: "turn.uris",
		},
		{
			name:    "invalid recording format",
			mutate:  func(c *Config) { c.Recording.Format = "mkv" },
			wantErr: "recording.format",
		},
		{
			name:    "certificate without key",
			mutate:  func(c *Config) { c.TLSCertFile = "cert.pem" },
			wantErr: "must be set together",
		},
		{
			name: "production plaintext control",
			mutate: func(c *Config) {
				c.DevMode = false
				c.TLSEnabled = false
			},
			wantErr: "tls_enabled",
		},
		{
			name: "production plaintext files",
			mutate: func(c *Config) {
				c.DevMode = false
				c.FileTLSEnabled = false
			},
			wantErr: "file_tls_enabled",
		},
		{
			name: "production plaintext chat",
			mutate: func(c *Config) {
				c.DevMode = false
				c.ChatAllowPlaintext = true
			},
			wantErr: "chat_allow_plaintext",
		},
		{name: "negative udp rate", mutate: func(c *Config) { c.UDPRateLimitPPS = -1 }, wantErr: "udp_rate_limit_pps"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := cloneConfig(base)
			test.mutate(cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() error = %v, want text %q", err, test.wantErr)
			}
		})
	}
}

func TestValidateLoopbackAndRemoteQueryOptIn(t *testing.T) {
	base := loadDefaultConfig(t)
	tests := []struct {
		name        string
		grpcAddr    string
		queryAddr   string
		allowRemote bool
	}{
		{name: "IPv4 loopback", grpcAddr: "127.0.0.1:12338", queryAddr: "127.0.0.1:12335"},
		{name: "IPv6 loopback", grpcAddr: "[::1]:12338", queryAddr: "[::1]:12335"},
		{name: "localhost", grpcAddr: "localhost:12338", queryAddr: "localhost:12335"},
		{name: "explicit remote query", grpcAddr: "127.0.0.1:12338", queryAddr: ":12335", allowRemote: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := cloneConfig(base)
			cfg.GRPCAddr = test.grpcAddr
			cfg.QueryAddr = test.queryAddr
			cfg.QueryAllowRemote = test.allowRemote
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestWarningsReportExplicitUnsafeChoices(t *testing.T) {
	cfg := loadDefaultConfig(t)
	cfg.QueryAddr = ":12335"
	cfg.QueryAllowRemote = true
	cfg.MetricsAllowRemote = true
	cfg.TLSEnabled = false
	cfg.FileTLSEnabled = false
	cfg.ChatAllowPlaintext = true

	warnings := strings.Join(cfg.Warnings(), "\n")
	for _, text := range []string{
		"query_allow_remote",
		"metrics_allow_remote",
		"tls_enabled=false",
		"file transfers are plaintext",
		"chat_allow_plaintext",
	} {
		if !strings.Contains(warnings, text) {
			t.Errorf("Warnings() = %q, want text %q", warnings, text)
		}
	}
}

// TestSummary verifies that Summary is informative without exposing endpoint
// credentials or ICE URLs.
func TestSummary(t *testing.T) {
	cfg := &Config{
		ServerName:    "voicx",
		LogLevel:      "info",
		DevMode:       true,
		TCPAddr:       DefaultTCPAddr,
		UDPAddr:       DefaultUDPAddr,
		GRPCAddr:      DefaultGRPCAddr,
		DatabaseURL:   "postgres://db-user:db-password@db:5432/voicx?sslmode=require&token=db-token",
		RedisAddr:     "redis://redis-user:redis-password@redis:6379/0?secret=redis-token",
		RedisPassword: "separate-redis-password",
		MaxClients:    1024,
		WebRTC: WebRTCConfig{
			ICEServers: []string{"turn:ice-user:ice-password@turn.example.com:3478"},
			EnableAV1:  false,
		},
	}
	s := cfg.Summary()
	if s == "" {
		t.Error("Summary() returned empty string")
	}
	if !strings.Contains(s, `name="voicx"`) || !strings.Contains(s, "db=\"postgres://db:5432/voicx?sslmode=require\"") {
		t.Errorf("Summary() = %q, want safe identifying fields", s)
	}
	for _, secret := range []string{
		"db-user",
		"db-password",
		"db-token",
		"redis-user",
		"redis-password",
		"redis-token",
		"separate-redis-password",
		"ice-user",
		"ice-password",
	} {
		if strings.Contains(s, secret) {
			t.Errorf("Summary() leaked %q: %s", secret, s)
		}
	}
}

func loadDefaultConfig(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "VOICX_") {
			if eq := strings.IndexByte(kv, '='); eq >= 0 {
				t.Setenv(kv[:eq], "")
			}
		}
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func cloneConfig(source *Config) *Config {
	clone := *source
	clone.TURN.URIs = append([]string{}, source.TURN.URIs...)
	clone.WebRTC.ICEServers = append([]string{}, source.WebRTC.ICEServers...)
	clone.Recording.VideoArgs = append([]string{}, source.Recording.VideoArgs...)
	clone.Recording.AudioArgs = append([]string{}, source.Recording.AudioArgs...)
	return &clone
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

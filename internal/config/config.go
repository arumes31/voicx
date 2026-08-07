package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config holds all runtime configuration for the voicx server.
// Values are populated from (in order of precedence):
//  1. environment variables prefixed with VOICX_ (e.g. VOICX_TCP_ADDR)
//  2. config.yaml at the project root (if present)
//  3. the defaults defined in Load().
type Config struct {
	ServerName     string `mapstructure:"server_name"`
	ServerPassword string `mapstructure:"server_password"`
	LogLevel       string `mapstructure:"log_level"`
	DevMode        bool   `mapstructure:"dev_mode"`
	TCPAddr        string `mapstructure:"tcp_addr"`
	UDPAddr        string `mapstructure:"udp_addr"`
	GRPCAddr       string `mapstructure:"grpc_addr"`
	HealthAddr     string `mapstructure:"health_addr"`
	QueryAddr      string `mapstructure:"query_addr"`
	// QueryAllowRemote is an explicit acknowledgement that the plaintext
	// ServerQuery listener may bind beyond loopback. Prefer QuerySSH instead.
	QueryAllowRemote bool `mapstructure:"query_allow_remote"`
	// ServerQuery over SSH (224): the same command set and the same
	// credentials as QueryAddr, wrapped in an SSH transport. Off by default.
	// QuerySSHHostKey is generated on first start and must persist, or every
	// client reports a changed host identity after a restart.
	QuerySSHEnabled    bool   `mapstructure:"query_ssh_enabled"`
	QuerySSHAddr       string `mapstructure:"query_ssh_addr"`
	QuerySSHHostKey    string `mapstructure:"query_ssh_host_key"`
	FileAddr           string `mapstructure:"file_addr"`
	FileRoot           string `mapstructure:"file_root"`
	FileMaxKBps        int    `mapstructure:"file_max_kbps"`
	FileChannelQuotaMB int64  `mapstructure:"file_channel_quota_mb"`
	FileMaxSizeMB      int64  `mapstructure:"file_max_size_mb"`
	// Quiet hours lift file_max_kbps between these local hours (276), so
	// backups and big uploads run at full speed when nobody is listening.
	// Both are 0-23; equal values disable the window. A start after the end
	// wraps past midnight (22 -> 6).
	FileQuietHoursStart int    `mapstructure:"file_quiet_hours_start"`
	FileQuietHoursEnd   int    `mapstructure:"file_quiet_hours_end"`
	DatabaseURL         string `mapstructure:"database_url"`
	PIIKeyFile          string `mapstructure:"pii_key_file"`
	RedisAddr           string `mapstructure:"redis_addr"`
	RedisPassword       string `mapstructure:"redis_password"`
	RedisEnabled        bool   `mapstructure:"redis_enabled"`
	// MetricsAllowRemote exposes /metrics beyond loopback on HealthAddr.
	// Liveness and readiness remain reachable wherever HealthAddr is bound.
	MetricsAllowRemote   bool `mapstructure:"metrics_allow_remote"`
	MaxClients           int  `mapstructure:"max_clients"`
	ClientTimeoutSeconds int  `mapstructure:"client_timeout_seconds"`
	DefaultOpusBitrate   int  `mapstructure:"default_opus_bitrate"`
	DefaultOpusFEC       bool `mapstructure:"default_opus_fec"`
	DefaultOpusDTX       bool `mapstructure:"default_opus_dtx"`
	DefaultOpusStereo    bool `mapstructure:"default_opus_stereo"`
	// EchoChannelName is the name of the loopback test channel: the server
	// ensures it exists at startup, and publishers in it hear their own audio
	// routed back (the echo channel is the only channel with self-fan-out).
	EchoChannelName string `mapstructure:"echo_channel_name"`

	// ChannelTempLifetimeSeconds is the grace period an empty temporary
	// channel survives before it is deleted (165). Zero or negative falls
	// back to channels.DefaultCleanupDelay.
	ChannelTempLifetimeSeconds int `mapstructure:"channel_temp_lifetime_seconds"`

	// TLS protects the TCP control channel with TLS 1.3. Enabled by default;
	// when CertFile/KeyFile are empty a self-signed ECDSA P-256 certificate
	// is generated into TLSDir on first start and reused afterwards (TOFU
	// pinning on clients needs a stable fingerprint).
	TLSEnabled  bool   `mapstructure:"tls_enabled"`
	TLSDir      string `mapstructure:"tls_dir"`
	TLSCertFile string `mapstructure:"tls_cert_file"`
	TLSKeyFile  string `mapstructure:"tls_key_file"`

	// ChatAllowPlaintext permits unencrypted chat SENDS (dev escape hatch;
	// default false). It never affects storage, relay, history or MOTD — the
	// server seals before storing and before broadcasting regardless (91).
	ChatAllowPlaintext bool `mapstructure:"chat_allow_plaintext"`

	// Chat encryption at rest (91). ChatMasterKeyFile holds the key-encryption
	// key that wraps every stored scope generation; it lives OUTSIDE the
	// database and must be backed up with it — losing it destroys all channel
	// and global history irreversibly. VOICX_CHAT_MASTER_KEY overrides it.
	// ChatLegacyHistory selects what the one-time backfill does with pre-012
	// plaintext rows: "encrypt" (default, sealed in place) or "purge".
	// ChatKeyRotateMinSecs coalesces channel key rotations so a flapping
	// client cannot mint one persisted generation per reconnect.
	// ChatSearchMaxMessages caps the client-side search scan (110).
	ChatMasterKeyFile     string `mapstructure:"chat_master_key_file"`
	ChatLegacyHistory     string `mapstructure:"chat_legacy_history"`
	ChatKeyRotateMinSecs  int    `mapstructure:"chat_key_rotate_min_seconds"`
	ChatSearchMaxMessages int    `mapstructure:"chat_search_max_messages"`

	// FileTLSEnabled wraps the file-transfer data port in TLS 1.3 with the SAME
	// certificate as the control channel. false is a dev-only escape hatch.
	FileTLSEnabled bool `mapstructure:"file_tls_enabled"`

	// ServerInfoMOTD includes the MOTD in the server-info reply (313) as
	// PLAINTEXT. Off by default: the reply is authenticated-only and the
	// sealed MOTD already rides the AuthResponse, so serving it again in the
	// clear would be the one path that escapes ciphertext-at-rest (91-135).
	// Enable only for a deliberately public, non-sensitive MOTD.
	ServerInfoMOTD bool `mapstructure:"server_info_motd"`

	// DefaultGroupsEnabled auto-creates the Guest/Member server groups and
	// auto-assigns them (143/144). Guests virtually hold the Guest group's
	// permissions; registered users get Member at first login.
	DefaultGroupsEnabled bool `mapstructure:"default_groups_enabled"`

	// Chat moderation/limits (wave 5a). MaxLength is in UTF-8 bytes
	// post-decrypt. RateMsgs/RateWindowSeconds is a per-user token bucket.
	//
	// WordFilter/LinkBlacklist/LinkWhitelist are comma-separated and
	// case-insensitive. WordFilter entries are SUBSTRINGS of the message; the
	// link lists are HOSTS compared against the hostname of each http(s) URL
	// in the message (exact or subdomain), and a non-empty whitelist means
	// ONLY those hosts may be linked. Filters apply to channel/global scopes
	// only (DMs are E2EE and exempt).
	//
	// These three are BOOT DEFAULTS only (117/118): they apply until an
	// operator stores a runtime override through MsgChatFilterSet, after which
	// the persisted chat_filters server setting wins and editing config.yaml
	// has no effect. Restarting does not restore them.
	ChatMaxLength         int    `mapstructure:"chat_max_length"`
	ChatRateMsgs          int    `mapstructure:"chat_rate_msgs"`
	ChatRateWindowSeconds int    `mapstructure:"chat_rate_window_seconds"`
	ChatWordFilter        string `mapstructure:"chat_word_filter"`
	ChatLinkBlacklist     string `mapstructure:"chat_link_blacklist"`
	ChatLinkWhitelist     string `mapstructure:"chat_link_whitelist"`

	// TURN holds TURN server settings for NAT traversal (445/446). When
	// Secret is set, the server mints time-limited coturn REST API
	// credentials and delivers them to clients in the auth response.
	TURN TURNConfig `mapstructure:"turn"`

	// Database connection pool tuning.
	DBMaxOpenConns    int           `mapstructure:"db_max_open_conns"`
	DBMaxIdleConns    int           `mapstructure:"db_max_idle_conns"`
	DBConnMaxLifetime time.Duration `mapstructure:"db_conn_max_lifetime"`

	WebRTC WebRTCConfig `mapstructure:"webrtc"`

	// UDP per-source-IP rate limiting (DDoS mitigation).
	UDPRateLimitPPS int `mapstructure:"udp_rate_limit_pps"`
	UDPRateBurst    int `mapstructure:"udp_rate_burst"`

	Recording RecordingConfig `mapstructure:"recording"`
}

// RecordingConfig holds configuration for server-side stream recording via
// an ffmpeg subprocess (see internal/recorder).
type RecordingConfig struct {
	// Enabled gates recording entirely. Default false.
	Enabled bool `mapstructure:"enabled"`
	// Dir is where SDP and output files are written.
	Dir string `mapstructure:"dir"`
	// FFmpegPath is the ffmpeg binary to run.
	FFmpegPath string `mapstructure:"ffmpeg_path"`
	// Format is the output container: "webm" or "mp4".
	Format string `mapstructure:"format"`
	// VideoArgs are the ffmpeg video output options; set e.g.
	// ["-c:v", "h264_nvenc"] for hardware encoding.
	VideoArgs []string `mapstructure:"video_args"`
	// AudioArgs are the ffmpeg audio output options.
	AudioArgs []string `mapstructure:"audio_args"`
	// MaxConcurrent bounds starting plus active ffmpeg subprocesses.
	MaxConcurrent int `mapstructure:"max_concurrent"`
	// WindowsACLReady acknowledges that Dir was provisioned with a restricted,
	// inheritable NTFS DACL; chmod alone cannot enforce it on Windows.
	WindowsACLReady bool `mapstructure:"windows_acl_ready"`
}

// TURNConfig holds TURN server settings (coturn REST API auth, 445/446).
// TURN is only needed for clients behind restrictive NATs; when Secret is
// empty no TURN entries are delivered to clients.
type TURNConfig struct {
	// Secret is the coturn static auth secret (use-auth-secret). Empty
	// disables TURN credential minting.
	Secret string `mapstructure:"secret"`
	// Realm is the TURN realm (informational; must match the coturn realm).
	Realm string `mapstructure:"realm"`
	// URIs are the TURN server URIs given to clients, e.g.
	// ["turn:turn.example.com:12340?transport=udp", "turn:...?transport=tcp"].
	URIs []string `mapstructure:"uris"`
	// CredentialsTTL is how long minted credentials stay valid.
	CredentialsTTL time.Duration `mapstructure:"credentials_ttl"`
}

// WebRTCConfig holds configuration for the Pion WebRTC engine.
type WebRTCConfig struct {
	// ICEServers is the list of STUN/TURN server URLs used when creating peer
	// connections. If empty, the engine falls back to
	// "stun:stun.l.google.com:19302".
	ICEServers []string `mapstructure:"ice_servers"`
	// EnableAV1 controls whether the AV1 video codec is registered on the
	// MediaEngine. AV1 is optional because not all clients support it yet.
	EnableAV1 bool `mapstructure:"enable_av1"`
}

// Load reads configuration from environment variables (VOICX_ prefix),
// an optional config.yaml (searched in the working directory first, then
// /etc/voicx), and built-in defaults. It returns a typed *Config or an error
// if configuration could not be unmarshalled.
func Load() (*Config, error) {
	v := viper.New()

	// Defaults -----------------------------------------------------------------
	v.SetDefault("server_name", "voicx")
	v.SetDefault("server_password", "")
	v.SetDefault("log_level", "info")
	v.SetDefault("dev_mode", true)
	v.SetDefault("tcp_addr", DefaultTCPAddr)
	v.SetDefault("udp_addr", DefaultUDPAddr)
	v.SetDefault("grpc_addr", DefaultGRPCAddr)
	v.SetDefault("health_addr", DefaultHealthAddr)
	v.SetDefault("query_addr", DefaultQueryAddr)
	v.SetDefault("query_allow_remote", false)
	v.SetDefault("query_ssh_enabled", false)
	v.SetDefault("query_ssh_addr", DefaultQuerySSHAddr)
	v.SetDefault("query_ssh_host_key", "./data/keys/query_ssh_host.key")
	v.SetDefault("file_addr", DefaultFileAddr)
	v.SetDefault("file_root", "./data/files")
	v.SetDefault("file_max_kbps", 0)
	v.SetDefault("file_channel_quota_mb", 0)
	v.SetDefault("file_max_size_mb", 100)
	v.SetDefault("file_quiet_hours_start", 0)
	v.SetDefault("file_quiet_hours_end", 0)
	v.SetDefault("database_url", "postgres://voicx:voicx@localhost:5432/voicx?sslmode=disable")
	v.SetDefault("redis_addr", "localhost:6379")
	v.SetDefault("redis_password", "")
	v.SetDefault("redis_enabled", true)
	v.SetDefault("metrics_allow_remote", false)
	v.SetDefault("max_clients", 1024)
	v.SetDefault("echo_channel_name", "Echo Test")
	// 60s matches channels.DefaultCleanupDelay (165).
	v.SetDefault("channel_temp_lifetime_seconds", 60)

	// TLS defaults: on, self-signed cert under ./data/tls (the Docker image
	// points tls_dir at the /data volume).
	v.SetDefault("tls_enabled", true)
	v.SetDefault("tls_dir", "./data/tls")
	v.SetDefault("tls_cert_file", "")
	v.SetDefault("tls_key_file", "")
	v.SetDefault("chat_allow_plaintext", false)
	v.SetDefault("chat_master_key_file", "./data/keys/chat_master.key")
	v.SetDefault("pii_key_file", "./data/keys/pii.key")
	v.SetDefault("chat_legacy_history", "encrypt")
	v.SetDefault("chat_key_rotate_min_seconds", 60)
	v.SetDefault("chat_search_max_messages", 2000)
	v.SetDefault("file_tls_enabled", true)
	v.SetDefault("server_info_motd", false)
	v.SetDefault("default_groups_enabled", true)
	v.SetDefault("chat_max_length", 4096)
	v.SetDefault("client_timeout_seconds", 90)
	v.SetDefault("default_opus_bitrate", 32000)
	v.SetDefault("default_opus_fec", true)
	v.SetDefault("default_opus_dtx", false)
	v.SetDefault("default_opus_stereo", false)
	v.SetDefault("chat_rate_msgs", 5)
	v.SetDefault("chat_rate_window_seconds", 3)
	v.SetDefault("chat_word_filter", "")
	v.SetDefault("chat_link_blacklist", "")
	v.SetDefault("chat_link_whitelist", "")

	// TURN defaults: disabled (no secret) until coturn is deployed.
	v.SetDefault("turn.secret", "")
	v.SetDefault("turn.realm", "voicx")
	v.SetDefault("turn.uris", []string{})
	v.SetDefault("turn.credentials_ttl", 24*time.Hour)

	// Database pool defaults -------------------------------------------------
	v.SetDefault("db_max_open_conns", 25)
	v.SetDefault("db_max_idle_conns", 5)
	v.SetDefault("db_conn_max_lifetime", 30*time.Minute)

	// WebRTC defaults ---------------------------------------------------------
	v.SetDefault("webrtc.ice_servers", []string{"stun:stun.l.google.com:19302"})
	v.SetDefault("webrtc.enable_av1", false)

	// UDP rate limiting defaults ----------------------------------------------
	v.SetDefault("udp_rate_limit_pps", 200)
	v.SetDefault("udp_rate_burst", 400)

	// Recording defaults ------------------------------------------------------
	v.SetDefault("recording.enabled", false)
	v.SetDefault("recording.dir", "recordings")
	v.SetDefault("recording.ffmpeg_path", "ffmpeg")
	v.SetDefault("recording.format", "webm")
	v.SetDefault("recording.video_args", []string{"-c:v", "copy"})
	v.SetDefault("recording.audio_args", []string{"-c:a", "copy"})
	v.SetDefault("recording.max_concurrent", 4)
	v.SetDefault("recording.windows_acl_ready", false)

	// config.yaml: the working directory wins; /etc/voicx is the system
	// location used by the Docker image. The first file found is used.
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(".")
	v.AddConfigPath("/etc/voicx")
	if err := v.ReadInConfig(); err != nil {
		// Missing config file is fine; we fall back to env + defaults.
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("reading config file: %w", err)
		}
	}

	// Environment variables: VOICX_<UPPER_SNAKE_FIELD> ------------------------
	v.SetEnvPrefix("VOICX")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	cfg := &Config{}
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("unmarshalling config: %w", err)
	}

	cfg.LogLevel = strings.ToLower(strings.TrimSpace(cfg.LogLevel))
	cfg.ChatLegacyHistory = strings.ToLower(strings.TrimSpace(cfg.ChatLegacyHistory))
	cfg.Recording.Format = strings.ToLower(strings.TrimSpace(cfg.Recording.Format))

	switch cfg.ChatLegacyHistory {
	case "encrypt", "purge":
	default:
		return nil, fmt.Errorf("invalid chat_legacy_history %q: must be \"encrypt\" or \"purge\"", cfg.ChatLegacyHistory)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return cfg, nil
}

const maxTURNCredentialsTTL = 30 * 24 * time.Hour

// Validate checks configuration values before any listener or resource is
// created. It returns every independent problem so operators can fix a bad
// deployment in one pass.
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("config is nil")
	}

	errs := []error{}
	if strings.TrimSpace(c.ServerName) == "" {
		errs = append(errs, errors.New("server_name must not be empty"))
	}
	switch strings.ToLower(strings.TrimSpace(c.LogLevel)) {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log_level %q must be debug, info, warn, or error", c.LogLevel))
	}

	addresses := []struct {
		name  string
		value string
	}{
		{name: "tcp_addr", value: c.TCPAddr},
		{name: "udp_addr", value: c.UDPAddr},
		{name: "grpc_addr", value: c.GRPCAddr},
		{name: "health_addr", value: c.HealthAddr},
		{name: "query_addr", value: c.QueryAddr},
		{name: "query_ssh_addr", value: c.QuerySSHAddr},
		{name: "file_addr", value: c.FileAddr},
	}
	for _, address := range addresses {
		if err := validateAddress(address.name, address.value); err != nil {
			errs = append(errs, err)
		}
	}
	if err := validateLoopbackAddress("grpc_addr", c.GRPCAddr); err != nil {
		errs = append(errs, fmt.Errorf("%w because the gRPC listener is plaintext", err))
	}
	if !isLoopbackAddress(c.QueryAddr) && !c.QueryAllowRemote {
		errs = append(errs, errors.New(
			"query_addr must be loopback-only unless query_allow_remote=true; "+
				"prefer query_ssh_enabled for remote administration",
		))
	}

	if err := validateDatabaseURL(c.DatabaseURL); err != nil {
		errs = append(errs, err)
	} else if !c.DevMode {
		if err := validateProductionDatabaseURL(c.DatabaseURL); err != nil {
			errs = append(errs, err)
		}
	}
	if c.RedisEnabled {
		if err := validateAddress("redis_addr", c.RedisAddr); err != nil {
			errs = append(errs, err)
		}
	}

	if c.MaxClients <= 0 || c.MaxClients > 100_000 {
		errs = append(errs, fmt.Errorf("max_clients %d must be between 1 and 100000", c.MaxClients))
	}
	if c.ClientTimeoutSeconds <= 0 {
		errs = append(errs, fmt.Errorf("client_timeout_seconds %d must be positive", c.ClientTimeoutSeconds))
	}
	if c.DefaultOpusBitrate < 6_000 || c.DefaultOpusBitrate > 512_000 {
		errs = append(errs, fmt.Errorf("default_opus_bitrate %d must be between 6000 and 512000", c.DefaultOpusBitrate))
	}

	if c.FileMaxKBps < 0 {
		errs = append(errs, fmt.Errorf("file_max_kbps %d must not be negative", c.FileMaxKBps))
	}
	if c.FileChannelQuotaMB < 0 {
		errs = append(errs, fmt.Errorf("file_channel_quota_mb %d must not be negative", c.FileChannelQuotaMB))
	}
	if c.FileMaxSizeMB < 0 {
		errs = append(errs, fmt.Errorf("file_max_size_mb %d must not be negative", c.FileMaxSizeMB))
	}
	if c.FileQuietHoursStart < 0 || c.FileQuietHoursStart > 23 {
		errs = append(errs, fmt.Errorf("file_quiet_hours_start %d must be between 0 and 23", c.FileQuietHoursStart))
	}
	if c.FileQuietHoursEnd < 0 || c.FileQuietHoursEnd > 23 {
		errs = append(errs, fmt.Errorf("file_quiet_hours_end %d must be between 0 and 23", c.FileQuietHoursEnd))
	}
	if strings.TrimSpace(c.FileRoot) == "" {
		errs = append(errs, errors.New("file_root must not be empty"))
	}

	certConfigured := strings.TrimSpace(c.TLSCertFile) != ""
	keyConfigured := strings.TrimSpace(c.TLSKeyFile) != ""
	if certConfigured != keyConfigured {
		errs = append(errs, errors.New("tls_cert_file and tls_key_file must be set together"))
	}
	if c.TLSEnabled && !certConfigured && strings.TrimSpace(c.TLSDir) == "" {
		errs = append(errs, errors.New("tls_dir must not be empty when TLS certificate files are not configured"))
	}
	if c.QuerySSHEnabled && strings.TrimSpace(c.QuerySSHHostKey) == "" {
		errs = append(errs, errors.New("query_ssh_host_key must not be empty when query_ssh_enabled=true"))
	}
	if !c.DevMode {
		if !c.TLSEnabled {
			errs = append(errs, errors.New("tls_enabled must be true when dev_mode=false"))
		}
		if !c.FileTLSEnabled {
			errs = append(errs, errors.New("file_tls_enabled must be true when dev_mode=false"))
		}
		if c.ChatAllowPlaintext {
			errs = append(errs, errors.New("chat_allow_plaintext must be false when dev_mode=false"))
		}
	}

	if strings.TrimSpace(c.ChatMasterKeyFile) == "" {
		errs = append(errs, errors.New("chat_master_key_file must not be empty"))
	}
	if strings.TrimSpace(c.PIIKeyFile) == "" {
		errs = append(errs, errors.New("pii_key_file must not be empty"))
	}
	switch strings.ToLower(strings.TrimSpace(c.ChatLegacyHistory)) {
	case "encrypt", "purge":
	default:
		errs = append(errs, fmt.Errorf("chat_legacy_history %q must be encrypt or purge", c.ChatLegacyHistory))
	}
	if c.ChatKeyRotateMinSecs < 0 {
		errs = append(errs, fmt.Errorf("chat_key_rotate_min_seconds %d must not be negative", c.ChatKeyRotateMinSecs))
	}
	if c.ChatSearchMaxMessages <= 0 {
		errs = append(errs, fmt.Errorf("chat_search_max_messages %d must be positive", c.ChatSearchMaxMessages))
	}
	if c.ChatMaxLength <= 0 {
		errs = append(errs, fmt.Errorf("chat_max_length %d must be positive", c.ChatMaxLength))
	}
	if c.ChatRateMsgs <= 0 {
		errs = append(errs, fmt.Errorf("chat_rate_msgs %d must be positive", c.ChatRateMsgs))
	}
	if c.ChatRateWindowSeconds <= 0 {
		errs = append(errs, fmt.Errorf("chat_rate_window_seconds %d must be positive", c.ChatRateWindowSeconds))
	}

	if c.DBMaxOpenConns <= 0 {
		errs = append(errs, fmt.Errorf("db_max_open_conns %d must be positive", c.DBMaxOpenConns))
	}
	if c.DBMaxIdleConns <= 0 {
		errs = append(errs, fmt.Errorf("db_max_idle_conns %d must be positive", c.DBMaxIdleConns))
	}
	if c.DBMaxIdleConns > c.DBMaxOpenConns {
		errs = append(errs, fmt.Errorf(
			"db_max_idle_conns %d must not exceed db_max_open_conns %d",
			c.DBMaxIdleConns,
			c.DBMaxOpenConns,
		))
	}
	if c.DBConnMaxLifetime < 0 {
		errs = append(errs, fmt.Errorf("db_conn_max_lifetime %s must not be negative", c.DBConnMaxLifetime))
	}

	if c.TURN.CredentialsTTL <= 0 || c.TURN.CredentialsTTL > maxTURNCredentialsTTL {
		errs = append(errs, fmt.Errorf(
			"turn.credentials_ttl %s must be positive and no greater than %s",
			c.TURN.CredentialsTTL,
			maxTURNCredentialsTTL,
		))
	}
	if c.TURN.Secret != "" && strings.TrimSpace(c.TURN.Realm) == "" {
		errs = append(errs, errors.New("turn.realm must not be empty when turn.secret is configured"))
	}
	for i, rawURL := range c.TURN.URIs {
		if err := validateICEURL(fmt.Sprintf("turn.uris[%d]", i), rawURL, true); err != nil {
			errs = append(errs, err)
		}
	}
	for i, rawURL := range c.WebRTC.ICEServers {
		if err := validateICEURL(fmt.Sprintf("webrtc.ice_servers[%d]", i), rawURL, false); err != nil {
			errs = append(errs, err)
		}
	}

	switch strings.ToLower(strings.TrimSpace(c.Recording.Format)) {
	case "webm", "mp4":
	default:
		errs = append(errs, fmt.Errorf("recording.format %q must be webm or mp4", c.Recording.Format))
	}
	if c.Recording.Enabled {
		if strings.TrimSpace(c.Recording.Dir) == "" {
			errs = append(errs, errors.New("recording.dir must not be empty when recording is enabled"))
		}
		if strings.TrimSpace(c.Recording.FFmpegPath) == "" {
			errs = append(errs, errors.New("recording.ffmpeg_path must not be empty when recording is enabled"))
		}
		if c.Recording.MaxConcurrent < 1 || c.Recording.MaxConcurrent > 64 {
			errs = append(errs, fmt.Errorf("recording.max_concurrent %d must be between 1 and 64", c.Recording.MaxConcurrent))
		}
		if runtime.GOOS == "windows" && !c.Recording.WindowsACLReady {
			errs = append(errs, errors.New(
				"recording.windows_acl_ready must be true after provisioning a restricted inheritable NTFS DACL",
			))
		}
	}

	if c.UDPRateLimitPPS < 0 {
		errs = append(errs, fmt.Errorf("udp_rate_limit_pps %d must not be negative", c.UDPRateLimitPPS))
	}
	if c.UDPRateBurst < 0 {
		errs = append(errs, fmt.Errorf("udp_rate_burst %d must not be negative", c.UDPRateBurst))
	}
	if c.UDPRateLimitPPS > 0 && c.UDPRateBurst <= 0 {
		errs = append(errs, errors.New("udp_rate_burst must be positive when udp_rate_limit_pps is enabled"))
	}

	return errors.Join(errs...)
}

// Warnings returns security-relevant choices that are valid only because the
// operator explicitly opted in or selected development mode.
func (c *Config) Warnings() []string {
	if c == nil {
		return []string{"configuration is nil"}
	}
	warnings := []string{}
	if !isLoopbackAddress(c.QueryAddr) && c.QueryAllowRemote {
		warnings = append(warnings, "query_allow_remote exposes plaintext ServerQuery; prefer ServerQuery over SSH")
	}
	if c.MetricsAllowRemote {
		warnings = append(warnings, "metrics_allow_remote exposes operational metrics beyond loopback")
	}
	if !c.TLSEnabled {
		warnings = append(warnings, "tls_enabled=false exposes the control protocol in plaintext")
	}
	if !c.FileTLSEnabled || !c.TLSEnabled {
		warnings = append(warnings, "file transfers are plaintext because file TLS is not fully enabled")
	}
	if c.ChatAllowPlaintext {
		warnings = append(warnings, "chat_allow_plaintext accepts legacy plaintext chat payloads")
	}
	if c.TURN.Secret != "" && len(c.TURN.URIs) == 0 {
		warnings = append(warnings, "turn.secret is configured but turn.uris is empty; TURN is disabled")
	}
	if c.TURN.Secret == "" && len(c.TURN.URIs) > 0 {
		warnings = append(warnings, "turn.uris is configured but turn.secret is empty; TURN is disabled")
	}
	return warnings
}

func validateAddress(name, address string) error {
	if strings.TrimSpace(address) != address || address == "" {
		return fmt.Errorf("%s %q must be a non-empty host:port address without surrounding whitespace", name, address)
	}
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%s %q must be a valid host:port address: %w", name, address, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("%s %q must use a port between 1 and 65535", name, address)
	}
	return nil
}

func validateLoopbackAddress(name, address string) error {
	if isLoopbackAddress(address) {
		return nil
	}
	return fmt.Errorf("%s %q must be loopback-only", name, address)
}

func isLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateDatabaseURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("database_url is invalid: %w", err)
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return fmt.Errorf("database_url scheme %q must be postgres or postgresql", parsed.Scheme)
	}
	if parsed.Host == "" {
		return errors.New("database_url must include a host")
	}
	return nil
}

func validateProductionDatabaseURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("database_url is invalid: %w", err)
	}
	if parsed.User != nil {
		password, hasPassword := parsed.User.Password()
		if hasPassword && strings.EqualFold(parsed.User.Username(), "voicx") && password == "voicx" {
			return errors.New("database_url must not use the default voicx:voicx credentials when dev_mode=false")
		}
	}

	sslMode := strings.ToLower(strings.TrimSpace(parsed.Query().Get("sslmode")))
	switch sslMode {
	case "require", "verify-ca", "verify-full":
		return nil
	default:
		return fmt.Errorf(
			"database_url sslmode %q must be require, verify-ca, or verify-full when dev_mode=false",
			sslMode,
		)
	}
}

func validateICEURL(name, raw string, turnOnly bool) error {
	if strings.TrimSpace(raw) != raw || raw == "" {
		return fmt.Errorf("%s must be a non-empty URL without surrounding whitespace", name)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s %q is invalid: %w", name, raw, err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	validScheme := scheme == "turn" || scheme == "turns"
	if !turnOnly {
		validScheme = validScheme || scheme == "stun" || scheme == "stuns"
	}
	if !validScheme {
		allowed := "turn or turns"
		if !turnOnly {
			allowed = "stun, stuns, turn, or turns"
		}
		return fmt.Errorf("%s scheme %q must be %s", name, parsed.Scheme, allowed)
	}
	if parsed.Host == "" && parsed.Opaque == "" {
		return fmt.Errorf("%s %q must include a server address", name, raw)
	}
	return nil
}

// RedactedDatabaseURL returns a log-safe database endpoint. User information
// and sensitive query values are removed; malformed values are not echoed.
func (c *Config) RedactedDatabaseURL() string {
	if c == nil {
		return ""
	}
	return redactURL(c.DatabaseURL)
}

// RedactedRedisAddr returns a log-safe Redis endpoint. Plain host:port values
// are unchanged, while URI credentials and sensitive query values are removed.
func (c *Config) RedactedRedisAddr() string {
	if c == nil {
		return ""
	}
	if !strings.Contains(c.RedisAddr, "://") {
		if strings.Contains(c.RedisAddr, "@") {
			return "<redacted>"
		}
		return c.RedisAddr
	}
	return redactURL(c.RedisAddr)
}

func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "<redacted>"
	}
	parsed.User = nil
	parsed.Fragment = ""
	query := parsed.Query()
	for key := range query {
		if sensitiveQueryKey(key) {
			query.Del(key)
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func sensitiveQueryKey(key string) bool {
	key = strings.ToLower(key)
	for _, marker := range []string{"credential", "key", "pass", "secret", "token"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}

// Summary returns a human-readable one-line summary of the configuration,
// suitable for logging at startup.
func (c *Config) Summary() string {
	const format = "name=%q dev=%t level=%s tcp=%q udp=%q grpc=%q health=%q " +
		"query=%q db=%q redis=%q redis_enabled=%t max_clients=%d av1=%t ice_servers=%d"
	return fmt.Sprintf(
		format,
		c.ServerName, c.DevMode, c.LogLevel, c.TCPAddr, c.UDPAddr, c.GRPCAddr,
		c.HealthAddr, c.QueryAddr, c.RedactedDatabaseURL(), c.RedactedRedisAddr(),
		c.RedisEnabled, c.MaxClients, c.WebRTC.EnableAV1, len(c.WebRTC.ICEServers),
	)
}

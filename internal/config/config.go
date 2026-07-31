package config

import (
	"fmt"
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
	ServerName         string `mapstructure:"server_name"`
	ServerPassword     string `mapstructure:"server_password"`
	LogLevel           string `mapstructure:"log_level"`
	DevMode            bool   `mapstructure:"dev_mode"`
	TCPAddr            string `mapstructure:"tcp_addr"`
	UDPAddr            string `mapstructure:"udp_addr"`
	GRPCAddr           string `mapstructure:"grpc_addr"`
	HealthAddr         string `mapstructure:"health_addr"`
	QueryAddr          string `mapstructure:"query_addr"`
	FileAddr           string `mapstructure:"file_addr"`
	FileRoot           string `mapstructure:"file_root"`
	FileMaxKBps        int    `mapstructure:"file_max_kbps"`
	FileChannelQuotaMB int64  `mapstructure:"file_channel_quota_mb"`
	FileMaxSizeMB      int64  `mapstructure:"file_max_size_mb"`
	DatabaseURL        string `mapstructure:"database_url"`
	RedisAddr          string `mapstructure:"redis_addr"`
	RedisPassword      string `mapstructure:"redis_password"`
	RedisEnabled       bool   `mapstructure:"redis_enabled"`
	MaxClients         int    `mapstructure:"max_clients"`

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
	v.SetDefault("tcp_addr", ":10011")
	v.SetDefault("udp_addr", ":9987")
	v.SetDefault("grpc_addr", ":50051")
	v.SetDefault("health_addr", ":9090")
	v.SetDefault("query_addr", ":10012")
	v.SetDefault("file_addr", ":30033")
	v.SetDefault("file_root", "./data/files")
	v.SetDefault("file_max_kbps", 0)
	v.SetDefault("file_channel_quota_mb", 0)
	v.SetDefault("file_max_size_mb", 100)
	v.SetDefault("database_url", "postgres://voicx:voicx@localhost:5432/voicx?sslmode=disable")
	v.SetDefault("redis_addr", "localhost:6379")
	v.SetDefault("redis_password", "")
	v.SetDefault("redis_enabled", true)
	v.SetDefault("max_clients", 1024)

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
	return cfg, nil
}

// Summary returns a human-readable one-line summary of the configuration,
// suitable for logging at startup.
func (c *Config) Summary() string {
	return fmt.Sprintf(
		"name=%s dev=%t level=%s tcp=%s udp=%s grpc=%s health=%s db=%s redis=%s redis_enabled=%t max_clients=%d av1=%t ice=%v",
		c.ServerName, c.DevMode, c.LogLevel, c.TCPAddr, c.UDPAddr, c.GRPCAddr,
		c.HealthAddr, c.DatabaseURL, c.RedisAddr, c.RedisEnabled, c.MaxClients,
		c.WebRTC.EnableAV1, c.WebRTC.ICEServers,
	)
}

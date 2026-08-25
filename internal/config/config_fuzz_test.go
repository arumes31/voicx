package config

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

const maxFuzzConfigInput = 128 << 10

func FuzzDecodeConfig(f *testing.F) {
	for _, seed := range [][]byte{
		{},
		[]byte("log_level: INFO\nchat_legacy_history: PURGE\nrecording:\n  format: WEBM\n"),
		[]byte("turn:\n  secret: example\n  realm: voicx\n  uris: [turn:turn.example:3478?transport=udp]\nwebrtc:\n  ice_servers: [stun:stun.example:3478]\n"),
		[]byte("recording:\n  enabled: true\n  format: MP4\n  video_args: [-c:v, copy]\n  audio_args: [-c:a, copy]\n"),
		[]byte("log_level: [not, a, scalar]\nturn: [not, a, mapping]\n"),
		[]byte("shared: &shared\n  format: webm\nrecording: *shared\n"),
		[]byte("redis_dial_timeout: 5s\nturn:\n  credentials_ttl: 24h\n"),
		[]byte("recording: [\n"),
		{0xff, 0xfe, 0xfd},
	} {
		f.Add(seed, uint8(0))
	}

	f.Fuzz(func(t *testing.T, source []byte, override uint8) {
		if len(source) > maxFuzzConfigInput {
			t.Skip()
		}

		cfg, err := decodeFuzzConfig(source, override)
		again, againErr := decodeFuzzConfig(source, override)
		if (err == nil) != (againErr == nil) {
			t.Fatalf("decode success changed between identical inputs: first=%v second=%v", err, againErr)
		}
		if err != nil {
			if err.Error() != againErr.Error() {
				t.Fatalf("decode error changed between identical inputs: first=%q second=%q", err, againErr)
			}
			return
		}
		if !reflect.DeepEqual(cfg, again) {
			t.Fatalf("decoded config changed between identical inputs: first=%#v second=%#v", cfg, again)
		}
		if cfg.LogLevel != strings.ToLower(strings.TrimSpace(cfg.LogLevel)) ||
			cfg.ChatLegacyHistory != strings.ToLower(strings.TrimSpace(cfg.ChatLegacyHistory)) ||
			cfg.Recording.Format != strings.ToLower(strings.TrimSpace(cfg.Recording.Format)) {
			t.Fatalf("successful config contains unnormalized enums: %#v", cfg)
		}
		if validateErr := cfg.Validate(); validateErr != nil {
			t.Fatalf("decodeConfig accepted invalid config: %v", validateErr)
		}
	})
}

func decodeFuzzConfig(source []byte, override uint8) (*Config, error) {
	v := newConfigViper()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(bytes.NewReader(source)); err != nil {
		return nil, err
	}

	// Model a small, explicit subset of environment overrides without reading
	// the real process environment or letting fuzz data select arbitrary keys.
	switch override % 4 {
	case 1:
		v.Set("log_level", "INFO")
	case 2:
		v.Set("chat_legacy_history", "PURGE")
	case 3:
		v.Set("recording.format", "WEBM")
	}
	return decodeConfig(v)
}

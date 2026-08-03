package server

import (
	"context"
	"testing"

	"voicx/internal/config"
)

type memorySettings map[string]string

func (m memorySettings) GetServerSetting(_ context.Context, key string) (string, uint32, error) {
	return m[key], 0, nil
}

func TestLoadPersistedServerConfig(t *testing.T) {
	cfg := &config.Config{MaxClients: 10, ClientTimeoutSeconds: 90, DefaultOpusBitrate: 32000}
	err := LoadPersistedServerConfig(context.Background(), cfg, memorySettings{
		"max_clients_override":   "5000",
		"client_timeout_seconds": "120",
		"default_opus_bitrate":   "64000",
		"default_opus_fec":       "true",
		"default_opus_dtx":       "true",
		"default_opus_stereo":    "false",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxClients != 5000 || cfg.ClientTimeoutSeconds != 120 || cfg.DefaultOpusBitrate != 64000 ||
		!cfg.DefaultOpusFEC || !cfg.DefaultOpusDTX || cfg.DefaultOpusStereo {
		t.Fatalf("loaded config = %+v", cfg)
	}
}

func TestLoadPersistedServerConfigRejectsCorruption(t *testing.T) {
	cfg := &config.Config{}
	if err := LoadPersistedServerConfig(context.Background(), cfg, memorySettings{"client_timeout_seconds": "5"}); err == nil {
		t.Fatal("accepted an invalid persisted timeout")
	}
}

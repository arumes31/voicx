package server

import (
	"context"
	"fmt"
	"strconv"

	"voicx/internal/config"
	"voicx/internal/netproto"
)

// LoadPersistedServerConfig applies administrator-managed runtime settings to
// the startup configuration. Missing keys retain config.yaml/environment
// values, so the UI becomes authoritative only after an administrator saves.
func LoadPersistedServerConfig(ctx context.Context, cfg *config.Config, settings interface {
	GetServerSetting(context.Context, string) (string, uint32, error)
}) error {
	type setting struct {
		key   string
		apply func(string) error
	}
	parseInt := func(dst *int, min, max int) func(string) error {
		return func(value string) error {
			n, err := strconv.Atoi(value)
			if err != nil || n < min || n > max {
				return fmt.Errorf("invalid persisted integer %q", value)
			}
			*dst = n
			return nil
		}
	}
	parseBool := func(dst *bool) func(string) error {
		return func(value string) error {
			v, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("invalid persisted boolean %q", value)
			}
			*dst = v
			return nil
		}
	}
	entries := []setting{
		{"max_clients_override", parseInt(&cfg.MaxClients, 0, 100_000)},
		{"client_timeout_seconds", parseInt(&cfg.ClientTimeoutSeconds, 30, 86_400)},
		{"default_opus_bitrate", parseInt(&cfg.DefaultOpusBitrate, 6_000, 510_000)},
		{"default_opus_fec", parseBool(&cfg.DefaultOpusFEC)},
		{"default_opus_dtx", parseBool(&cfg.DefaultOpusDTX)},
		{"default_opus_stereo", parseBool(&cfg.DefaultOpusStereo)},
	}
	for _, entry := range entries {
		value, _, err := settings.GetServerSetting(ctx, entry.key)
		if err != nil {
			return fmt.Errorf("loading %s: %w", entry.key, err)
		}
		if value != "" {
			if err := entry.apply(value); err != nil {
				return fmt.Errorf("loading %s: %w", entry.key, err)
			}
		}
	}
	return nil
}

func (s *TCPServer) serverConfig() netproto.ServerConfig {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return netproto.ServerConfig{
		MaxClients: s.cfg.MaxClients, ClientTimeoutSeconds: s.cfg.ClientTimeoutSeconds,
		OpusBitrate: s.cfg.DefaultOpusBitrate, OpusFEC: s.cfg.DefaultOpusFEC,
		OpusDTX: s.cfg.DefaultOpusDTX, OpusStereo: s.cfg.DefaultOpusStereo,
	}
}

func (s *TCPServer) handleServerConfigQuery(_ context.Context, client *Client, f *netproto.Frame) error {
	if err := netproto.Decode(f, &netproto.ServerConfigQuery{}); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed server_config_query: "+err.Error())
	}
	if !client.isAdmin() {
		return s.sendError(client, errCodePermissionDenied, "server configuration requires administrator access")
	}
	return s.writeMessage(client, netproto.MsgServerConfigResponse, s.serverConfig())
}

func (s *TCPServer) handleServerConfigSet(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ServerConfig
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed server_config_set: "+err.Error())
	}
	if !client.isAdmin() {
		return s.sendError(client, errCodePermissionDenied, "server configuration requires administrator access")
	}
	if msg.MaxClients < 0 || msg.MaxClients > 100_000 || msg.ClientTimeoutSeconds < 30 || msg.ClientTimeoutSeconds > 86_400 ||
		msg.OpusBitrate < 6_000 || msg.OpusBitrate > 510_000 {
		return s.sendError(client, errCodeMalformed, "invalid server configuration limits")
	}
	if s.deps == nil || s.deps.Chat == nil {
		return s.sendError(client, errCodeUnavailable, "settings store unavailable")
	}
	values := map[string]string{
		"max_clients_override":   strconv.Itoa(msg.MaxClients),
		"client_timeout_seconds": strconv.Itoa(msg.ClientTimeoutSeconds),
		"default_opus_bitrate":   strconv.Itoa(msg.OpusBitrate),
		"default_opus_fec":       strconv.FormatBool(msg.OpusFEC),
		"default_opus_dtx":       strconv.FormatBool(msg.OpusDTX),
		"default_opus_stereo":    strconv.FormatBool(msg.OpusStereo),
	}
	batch, ok := s.deps.Chat.(interface {
		SetServerSettings(context.Context, map[string]string, uint32) error
	})
	if !ok {
		return s.sendError(client, errCodeUnavailable, "settings store does not support atomic updates")
	}
	if err := batch.SetServerSettings(ctx, values, 0); err != nil {
		return s.sendError(client, errCodeUnavailable, "saving server configuration failed")
	}
	s.configMu.Lock()
	s.cfg.MaxClients = msg.MaxClients
	s.cfg.ClientTimeoutSeconds = msg.ClientTimeoutSeconds
	s.cfg.DefaultOpusBitrate = msg.OpusBitrate
	s.cfg.DefaultOpusFEC = msg.OpusFEC
	s.cfg.DefaultOpusDTX = msg.OpusDTX
	s.cfg.DefaultOpusStereo = msg.OpusStereo
	s.configMu.Unlock()
	s.audit(ctx, client.UniqueID, "server_config_set", "server", fmt.Sprintf("max_clients=%d timeout=%d opus=%d", msg.MaxClients, msg.ClientTimeoutSeconds, msg.OpusBitrate))
	return s.writeMessage(client, netproto.MsgServerConfigResponse, s.serverConfig())
}

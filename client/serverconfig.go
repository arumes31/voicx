package main

import (
	"time"

	"voicx/internal/netproto"
)

// GetServerConfig returns the effective runtime settings exposed to server
// administrators. The server is authoritative and rejects non-admin callers.
func (a *App) GetServerConfig() (netproto.ServerConfig, error) {
	m, err := a.requireCM()
	if err != nil {
		return netproto.ServerConfig{}, err
	}
	f, err := m.request(netproto.MsgServerConfigQuery, netproto.MsgServerConfigResponse,
		netproto.ServerConfigQuery{}, 5*time.Second)
	if err != nil {
		return netproto.ServerConfig{}, err
	}
	var cfg netproto.ServerConfig
	if err := netproto.Decode(f, &cfg); err != nil {
		return netproto.ServerConfig{}, err
	}
	return cfg, nil
}

// SetServerConfig validates and persists runtime server settings.
func (a *App) SetServerConfig(cfg netproto.ServerConfig) (netproto.ServerConfig, error) {
	m, err := a.requireCM()
	if err != nil {
		return netproto.ServerConfig{}, err
	}
	f, err := m.request(netproto.MsgServerConfigSet, netproto.MsgServerConfigResponse, cfg, 5*time.Second)
	if err != nil {
		return netproto.ServerConfig{}, err
	}
	var applied netproto.ServerConfig
	if err := netproto.Decode(f, &applied); err != nil {
		return netproto.ServerConfig{}, err
	}
	return applied, nil
}

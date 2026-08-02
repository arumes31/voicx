package main

import (
	"testing"

	"voicx/internal/netproto"
)

func TestServerConfigBindings(t *testing.T) {
	if _, err := (&App{}).GetServerConfig(); err == nil || err.Error() != "not connected" {
		t.Fatalf("offline GetServerConfig error = %v", err)
	}
	if _, err := (&App{}).SetServerConfig(netproto.ServerConfig{}); err == nil || err.Error() != "not connected" {
		t.Fatalf("offline SetServerConfig error = %v", err)
	}

	frames := make(chan *netproto.Frame, 2)
	want := netproto.ServerConfig{
		MaxClients: 100, ClientTimeoutSeconds: 30, OpusBitrate: 64_000,
		OpusFEC: true, OpusDTX: true, OpusStereo: true,
	}
	app, _ := newPipedApp(t, func(frame *netproto.Frame) (netproto.MessageType, any, bool) {
		frames <- frame
		switch netproto.MessageType(frame.Type) {
		case netproto.MsgServerConfigQuery, netproto.MsgServerConfigSet:
			return netproto.MsgServerConfigResponse, want, true
		default:
			return 0, nil, false
		}
	})

	got, err := app.GetServerConfig()
	if err != nil || got != want {
		t.Fatalf("GetServerConfig = %+v, %v", got, err)
	}
	nextFrame(t, frames, netproto.MsgServerConfigQuery)

	got, err = app.SetServerConfig(want)
	if err != nil || got != want {
		t.Fatalf("SetServerConfig = %+v, %v", got, err)
	}
	var sent netproto.ServerConfig
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgServerConfigSet), &sent); err != nil || sent != want {
		t.Fatalf("SetServerConfig payload = %+v, %v", sent, err)
	}
}

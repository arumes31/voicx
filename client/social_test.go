package main

import (
	"testing"

	"voicx/internal/netproto"
)

func TestSocialBindings(t *testing.T) {
	frames := make(chan *netproto.Frame, 4)
	app, _ := newPipedApp(t, func(frame *netproto.Frame) (netproto.MessageType, any, bool) {
		frames <- frame
		if netproto.MessageType(frame.Type) == netproto.MsgServerInfoQuery {
			return netproto.MsgServerInfoResponse, netproto.ServerInfoResponse{
				Name: "voicx", Version: "1.0", UptimeSeconds: 60, ClientsOnline: 2, ChannelsOnline: 3, MaxClients: 100,
			}, true
		}
		return 0, nil, false
	})
	app.settings.AutoAwayMessage = "Stepped out"

	if got := app.SetStatus("away", autoAwaySentinel); got != "" {
		t.Fatalf("SetStatus = %q", got)
	}
	var status netproto.SetStatus
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgSetStatus), &status); err != nil ||
		status.Status != "away" || status.Message != "Stepped out" {
		t.Fatalf("SetStatus payload = %+v, %v", status, err)
	}

	if got := app.Poke("c2", "hello"); got != "" {
		t.Fatalf("Poke = %q", got)
	}
	var poke netproto.Poke
	if err := netproto.Decode(nextFrame(t, frames, netproto.MsgPoke), &poke); err != nil ||
		poke.ClientID != "c2" || poke.Message != "hello" {
		t.Fatalf("Poke payload = %+v, %v", poke, err)
	}

	info, err := app.ServerInfo()
	if err != nil || info.Name != "voicx" || info.ClientsOnline != 2 || info.ChannelsOnline != 3 || info.MaxClients != 100 {
		t.Fatalf("ServerInfo = %+v, %v", info, err)
	}
	nextFrame(t, frames, netproto.MsgServerInfoQuery)
}

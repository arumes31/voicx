package main

import (
	"testing"

	"voicx/internal/netproto"
)

func TestDispatchRulesAndAuthoritativeSubscriptions(t *testing.T) {
	rec := &eventRecorder{}
	cm := newConnManager(nil)
	cm.sink = rec

	subscriptions := []byte(`{"channel_ids":[3,9]}`)
	cm.dispatch(&netproto.Frame{Type: uint16(netproto.MsgSubscriptionState), Payload: subscriptions})
	cm.dispatch(&netproto.Frame{Type: uint16(netproto.MsgServerRules), Payload: []byte(`{"text":"be kind","hash":"v1"}`)})

	if cm.lastSubscriptions != string(subscriptions) {
		t.Fatalf("lastSubscriptions = %q, want %q", cm.lastSubscriptions, subscriptions)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.events) != 2 {
		t.Fatalf("events = %+v, want subscriptions and server_rules", rec.events)
	}
	if rec.events[0].name != "subscriptions" || rec.events[1].name != "server_rules" {
		t.Fatalf("event order = %q, %q", rec.events[0].name, rec.events[1].name)
	}
}

func TestAcceptServerRulesWritesExactHash(t *testing.T) {
	got := make(chan netproto.ServerRulesAccept, 1)
	app, _ := newPipedApp(t, func(f *netproto.Frame) (netproto.MessageType, any, bool) {
		if netproto.MessageType(f.Type) != netproto.MsgServerRulesAccept {
			return 0, nil, false
		}
		var accept netproto.ServerRulesAccept
		if err := netproto.Decode(f, &accept); err == nil {
			got <- accept
		}
		return 0, nil, false
	})

	if err := app.AcceptServerRules("rules-v2"); err != "" {
		t.Fatalf("AcceptServerRules returned %q", err)
	}
	if accept := <-got; accept.Hash != "rules-v2" {
		t.Fatalf("accepted hash = %q, want rules-v2", accept.Hash)
	}
}

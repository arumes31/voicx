package main

import "testing"

func TestTrayReconnectLastOnlyEmitsWhileDisconnected(t *testing.T) {
	var event string
	controller := &tray{emit: func(got string, _ any) {
		event = got
	}}

	controller.setConnected(true)
	controller.reconnectLast()
	if event != "" {
		t.Fatalf("connected tray emitted %q", event)
	}

	controller.setConnected(false)
	controller.reconnectLast()
	if event != "tray_reconnect" {
		t.Fatalf("tray event = %q, want tray_reconnect", event)
	}

	controller = &tray{}
	controller.reconnectLast()
}

func TestTrayDisconnectOnlyEmitsWhileConnected(t *testing.T) {
	var event string
	controller := &tray{emit: func(got string, _ any) {
		event = got
	}}

	controller.setConnected(false)
	controller.disconnectActive()
	if event != "" {
		t.Fatalf("disconnected tray emitted %q", event)
	}

	controller.setConnected(true)
	controller.disconnectActive()
	if event != "tray_disconnect" {
		t.Fatalf("tray event = %q, want tray_disconnect", event)
	}
}

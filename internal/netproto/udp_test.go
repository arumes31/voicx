package netproto

import (
	"errors"
	"testing"
)

func TestUDPMsgName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		msgType byte
		want    string
	}{
		{name: "ping", msgType: UDPMsgPing, want: "ping"},
		{name: "pong", msgType: UDPMsgPong, want: "pong"},
		{name: "zero", msgType: 0, want: "unknown"},
		{name: "unassigned", msgType: 0xff, want: "unknown"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := UDPMsgName(test.msgType); got != test.want {
				t.Errorf("UDPMsgName(%#x) = %q, want %q", test.msgType, got, test.want)
			}
		})
	}
}

func TestParseUDPHeader(t *testing.T) {
	t.Parallel()

	packet := []byte{UDPMsgPing, 1, 2, 3}
	msgType, payload, err := ParseUDPHeader(packet)
	if err != nil {
		t.Fatalf("ParseUDPHeader() error = %v", err)
	}
	if msgType != UDPMsgPing {
		t.Errorf("message type = %#x, want %#x", msgType, UDPMsgPing)
	}
	if len(payload) != 3 || payload[0] != 1 || payload[2] != 3 {
		t.Fatalf("payload = %v, want [1 2 3]", payload)
	}

	payload[0] = 9
	if packet[1] != 9 {
		t.Fatal("payload does not alias the packet buffer")
	}
}

func TestParseUDPHeaderEmptyPacket(t *testing.T) {
	t.Parallel()

	msgType, payload, err := ParseUDPHeader(nil)
	if !errors.Is(err, ErrUDPTooShort) {
		t.Fatalf("ParseUDPHeader(nil) error = %v, want %v", err, ErrUDPTooShort)
	}
	if msgType != 0 || payload != nil {
		t.Errorf("ParseUDPHeader(nil) = (%d, %v), want zero values", msgType, payload)
	}
}

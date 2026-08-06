package netproto

import (
	"strings"
	"testing"
)

func TestEncodeRejectsUnsupportedJSONValue(t *testing.T) {
	t.Parallel()

	if _, err := Encode(MsgPing, make(chan int)); err == nil || !strings.Contains(err.Error(), "netproto: encoding Ping") {
		t.Fatalf("Encode(channel) error = %v, want wrapped encoding error", err)
	}
}

func TestDecodeRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		frame   *Frame
		target  any
		wantErr string
	}{
		{name: "nil frame", target: &Ping{}, wantErr: "nil frame"},
		{name: "malformed JSON", frame: &Frame{Type: uint16(MsgPing), Payload: []byte("{")}, target: &Ping{}, wantErr: "decoding Ping"},
		{name: "nil target", frame: &Frame{Type: uint16(MsgPing), Payload: []byte("{}")}, wantErr: "decoding Ping"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := Decode(test.frame, test.target); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Decode() error = %v, want text %q", err, test.wantErr)
			}
		})
	}
}

func TestDecodeAllowsEmptyControlPayload(t *testing.T) {
	t.Parallel()

	if err := Decode(&Frame{Type: uint16(MsgPing)}, nil); err != nil {
		t.Fatalf("Decode(empty payload) error = %v, want nil", err)
	}
}

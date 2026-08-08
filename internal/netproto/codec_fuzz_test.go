package netproto

import (
	"bytes"
	"reflect"
	"testing"
)

const maxFuzzCodecInput = 64 << 10

func FuzzDecodeChatSend(f *testing.F) {
	for _, payload := range [][]byte{
		{},
		[]byte(`{}`),
		[]byte(`null`),
		[]byte(`{"channel_id":"7","text":"hello","enc":true,"key_id":42}`),
		[]byte(`{"text":"line\n\u003cscript\u003e","client_msg_id":"m-1"}`),
		[]byte(`{"text":"first","text":"last"}`),
		[]byte(`{"text":"x","unknown":[1,2,3]}`),
		[]byte(`{"key_id":4294967296,"text":"overflow"}`),
		[]byte(`{"text":`),
		{0xff, 0xfe, 0xfd},
	} {
		f.Add(payload)
	}

	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) > maxFuzzCodecInput {
			t.Skip()
		}

		before := bytes.Clone(payload)
		var decoded ChatSend
		if err := Decode(&Frame{Type: uint16(MsgChatSend), Payload: payload}, &decoded); err != nil {
			return
		}
		if !bytes.Equal(payload, before) {
			t.Fatal("Decode mutated the input payload")
		}

		encoded, err := Encode(MsgChatSend, decoded)
		if err != nil {
			t.Fatalf("Encode after successful Decode: %v", err)
		}
		if encoded.Type != uint16(MsgChatSend) {
			t.Fatalf("Encode type = %d, want %d", encoded.Type, MsgChatSend)
		}
		// encoding/json can expand one hostile input byte to a six-byte
		// escape. Struct field names add a small constant overhead.
		if len(encoded.Payload) > 6*len(payload)+512 {
			t.Fatalf("Encode expanded %d bytes to %d bytes", len(payload), len(encoded.Payload))
		}
		if len(encoded.Payload) > MaxPayloadSize {
			t.Fatalf("bounded fuzz input encoded to oversized payload: %d", len(encoded.Payload))
		}

		var roundTrip ChatSend
		if err := Decode(encoded, &roundTrip); err != nil {
			t.Fatalf("Decode(Encode(decoded)) error = %v", err)
		}
		if !reflect.DeepEqual(roundTrip, decoded) {
			t.Fatalf("Decode(Encode(decoded)) = %#v, want %#v", roundTrip, decoded)
		}

		truncated := encoded.Payload[:len(encoded.Payload)-1]
		if err := Decode(&Frame{Type: encoded.Type, Payload: truncated}, &ChatSend{}); err == nil {
			t.Fatal("Decode accepted canonical JSON truncated by one byte")
		}
	})
}

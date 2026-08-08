package netproto

import (
	"strings"
	"testing"
)

var codecBenchmarkCases = []struct {
	name     string
	textSize int
}{
	{name: "small_32B", textSize: 32},
	{name: "medium_4KiB", textSize: 4 << 10},
	{name: "maximum_safe", textSize: MaxPayloadSize - 256},
}

func benchmarkChatSend(textSize int) ChatSend {
	return ChatSend{
		ChannelID:   "42",
		Text:        strings.Repeat("x", textSize),
		Enc:         true,
		KeyID:       7,
		ReplyToID:   99,
		ClientMsgID: "benchmark-message",
	}
}

func BenchmarkEncodeChatSendJSON(b *testing.B) {
	for _, test := range codecBenchmarkCases {
		b.Run(test.name, func(b *testing.B) {
			message := benchmarkChatSend(test.textSize)
			preflight, err := Encode(MsgChatSend, message)
			if err != nil {
				b.Fatalf("preflight Encode: %v", err)
			}
			if len(preflight.Payload) > MaxPayloadSize {
				b.Fatalf("fixture payload = %d bytes, maximum is %d", len(preflight.Payload), MaxPayloadSize)
			}
			b.SetBytes(int64(len(preflight.Payload)))
			b.ReportAllocs()

			var frame *Frame
			for b.Loop() {
				frame, err = Encode(MsgChatSend, message)
			}
			if err != nil {
				b.Fatalf("Encode: %v", err)
			}
			if frame.Type != uint16(MsgChatSend) || len(frame.Payload) != len(preflight.Payload) {
				b.Fatal("Encode returned an unexpected frame")
			}
		})
	}
}

func BenchmarkDecodeChatSendJSON(b *testing.B) {
	for _, test := range codecBenchmarkCases {
		b.Run(test.name, func(b *testing.B) {
			message := benchmarkChatSend(test.textSize)
			frame, err := Encode(MsgChatSend, message)
			if err != nil {
				b.Fatalf("building frame: %v", err)
			}
			b.SetBytes(int64(len(frame.Payload)))
			b.ReportAllocs()

			var decoded ChatSend
			for b.Loop() {
				decoded = ChatSend{}
				err = Decode(frame, &decoded)
			}
			if err != nil {
				b.Fatalf("Decode: %v", err)
			}
			if decoded != message {
				b.Fatal("Decode returned a different message")
			}
		})
	}
}

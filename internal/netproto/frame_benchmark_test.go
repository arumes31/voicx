package netproto

import (
	"bytes"
	"testing"
)

var frameBenchmarkCases = []struct {
	name string
	size int
}{
	{name: "small_32B", size: 32},
	{name: "medium_32KiB", size: 32 << 10},
	{name: "maximum_safe", size: MaxPayloadSize},
}

func BenchmarkWriteFrame(b *testing.B) {
	for _, test := range frameBenchmarkCases {
		b.Run(test.name, func(b *testing.B) {
			payload := bytes.Repeat([]byte{0xa5}, test.size)
			frame := &Frame{Type: 0x1234, Payload: payload}
			var dst bytes.Buffer
			dst.Grow(6 + len(payload))
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()

			var err error
			for b.Loop() {
				dst.Reset()
				err = WriteFrame(&dst, frame)
			}
			if err != nil {
				b.Fatalf("WriteFrame: %v", err)
			}
			if got, want := dst.Len(), 6+len(payload); got != want {
				b.Fatalf("wire length = %d, want %d", got, want)
			}
		})
	}
}

func BenchmarkReadFrame(b *testing.B) {
	for _, test := range frameBenchmarkCases {
		b.Run(test.name, func(b *testing.B) {
			payload := bytes.Repeat([]byte{0x5a}, test.size)
			var encoded bytes.Buffer
			if err := WriteFrame(&encoded, &Frame{Type: 0x4321, Payload: payload}); err != nil {
				b.Fatalf("building frame: %v", err)
			}
			wire := bytes.Clone(encoded.Bytes())
			reader := bytes.NewReader(wire)
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()

			var (
				frame *Frame
				err   error
			)
			for b.Loop() {
				reader.Reset(wire)
				frame, err = ReadFrame(reader)
			}
			if err != nil {
				b.Fatalf("ReadFrame: %v", err)
			}
			if frame.Type != 0x4321 || !bytes.Equal(frame.Payload, payload) {
				b.Fatal("ReadFrame returned a different frame")
			}
		})
	}
}

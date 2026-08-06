package netproto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func FuzzReadFrame(f *testing.F) {
	oversize := make([]byte, 4)
	binary.BigEndian.PutUint32(oversize, MaxFrameSize+1)
	maxTruncated := make([]byte, 4)
	binary.BigEndian.PutUint32(maxTruncated, MaxFrameSize)

	for _, wire := range [][]byte{
		{},
		{0},
		{0, 0, 0, 1},
		{0, 0, 0, 2, 0x12, 0x34},
		{0, 0, 0, 5, 0x12, 0x34, 'a', 'b', 'c'},
		{0, 0, 0, 5, 0x12, 0x34, 'a'},
		{0, 0, 0, 2, 0x12, 0x34, 't', 'r', 'a', 'i', 'l'},
		oversize,
		maxTruncated,
	} {
		f.Add(wire)
	}

	f.Fuzz(func(t *testing.T, wire []byte) {
		// Production framing caps allocations at MaxFrameSize. Keep the fuzz
		// harness itself under the same order of magnitude.
		if len(wire) > MaxFrameSize+4 {
			t.Skip()
		}

		got, err := ReadFrame(bytes.NewReader(wire))
		if len(wire) < 4 {
			if err == nil {
				t.Fatal("ReadFrame accepted a truncated length prefix")
			}
			return
		}

		announced := binary.BigEndian.Uint32(wire[:4])
		switch {
		case announced < 2:
			if err == nil {
				t.Fatalf("ReadFrame accepted invalid announced length %d", announced)
			}
			return
		case announced > MaxFrameSize:
			if !errors.Is(err, ErrFrameTooLarge) {
				t.Fatalf("ReadFrame announced length %d error = %v, want ErrFrameTooLarge", announced, err)
			}
			return
		}

		required := 4 + int(announced)
		if len(wire) < required {
			if err == nil {
				t.Fatalf("ReadFrame accepted %d-byte wire for announced length %d", len(wire), announced)
			}
			return
		}
		if err != nil {
			t.Fatalf("ReadFrame rejected complete frame: %v", err)
		}
		if got == nil {
			t.Fatal("ReadFrame returned a nil frame without an error")
		}
		if got.Type != binary.BigEndian.Uint16(wire[4:6]) {
			t.Fatalf("ReadFrame type = %d, want %d", got.Type, binary.BigEndian.Uint16(wire[4:6]))
		}
		if !bytes.Equal(got.Payload, wire[6:required]) {
			t.Fatalf("ReadFrame payload = %x, want %x", got.Payload, wire[6:required])
		}
		if len(got.Payload) > MaxPayloadSize {
			t.Fatalf("ReadFrame returned %d-byte payload, limit is %d", len(got.Payload), MaxPayloadSize)
		}

		var canonical bytes.Buffer
		if err := WriteFrame(&canonical, got); err != nil {
			t.Fatalf("WriteFrame(ReadFrame(wire)) error = %v", err)
		}
		if !bytes.Equal(canonical.Bytes(), wire[:required]) {
			t.Fatalf("canonical frame = %x, want %x", canonical.Bytes(), wire[:required])
		}

		truncated := canonical.Bytes()[:canonical.Len()-1]
		if _, err := ReadFrame(bytes.NewReader(truncated)); err == nil {
			t.Fatal("ReadFrame accepted a canonical frame truncated by one byte")
		}
	})
}

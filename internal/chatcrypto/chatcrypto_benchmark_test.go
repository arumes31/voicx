package chatcrypto

import (
	"encoding/binary"
	"testing"
)

func benchmarkKEKRing(size int) *KEKRing {
	keys := make(map[uint16][32]byte, size)
	for n := 1; n <= size; n++ {
		var key [32]byte
		binary.BigEndian.PutUint16(key[:2], uint16(n))
		for i := 2; i < len(key); i++ {
			key[i] = byte(n + i)
		}
		keys[uint16(n)] = key
	}
	return &KEKRing{keys: keys, newest: uint16(size)}
}

func BenchmarkKEKRingWrap(b *testing.B) {
	ring := benchmarkKEKRing(1)
	var plain [32]byte
	for i := range plain {
		plain[i] = byte(i)
	}
	b.SetBytes(int64(len(plain)))
	b.ReportAllocs()

	var (
		id      uint16
		wrapped []byte
		err     error
	)
	for b.Loop() {
		id, wrapped, err = ring.Wrap(plain)
	}
	if err != nil {
		b.Fatalf("Wrap: %v", err)
	}
	if id != ring.NewestID() || len(wrapped) != WrappedLen {
		b.Fatalf("Wrap returned id=%d len=%d", id, len(wrapped))
	}
}

func BenchmarkKEKRingUnwrap(b *testing.B) {
	for _, test := range []struct {
		name string
		size int
	}{
		{name: "single_key", size: 1},
		{name: "medium_64_keys", size: 64},
		{name: "maximum_65535_keys", size: 65535},
	} {
		b.Run(test.name, func(b *testing.B) {
			ring := benchmarkKEKRing(test.size)
			var plain [32]byte
			for i := range plain {
				plain[i] = byte(255 - i)
			}
			id, wrapped, err := ring.Wrap(plain)
			if err != nil {
				b.Fatalf("building wrapped key: %v", err)
			}
			b.SetBytes(int64(len(plain)))
			b.ReportAllocs()

			var unwrapped [32]byte
			for b.Loop() {
				unwrapped, err = ring.Unwrap(id, wrapped)
			}
			if err != nil {
				b.Fatalf("Unwrap: %v", err)
			}
			if unwrapped != plain {
				b.Fatal("Unwrap returned different key material")
			}
		})
	}
}

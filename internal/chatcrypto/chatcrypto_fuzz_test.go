package chatcrypto

import (
	"bufio"
	"encoding/base64"
	"maps"
	"strings"
	"testing"
)

const maxFuzzRingInput = 128 << 10

func FuzzParseRing(f *testing.F) {
	key1 := base64.StdEncoding.EncodeToString(bytesOf(0x11, 32))
	key2 := base64.StdEncoding.EncodeToString(bytesOf(0x22, 32))
	for _, input := range []string{
		"",
		"# comments only\n\n",
		key1,
		"1:" + key1 + "\n2:" + key2 + "\n",
		"65535:" + key1 + "\r\n# rotated\r\n7:" + key2,
		"1:" + key1 + "\n1:" + key2,
		"0:" + key1,
		"not-an-id:" + key1,
		"1:not-base64",
		strings.Repeat("A", bufio.MaxScanTokenSize+1),
	} {
		f.Add(input)
	}

	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > maxFuzzRingInput {
			t.Skip()
		}

		ring, err := parseRing(strings.NewReader(input), "fuzz")
		if err != nil {
			return
		}
		if ring == nil || len(ring.keys) == 0 {
			t.Fatal("parseRing succeeded with an empty ring")
		}
		if ring.newest == 0 {
			t.Fatal("parseRing succeeded with key id zero")
		}
		if _, ok := ring.keys[ring.newest]; !ok {
			t.Fatalf("newest key id %d is absent from the ring", ring.newest)
		}
		if len(ring.keys) > strings.Count(input, "\n")+1 {
			t.Fatalf("parsed %d keys from fewer physical lines", len(ring.keys))
		}
		ids := ring.IDs()
		if len(ids) != len(ring.keys) {
			t.Fatalf("IDs returned %d ids for %d keys", len(ids), len(ring.keys))
		}
		for _, id := range ids {
			if id == 0 {
				t.Fatal("IDs returned reserved key id zero")
			}
			if _, ok := ring.keys[id]; !ok {
				t.Fatalf("IDs returned unknown key id %d", id)
			}
		}
		if got := ring.Fingerprint(); len(got) != 16 {
			t.Fatalf("fingerprint length = %d, want 16", len(got))
		}

		again, err := parseRing(strings.NewReader(input), "fuzz")
		if err != nil {
			t.Fatalf("second parseRing call failed: %v", err)
		}
		if ring.newest != again.newest || !maps.Equal(ring.keys, again.keys) {
			t.Fatal("parseRing is not deterministic")
		}
		if ring.Fingerprint() != again.Fingerprint() {
			t.Fatal("identical key rings produced different fingerprints")
		}
	})
}

func bytesOf(value byte, length int) []byte {
	out := make([]byte, length)
	for i := range out {
		out[i] = value
	}
	return out
}

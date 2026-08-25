package main

import (
	"maps"
	"strings"
	"testing"
)

const (
	maxFuzzFingerprintInput  = 4 << 10
	maxFuzzKnownServersInput = 128 << 10
)

func FuzzNormalizeFingerprint(f *testing.F) {
	valid := strings.Repeat("ab:", 31) + "cd"
	for _, fingerprint := range []string{
		valid,
		strings.ToUpper(valid),
		"  " + valid + "  ",
		"ab:cd",
		strings.Repeat("aa:", 32),
		strings.Repeat("aa:", 31) + "gg",
		strings.Repeat("a:", 31) + "b",
	} {
		f.Add(fingerprint)
	}

	f.Fuzz(func(t *testing.T, fingerprint string) {
		if len(fingerprint) > maxFuzzFingerprintInput {
			t.Skip()
		}

		normalized, err := normalizeFingerprint(fingerprint)
		if err != nil {
			return
		}
		parts := strings.Split(normalized, ":")
		if len(parts) != 32 {
			t.Fatalf("normalized fingerprint has %d components, want 32", len(parts))
		}
		for _, part := range parts {
			isLowerHex := strings.Trim(part, "0123456789abcdef") == ""
			if len(part) != 2 || part != strings.ToLower(part) || !isLowerHex {
				t.Fatalf("normalized fingerprint component = %q, want two lowercase hex digits", part)
			}
		}
		again, againErr := normalizeFingerprint(normalized)
		if againErr != nil || again != normalized {
			t.Fatalf("normalization is not idempotent: got %q, %v", again, againErr)
		}
	})
}

func FuzzLoadKnownServers(f *testing.F) {
	for _, document := range [][]byte{
		[]byte(`{}`),
		[]byte(`{"servers":null}`),
		[]byte(`{"servers":{"127.0.0.1:443":"pin"}}`),
		[]byte(`{"servers":{"[::1]:443":"pin"}}`),
		[]byte(`{"servers":{"EXAMPLE.com:443":"pin","example.com:443":"PIN"}}`),
		[]byte(`{"servers":{"EXAMPLE.com:443":"one","example.com:443":"two"}}`),
		[]byte(`{"servers":{"bad address":"pin"}}`),
		[]byte(`{"servers":[]}`),
		[]byte(`{"servers":`),
		{0xff, 0xfe, 0xfd},
	} {
		f.Add(document)
	}

	f.Fuzz(func(t *testing.T, document []byte) {
		if len(document) > maxFuzzKnownServersInput {
			t.Skip()
		}

		servers, err := decodeKnownServers(document)
		again, againErr := decodeKnownServers(document)
		if (err == nil) != (againErr == nil) {
			t.Fatalf("decode success changed between identical inputs: first=%v second=%v", err, againErr)
		}
		if err != nil {
			if err.Error() != againErr.Error() {
				t.Fatalf("decode error changed between identical inputs: first=%q second=%q", err, againErr)
			}
			return
		}
		if !maps.Equal(servers, again) {
			t.Fatalf("decoded maps changed between identical inputs: first=%#v second=%#v", servers, again)
		}
		for addr := range servers {
			canonical, canonicalErr := normalizeServerAddr(addr)
			if canonicalErr != nil || canonical != addr {
				t.Fatalf("decoded non-canonical address %q: canonical=%q err=%v", addr, canonical, canonicalErr)
			}
		}
	})
}

package auth

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestHashPasswordFormat verifies that HashPassword returns a non-empty encoded
// hash with the expected argon2id$ prefix.
func TestHashPasswordFormat(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if h == "" {
		t.Fatal("expected non-empty hash")
	}
	if !strings.HasPrefix(h, "argon2id$") {
		t.Fatalf("expected argon2id$ prefix, got %q", h)
	}
}

// TestVerifyPasswordCorrect verifies that VerifyPassword succeeds for the
// correct password and fails for a wrong one.
func TestVerifyPasswordCorrect(t *testing.T) {
	pw := "supersecret"
	h, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := VerifyPassword(pw, h); err != nil {
		t.Fatalf("VerifyPassword(correct): %v", err)
	}
	if err := VerifyPassword("wrong", h); err == nil {
		t.Fatal("VerifyPassword(wrong) unexpectedly succeeded")
	}
}

// TestVerifyPasswordMalformed verifies that VerifyPassword fails on a
// malformed hash.
func TestVerifyPasswordMalformed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		hash string
	}{
		{name: "empty", hash: ""},
		{name: "not encoded", hash: "not-a-hash"},
		{name: "missing fields", hash: "argon2id$v=19$m=65536,t=3,p=4$"},
		{name: "empty hash", hash: "argon2id$v=19$m=65536,t=3,p=4$YmFkc2FsdA==$"},
		{name: "wrong version", hash: "argon2id$v=99$m=65536,t=3,p=4$YmFkc2FsdA==$YmFkaGFzaA=="},
		{name: "encoded value too long", hash: strings.Repeat("x", argonMaxEncodedLength+1)},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := VerifyPassword("pw", test.hash); !errors.Is(err, ErrMalformedHash) {
				t.Fatalf("VerifyPassword(%q) error = %v, want ErrMalformedHash", test.hash, err)
			}
		})
	}
}

func TestParseParams(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		params          string
		expectedMemory  uint32
		expectedTime    uint32
		expectedThreads uint8
		shouldErr       bool
	}{
		{name: "valid", params: "m=65536,t=3,p=4", expectedMemory: 65536, expectedTime: 3, expectedThreads: 4},
		{name: "maximum safe values", params: "m=262144,t=10,p=16", expectedMemory: argonMaxMemory, expectedTime: argonMaxTime, expectedThreads: argonMaxThreads},
		{name: "parallelism above cap", params: "m=65536,t=3,p=17", shouldErr: true},
		{name: "parallelism overflow", params: "m=65536,t=3,p=256", shouldErr: true},
		{name: "memory above cap", params: "m=262145,t=3,p=4", shouldErr: true},
		{name: "iterations above cap", params: "m=65536,t=11,p=4", shouldErr: true},
		{name: "duplicate parameter", params: "m=65536,t=3,p=4,p=5", shouldErr: true},
		{name: "missing parameter", params: "m=65536,t=3", shouldErr: true},
		{name: "zero parameter", params: "m=65536,t=3,p=0", shouldErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			memory, time, threads, err := parseParams(test.params)
			if test.shouldErr {
				if !errors.Is(err, ErrMalformedHash) {
					t.Fatalf("parseParams(%q) error = %v, want ErrMalformedHash", test.params, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseParams(%q): %v", test.params, err)
			}
			if memory != test.expectedMemory || time != test.expectedTime || threads != test.expectedThreads {
				t.Fatalf("parseParams(%q) = (%d, %d, %d), want (%d, %d, %d)",
					test.params, memory, time, threads,
					test.expectedMemory, test.expectedTime, test.expectedThreads)
			}
		})
	}
}

// TestHashPasswordRandomSalt verifies that two HashPassword calls with the same
// password produce different hashes (random salt).
func TestHashPasswordRandomSalt(t *testing.T) {
	pw := "samepassword"
	h1, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword 1: %v", err)
	}
	h2, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword 2: %v", err)
	}
	if h1 == h2 {
		t.Fatal("expected different hashes for same password (random salt)")
	}
}

func FuzzParseEncodedHash(f *testing.F) {
	saltB64 := base64.StdEncoding.EncodeToString([]byte("salt"))
	hashB64 := base64.StdEncoding.EncodeToString([]byte("hash"))
	for _, encoded := range []string{
		"",
		"argon2id$v=19$m=1,t=1,p=1$" + saltB64 + "$" + hashB64,
		"argon2id$v=19$p=16,t=10,m=262144$" + saltB64 + "$" + hashB64,
		"argon2id$v=19$m=0,t=1,p=1$" + saltB64 + "$" + hashB64,
		"argon2id$v=19$m=262145,t=1,p=1$" + saltB64 + "$" + hashB64,
		"argon2id$v=19$m=1,t=1,p=1,p=2$" + saltB64 + "$" + hashB64,
		"argon2id$v=19$m=1,t=1,x=1$" + saltB64 + "$" + hashB64,
		"argon2id$v=19$m=1,t=1,p=1$!$" + hashB64,
		"argon2id$v=19$m=1,t=1,p=1$" + saltB64 + "$!",
		strings.Repeat("x", argonMaxEncodedLength+1),
	} {
		f.Add(encoded)
	}

	f.Fuzz(func(t *testing.T, encoded string) {
		salt, hash, memory, timeCost, threads, err := parseEncodedHash(encoded)
		if err != nil {
			if !errors.Is(err, ErrMalformedHash) {
				t.Fatalf("parseEncodedHash error = %v, want ErrMalformedHash", err)
			}
			return
		}

		if len(encoded) > argonMaxEncodedLength {
			t.Fatalf("parseEncodedHash accepted %d bytes, limit is %d", len(encoded), argonMaxEncodedLength)
		}
		if len(salt) == 0 || len(salt) > argonMaxSaltLength {
			t.Fatalf("salt length = %d, want 1..%d", len(salt), argonMaxSaltLength)
		}
		if len(hash) == 0 || len(hash) > argonMaxHashLength {
			t.Fatalf("hash length = %d, want 1..%d", len(hash), argonMaxHashLength)
		}
		if memory == 0 || memory > argonMaxMemory {
			t.Fatalf("memory cost = %d, want 1..%d", memory, argonMaxMemory)
		}
		if timeCost == 0 || timeCost > argonMaxTime {
			t.Fatalf("time cost = %d, want 1..%d", timeCost, argonMaxTime)
		}
		if threads == 0 || threads > argonMaxThreads {
			t.Fatalf("thread cost = %d, want 1..%d", threads, argonMaxThreads)
		}

		canonical := fmt.Sprintf("argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
			argon2idVersion, memory, timeCost, threads,
			base64.StdEncoding.EncodeToString(salt),
			base64.StdEncoding.EncodeToString(hash),
		)
		salt2, hash2, memory2, time2, threads2, err := parseEncodedHash(canonical)
		if err != nil {
			t.Fatalf("parseEncodedHash(canonical) error = %v", err)
		}
		if !bytes.Equal(salt2, salt) || !bytes.Equal(hash2, hash) ||
			memory2 != memory || time2 != timeCost || threads2 != threads {
			t.Fatal("canonical hash did not preserve parsed values")
		}
	})
}

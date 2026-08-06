package auth

import (
	"encoding/base64"
	"errors"
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
	valid := "argon2id$v=19$m=65536,t=3,p=4$" +
		base64.StdEncoding.EncodeToString([]byte("salt")) + "$" +
		base64.StdEncoding.EncodeToString([]byte("hash"))
	for _, seed := range []string{
		"",
		"argon2id$v=19$m=65536,t=3,p=4$bad$bad",
		"argon2id$v=19$m=262145,t=3,p=4$c2FsdA==$aGFzaA==",
		valid,
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, encoded string) {
		salt, hash, memory, time, threads, err := parseEncodedHash(encoded)
		if err != nil {
			if !errors.Is(err, ErrMalformedHash) {
				t.Fatalf("parseEncodedHash(%q) error = %v, want ErrMalformedHash", encoded, err)
			}
			return
		}
		if len(salt) == 0 || len(hash) == 0 || len(hash) > argonMaxHashLength {
			t.Fatalf("accepted invalid decoded lengths: salt=%d hash=%d", len(salt), len(hash))
		}
		if memory == 0 || memory > argonMaxMemory || time == 0 || time > argonMaxTime || threads == 0 || threads > argonMaxThreads {
			t.Fatalf("accepted unsafe params: m=%d t=%d p=%d", memory, time, threads)
		}
	})
}

package auth

import (
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
	cases := []string{
		"",
		"not-a-hash",
		"argon2id$v=19$m=65536,t=3,p=4$",
		"argon2id$v=19$m=65536,t=3,p=4$YmFkc2FsdA==$",
		"argon2id$v=99$m=65536,t=3,p=4$YmFkc2FsdA==$YmFkaGFzaA==",
	}
	for _, c := range cases {
		if err := VerifyPassword("pw", c); err == nil {
			t.Fatalf("VerifyPassword(%q) unexpectedly succeeded", c)
		}
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

package auth

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestGenerateSaltRejectsNonPositiveLength(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		length int
	}{
		{name: "negative", length: -1},
		{name: "zero", length: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := GenerateSalt(test.length); err == nil {
				t.Errorf("GenerateSalt(%d) unexpectedly succeeded", test.length)
			}
		})
	}
}

func TestHashPasswordRejectsEmptyPassword(t *testing.T) {
	t.Parallel()

	if _, err := HashPassword(""); err == nil {
		t.Fatal("HashPassword(empty) unexpectedly succeeded")
	}
}

func TestParseEncodedHashRejectsHostileFields(t *testing.T) {
	t.Parallel()

	validSalt := base64.StdEncoding.EncodeToString([]byte("salt"))
	validHash := base64.StdEncoding.EncodeToString([]byte("hash"))
	tests := []struct {
		name string
		hash string
	}{
		{name: "missing version prefix", hash: "argon2id$19$m=1,t=1,p=1$" + validSalt + "$" + validHash},
		{name: "non-numeric version", hash: "argon2id$v=nope$m=1,t=1,p=1$" + validSalt + "$" + validHash},
		{name: "unknown parameter", hash: "argon2id$v=19$m=1,t=1,x=1$" + validSalt + "$" + validHash},
		{name: "parameter missing equals", hash: "argon2id$v=19$m=1,t=1,p$" + validSalt + "$" + validHash},
		{name: "negative parameter", hash: "argon2id$v=19$m=-1,t=1,p=1$" + validSalt + "$" + validHash},
		{name: "invalid salt base64", hash: "argon2id$v=19$m=1,t=1,p=1$!$" + validHash},
		{name: "empty salt", hash: "argon2id$v=19$m=1,t=1,p=1$$" + validHash},
		{
			name: "decoded salt above cap",
			hash: "argon2id$v=19$m=1,t=1,p=1$" +
				base64.StdEncoding.EncodeToString(make([]byte, argonMaxSaltLength+1)) + "$" + validHash,
		},
		{name: "invalid hash base64", hash: "argon2id$v=19$m=1,t=1,p=1$" + validSalt + "$!"},
		{
			name: "decoded hash above cap",
			hash: "argon2id$v=19$m=1,t=1,p=1$" + validSalt + "$" +
				base64.StdEncoding.EncodeToString(make([]byte, argonMaxHashLength+1)),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if _, _, _, _, _, err := parseEncodedHash(test.hash); !errors.Is(err, ErrMalformedHash) {
				t.Fatalf("parseEncodedHash(%q) error = %v, want ErrMalformedHash", test.hash, err)
			}
		})
	}
}

func TestVerifyPasswordPreservesMalformedHashClassification(t *testing.T) {
	t.Parallel()

	encoded := strings.Repeat("x", argonMaxEncodedLength+1)
	if err := VerifyPassword("password", encoded); !errors.Is(err, ErrMalformedHash) {
		t.Fatalf("VerifyPassword() error = %v, want ErrMalformedHash", err)
	}
}

package main

import (
	"context"
	"testing"

	"voicx/internal/auth"
)

func TestQueryBackendHidesUnknownAccount(t *testing.T) {
	backend := &queryBackend{
		passwordAuthenticator: func(context.Context, string, string) (bool, error) {
			return false, auth.ErrUserNotFound
		},
	}
	ok, admin, err := backend.Authenticate(context.Background(), "missing", "password")
	if err != nil || ok || admin {
		t.Fatalf("Authenticate unknown = ok=%t admin=%t err=%v, want false false nil", ok, admin, err)
	}
}

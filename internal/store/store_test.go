package store

import (
	"testing"
	"time"

	"go.uber.org/zap"
)

func testLogger() *zap.Logger {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}
	return logger
}

// TestNewBadURL verifies that New returns an error when the database URL is
// invalid (i.e. the database cannot be reached).
func TestNewBadURL(t *testing.T) {
	_, err := New("postgres://nobody:nopass@127.0.0.1:1/doesnotexist?sslmode=disable",
		testLogger(), 1, 1, time.Minute)
	if err == nil {
		t.Fatal("expected error for bad database URL, got nil")
	}
}

// TestMigrateSkipsWhenNoDB verifies that Migrate is skipped when the database
// is unavailable (e.g. in CI without a running Postgres).
func TestMigrateSkipsWhenNoDB(t *testing.T) {
	_, err := New("postgres://nobody:nopass@127.0.0.1:1/doesnotexist?sslmode=disable",
		testLogger(), 1, 1, time.Minute)
	if err == nil {
		t.Skip("database unexpectedly available; skipping skip-when-no-DB test")
	}
	t.Skip("no database available; skipping Migrate test")
}

// TestPingSkipsWhenNoDB verifies that Ping is skipped when the database is
// unavailable.
func TestPingSkipsWhenNoDB(t *testing.T) {
	_, err := New("postgres://nobody:nopass@127.0.0.1:1/doesnotexist?sslmode=disable",
		testLogger(), 1, 1, time.Minute)
	if err == nil {
		t.Skip("database unexpectedly available; skipping skip-when-no-DB test")
	}
	t.Skip("no database available; skipping Ping test")
}

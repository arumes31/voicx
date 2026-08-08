package main

import (
	"context"
	"strings"
	"testing"
)

func TestRunRejectsInvalidMigrationBounds(t *testing.T) {
	if err := run(context.Background(), "name", "password", false, "postgres://unused", 0); err == nil ||
		!strings.Contains(err.Error(), "positive") {
		t.Fatalf("run(zero timeout) error = %v, want timeout validation", err)
	}
}

func TestDatabaseDSNPrecedence(t *testing.T) {
	t.Setenv("VOICX_DATABASE_URL", "postgres://environment")

	got, err := databaseDSN("postgres://explicit")
	if err != nil {
		t.Fatalf("databaseDSN: %v", err)
	}
	if got != "postgres://explicit" {
		t.Fatalf("databaseDSN = %q, want explicit DSN", got)
	}

	got, err = databaseDSN("")
	if err != nil {
		t.Fatalf("databaseDSN from environment: %v", err)
	}
	if got != "postgres://environment" {
		t.Fatalf("databaseDSN = %q, want environment DSN", got)
	}
}

func TestDatabaseDSNRequired(t *testing.T) {
	t.Setenv("VOICX_DATABASE_URL", "")

	_, err := databaseDSN("")
	if err == nil || !strings.Contains(err.Error(), "VOICX_DATABASE_URL") {
		t.Fatalf("databaseDSN error = %v, want configuration error", err)
	}
}

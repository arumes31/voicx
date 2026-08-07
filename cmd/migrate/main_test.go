package main

import (
	"context"
	"strings"
	"testing"
)

func TestRunRejectsInvalidMigrationBounds(t *testing.T) {
	if err := run(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatalf("run(zero timeout) error = %v, want timeout validation", err)
	}
}

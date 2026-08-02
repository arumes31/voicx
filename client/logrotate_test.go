package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDailyLogWriterRotatesAndCapsDirectory(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	w, err := newDailyLogWriter(dir, "client.log")
	if err != nil {
		t.Fatal(err)
	}
	w.now = func() time.Time { return now }
	w.day = now.Format("20060102")
	if _, err := w.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(24 * time.Hour)
	if _, err := w.Write([]byte("second\n")); err != nil {
		t.Fatal(err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "client-*.log")); len(matches) != 1 {
		t.Fatalf("rotated logs = %v", matches)
	}
	old := filepath.Join(dir, "old.log")
	if err := os.WriteFile(old, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	w.maxBytes = 16
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("oldest rotated log was not pruned")
	}
	_ = w.Close()
}

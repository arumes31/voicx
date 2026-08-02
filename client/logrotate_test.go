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
	old := filepath.Join(dir, "client-20260701.log")
	if err := os.WriteFile(old, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "client-unrelated.log")
	if err := os.WriteFile(other, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	w.maxBytes = 16
	w.sincePrune = pruneCheckBytes - 1
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("oldest rotated log was not pruned")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("another writer's log was pruned: %v", err)
	}
	_ = w.Close()
}

func TestAppendDailyLogReusesWriterAndCloseClearsCache(t *testing.T) {
	closeDailyLogs()
	dir := t.TempDir()
	appendDailyLog(dir, "chat.log", "one\n")
	appendDailyLog(dir, "chat.log", "two\n")
	chatLogMu.Lock()
	count := len(chatLogWriters)
	chatLogMu.Unlock()
	if count != 1 {
		t.Fatalf("cached writers = %d, want 1", count)
	}
	closeDailyLogs()
	chatLogMu.Lock()
	count = len(chatLogWriters)
	chatLogMu.Unlock()
	if count != 0 {
		t.Fatalf("cached writers after close = %d", count)
	}
	contents, err := os.ReadFile(filepath.Join(dir, "chat.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "one\ntwo\n" {
		t.Fatalf("contents = %q", contents)
	}
}

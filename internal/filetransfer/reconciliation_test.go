package filetransfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReconcileChannelDataRemovesOnlyCanonicalOrphans(t *testing.T) {
	rootDir := t.TempDir()
	for _, name := range []string{"7", "8", "70", "007", "avatars", "icons", "group_icons"} {
		if err := os.Mkdir(filepath.Join(rootDir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(rootDir, "9"), []byte("not a channel directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{Addr: ":0", RootDir: rootDir}, newFakeFileStore(), nil)

	removed, err := s.ReconcileChannelData(context.Background(), []int64{7, 70})
	if err != nil {
		t.Fatalf("ReconcileChannelData: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(filepath.Join(rootDir, "8")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan channel 8 survived: %v", err)
	}
	for _, name := range []string{"7", "70", "007", "avatars", "icons", "group_icons", "9"} {
		if _, err := os.Stat(filepath.Join(rootDir, name)); err != nil {
			t.Errorf("preserved entry %q: %v", name, err)
		}
	}

	removed, err = s.ReconcileChannelData(context.Background(), []int64{7, 70})
	if err != nil || removed != 0 {
		t.Fatalf("idempotent reconcile = (%d, %v), want (0, nil)", removed, err)
	}
}

func TestReconcileChannelDataContinuesAfterCleanupFailure(t *testing.T) {
	rootDir := t.TempDir()
	for _, name := range []string{"8", "9"} {
		if err := os.Mkdir(filepath.Join(rootDir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	s := New(Config{Addr: ":0", RootDir: rootDir}, newFakeFileStore(), nil)
	allowEight := false
	s.removeChannelDataFn = func(channelID int64) error {
		if channelID == 8 && !allowEight {
			return errors.New("volume unavailable")
		}
		return s.removeChannelData(channelID)
	}

	removed, err := s.ReconcileChannelData(context.Background(), nil)
	if err == nil || removed != 1 {
		t.Fatalf("partial reconcile = (%d, %v), want (1, error)", removed, err)
	}
	if _, err := os.Stat(filepath.Join(rootDir, "8")); err != nil {
		t.Fatalf("failed orphan cleanup changed channel 8: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootDir, "9")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("channel 9 was not cleaned after channel 8 failure: %v", err)
	}

	allowEight = true
	removed, err = s.ReconcileChannelData(context.Background(), nil)
	if err != nil || removed != 1 {
		t.Fatalf("retry reconcile = (%d, %v), want (1, nil)", removed, err)
	}
	if _, err := os.Stat(filepath.Join(rootDir, "8")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("channel 8 survived reconciliation retry: %v", err)
	}
}

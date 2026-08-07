package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"voicx/internal/state"
)

func TestMarkChannelIconsUsesConfinedValidatedDiscovery(t *testing.T) {
	rootDir := t.TempDir()
	iconDir := filepath.Join(rootDir, "icons")
	if err := os.Mkdir(iconDir, 0o700); err != nil {
		t.Fatal(err)
	}
	validPNG, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(iconDir, "1.png"), validPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(iconDir, "2.txt"), []byte("unsupported"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(iconDir, "3.png"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(iconDir, "not-a-channel.png"), []byte("malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.png")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Symlink(outside, filepath.Join(iconDir, "4.png"))
	if err := os.WriteFile(filepath.Join(iconDir, "5.png"), validPNG[:24], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(iconDir, "6.jpg"), validPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	oversized := make([]byte, 300*1024)
	copy(oversized, validPNG)
	if err := os.WriteFile(filepath.Join(iconDir, "7.png"), oversized, 0o600); err != nil {
		t.Fatal(err)
	}

	manager := state.New(zap.NewNop())
	for id := int64(1); id <= 7; id++ {
		manager.AddChannel(&state.Channel{ChannelID: id})
	}
	marked, err := markChannelIcons(rootDir, manager)
	if err != nil {
		t.Fatalf("markChannelIcons: %v", err)
	}
	if marked != 1 {
		t.Fatalf("marked = %d, want 1", marked)
	}
	for id := int64(1); id <= 7; id++ {
		hasIcon, ok := manager.ChannelHasIcon(id)
		if !ok {
			t.Fatalf("channel %d missing", id)
		}
		if got, want := hasIcon, id == 1; got != want {
			t.Fatalf("channel %d HasIcon = %t, want %t", id, got, want)
		}
	}
}

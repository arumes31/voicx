package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appversion "voicx/internal/version"
)

func TestLinkerFlags(t *testing.T) {
	t.Parallel()

	metadata := appversion.Metadata{
		Version:   "0.4.0-dev+gabc1234.dirty.hdef5678",
		Commit:    "abc1234",
		BuildDate: "2026-08-08T10:00:00Z",
		Dirty:     true,
	}
	got := linkerFlags(metadata)
	for _, expected := range []string{
		"-X=voicx/internal/version.Version=0.4.0-dev+gabc1234.dirty.hdef5678",
		"-X=voicx/internal/version.Commit=abc1234",
		"-X=voicx/internal/version.BuildDate=2026-08-08T10:00:00Z",
		"-X=voicx/internal/version.Dirty=true",
	} {
		if !strings.Contains(got, expected) {
			t.Errorf("linkerFlags() = %q, missing %q", got, expected)
		}
	}
}

func TestWriteGitHubEnvironment(t *testing.T) {
	t.Parallel()

	metadata := appversion.Metadata{Version: "0.4.0-dev+gabc", Commit: "abc"}
	var output bytes.Buffer
	if err := writeGitHubEnvironment(&output, metadata); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"VOICX_VERSION=0.4.0-dev+gabc\n",
		"VOICX_COMMIT=abc\n",
		"VOICX_DIRTY=false\n",
		"VOICX_PRERELEASE=true\n",
		"VOICX_LDFLAGS=",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("environment = %q, missing %q", output.String(), expected)
		}
	}
}

func TestResolveRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte("1.2.3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "one", "two")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := resolveRoot(nested)
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Errorf("resolveRoot() = %q, want %q", got, root)
	}
}

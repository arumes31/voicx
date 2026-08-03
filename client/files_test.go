// files_test.go covers the client-side pieces of wave-7 file transfer that
// run without a server: resume bookkeeping (259) and the download-folder
// setting (Downloads settings page).
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// TestResumeState verifies the partial file is reopened at its end with a
// hasher already primed with the bytes on disk (259): the server's digest
// covers the whole file, so a resumed transfer can only verify if the prefix
// went through the same hash.
func TestResumeState(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "movie.bin")

	// No remnant: start from zero.
	f, have, h, err := resumeState(dest)
	if err != nil {
		t.Fatalf("resumeState (fresh): %v", err)
	}
	if have != 0 {
		t.Errorf("fresh offset = %d, want 0", have)
	}
	whole := []byte("the first half the second half")
	if _, err := f.Write(whole[:14]); err != nil {
		t.Fatalf("write prefix: %v", err)
	}
	h.Write(whole[:14])
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The remnant survives the failed attempt and is picked up next time.
	f2, have2, h2, err := resumeState(dest)
	if err != nil {
		t.Fatalf("resumeState (resume): %v", err)
	}
	defer f2.Close()
	if have2 != 14 {
		t.Fatalf("resume offset = %d, want 14", have2)
	}
	// Appending the tail must reproduce the whole-file digest.
	if _, err := f2.Write(whole[14:]); err != nil {
		t.Fatalf("write tail: %v", err)
	}
	h2.Write(whole[14:])
	sum := sha256.Sum256(whole)
	if got := hex.EncodeToString(h2.Sum(nil)); got != hex.EncodeToString(sum[:]) {
		t.Errorf("resumed digest = %s, want whole-file %s", got, hex.EncodeToString(sum[:]))
	}
	if _, err := os.Stat(dest + partSuffix); err != nil {
		t.Errorf("partial file missing: %v", err)
	}
}

// TestDownloadPath verifies the Downloads settings folder is what decides
// where a download lands, and that an unset or bogus folder falls back to the
// save dialog by returning "".
func TestDownloadPath(t *testing.T) {
	dir := t.TempDir()
	a := &App{settings: DefaultSettings(), settingsPath: filepath.Join(dir, "settings.json")}

	if got := a.DownloadPath("a.txt"); got != "" {
		t.Errorf("unset download folder = %q, want \"\"", got)
	}

	a.settings.DownloadFolder = dir
	if got, want := a.DownloadPath("a.txt"), filepath.Join(dir, "a.txt"); got != want {
		t.Errorf("DownloadPath = %q, want %q", got, want)
	}
	// A name carrying a path must not escape the chosen folder.
	if got, want := a.DownloadPath("../../etc/passwd"), filepath.Join(dir, "passwd"); got != want {
		t.Errorf("traversal DownloadPath = %q, want %q", got, want)
	}

	a.settings.DownloadFolder = filepath.Join(dir, "gone")
	if got := a.DownloadPath("a.txt"); got != "" {
		t.Errorf("missing folder = %q, want \"\" (fall back to the dialog)", got)
	}
}

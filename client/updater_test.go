// updater_test.go covers the update check against a fake GitHub API and the
// checksum verification.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"voicx/internal/version"
)

// fakeGitHub serves a canned latest-release response and asset downloads.
func fakeGitHub(t *testing.T, releaseJSON string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("missing Accept header")
		}
		w.Write([]byte(releaseJSON))
	})
	return httptest.NewServer(mux)
}

func withUpdateRepo(t *testing.T, repo string) {
	t.Helper()
	old := version.UpdateRepo
	version.UpdateRepo = repo
	t.Cleanup(func() { version.UpdateRepo = old })
}

func TestCheckForUpdateAvailable(t *testing.T) {
	withUpdateRepo(t, "o/r")
	srv := fakeGitHub(t, `{
		"tag_name": "v99.0.0+999",
		"assets": [
			{"name": "voicx-client-windows-amd64.exe", "browser_download_url": "https://x/exe", "size": 12345},
			{"name": "checksums.txt", "browser_download_url": "https://x/sums"}
		]}`)
	defer srv.Close()

	old := updateAPIBase
	updateAPIBase = srv.URL
	defer func() { updateAPIBase = old }()

	a := &App{}
	info, err := a.CheckForUpdate()
	if err != nil {
		t.Fatalf("CheckForUpdate: %v", err)
	}
	if !info.Available {
		t.Fatal("expected update available")
	}
	if info.Version != "v99.0.0+999" || info.URL != "https://x/exe" || info.SHA256URL != "https://x/sums" || info.Size != 12345 {
		t.Fatalf("info = %+v", info)
	}
}

func TestCheckForUpdateUpToDate(t *testing.T) {
	withUpdateRepo(t, "o/r")
	oldV, oldB := version.Version, version.Build
	version.Version, version.Build = "0.4.0", "100"
	defer func() { version.Version, version.Build = oldV, oldB }()

	srv := fakeGitHub(t, `{"tag_name": "v0.4.0+99", "assets": []}`)
	defer srv.Close()

	old := updateAPIBase
	updateAPIBase = srv.URL
	defer func() { updateAPIBase = old }()

	a := &App{}
	info, err := a.CheckForUpdate()
	if err != nil {
		t.Fatalf("CheckForUpdate: %v", err)
	}
	if info.Available {
		t.Fatal("expected up-to-date")
	}
}

func TestCheckForUpdateNotFound(t *testing.T) {
	withUpdateRepo(t, "o/r")
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	old := updateAPIBase
	updateAPIBase = srv.URL
	defer func() { updateAPIBase = old }()

	a := &App{}
	if _, err := a.CheckForUpdate(); err == nil {
		t.Fatal("expected error on 404")
	}
}

func TestCheckForUpdateBadJSON(t *testing.T) {
	withUpdateRepo(t, "o/r")
	srv := fakeGitHub(t, `{not json`)
	defer srv.Close()

	old := updateAPIBase
	updateAPIBase = srv.URL
	defer func() { updateAPIBase = old }()

	a := &App{}
	if _, err := a.CheckForUpdate(); err == nil {
		t.Fatal("expected error on bad JSON")
	}
}

func TestCheckForUpdateNoSource(t *testing.T) {
	withUpdateRepo(t, "voicx/voicx") // placeholder slug
	a := &App{}
	if _, err := a.CheckForUpdate(); err == nil {
		t.Fatal("expected error for placeholder update repo")
	}
}

func TestVerifyChecksum(t *testing.T) {
	content := []byte("fake client binary contents")
	sum := sha256.Sum256(content)
	hexSum := hex.EncodeToString(sum[:])

	dir := t.TempDir()
	path := filepath.Join(dir, clientAssetName)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	sums := fmt.Sprintf("%s  %s\ndeadbeef  other-file\n", hexSum, clientAssetName)
	if err := verifyChecksum(path, sums, clientAssetName); err != nil {
		t.Fatalf("verifyChecksum(match): %v", err)
	}
	if err := verifyChecksum(path, "0000  "+clientAssetName+"\n", clientAssetName); err == nil {
		t.Fatal("verifyChecksum(mismatch) accepted")
	}
	if err := verifyChecksum(path, sums, "missing.exe"); err == nil {
		t.Fatal("verifyChecksum(missing asset) accepted")
	}
}

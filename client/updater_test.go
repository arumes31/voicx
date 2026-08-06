// updater_test.go covers the update check against a fake GitHub API and the
// checksum verification.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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
		_, _ = w.Write([]byte(releaseJSON))
	})
	return httptest.NewServer(mux)
}

func withUpdateRepo(t *testing.T, repo string) {
	t.Helper()
	old := version.UpdateRepo
	version.UpdateRepo = repo
	t.Cleanup(func() { version.UpdateRepo = old })
}

func withUpdatePublicKeys(t *testing.T, keys string) {
	t.Helper()
	old := version.UpdatePublicKeys
	version.UpdatePublicKeys = keys
	t.Cleanup(func() { version.UpdatePublicKeys = old })
}

func TestCheckForUpdateAvailable(t *testing.T) {
	withUpdateRepo(t, "o/r")
	srv := fakeGitHub(t, `{
		"tag_name": "v99.0.0+999",
		"assets": [
			{"name": "voicx-client-windows-amd64.exe", "browser_download_url": "https://x/exe", "size": 12345},
			{"name": "checksums.txt", "browser_download_url": "https://x/sums"},
			{"name": "checksums.txt.sig", "browser_download_url": "https://x/signature"}
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
	if info.Version != "v99.0.0+999" || info.URL != "https://x/exe" ||
		info.SHA256URL != "https://x/sums" || info.SignatureURL != "https://x/signature" ||
		info.Size != 12345 {
		t.Fatalf("info = %+v", info)
	}
}

func TestCheckForUpdateRequiresSignature(t *testing.T) {
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

	if _, err := (&App{}).CheckForUpdate(); err == nil {
		t.Fatal("expected unsigned release to be rejected")
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

func TestCheckForUpdateRejectsInvalidRepo(t *testing.T) {
	withUpdateRepo(t, "owner/repo/extra")
	if _, err := (&App{}).CheckForUpdate(); err == nil {
		t.Fatal("expected invalid repository to be rejected")
	}
}

func TestVerifyChecksum(t *testing.T) {
	content := []byte("fake client binary contents")
	sum := sha256.Sum256(content)
	hexSum := hex.EncodeToString(sum[:])

	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile(clientAssetName, content, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	sums := fmt.Sprintf("%s  %s\ndeadbeef  other-file\n", hexSum, clientAssetName)
	if err := verifyChecksum(root, clientAssetName, sums, clientAssetName); err != nil {
		t.Fatalf("verifyChecksum(match): %v", err)
	}
	if err := verifyChecksum(root, clientAssetName, "0000  "+clientAssetName+"\n", clientAssetName); err == nil {
		t.Fatal("verifyChecksum(mismatch) accepted")
	}
	if err := verifyChecksum(root, clientAssetName, sums, "missing.exe"); err == nil {
		t.Fatal("verifyChecksum(missing asset) accepted")
	}
}

func TestVerifySignedManifest(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	withUpdatePublicKeys(t, base64.StdEncoding.EncodeToString(publicKey))

	manifest := []byte(manifestVersionPrefix + "v1.2.3\nabc  " + clientAssetName + "\n")
	signature := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, manifest)))
	if err := verifySignedManifest(manifest, signature, "v1.2.3"); err != nil {
		t.Fatalf("verify signed manifest: %v", err)
	}
	if err := verifySignedManifest(append([]byte(nil), manifest...), signature, "v1.2.4"); err == nil {
		t.Fatal("accepted manifest for a different release")
	}
	tampered := append([]byte(nil), manifest...)
	tampered[len(tampered)-2] ^= 1
	if err := verifySignedManifest(tampered, signature, "v1.2.3"); err == nil {
		t.Fatal("accepted tampered manifest")
	}
}

func TestVerifySignedManifestFailsClosedWithoutKey(t *testing.T) {
	withUpdatePublicKeys(t, "")
	if err := verifySignedManifest([]byte("manifest"), []byte("signature"), "v1"); err == nil {
		t.Fatal("accepted manifest without a configured public key")
	}
}

func TestDownloadToEnforcesLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("too large"))
	}))
	defer srv.Close()

	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer func() { _ = root.Close() }()
	err = downloadTo(context.Background(), srv.URL, root, "download", 4, func(int) {})
	if err == nil {
		t.Fatal("expected oversized download to fail")
	}
	if _, statErr := root.Stat("download"); !os.IsNotExist(statErr) {
		t.Fatalf("partial download was not removed: %v", statErr)
	}
}

func TestDownloadToRejectsInsecureRemoteURL(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer func() { _ = root.Close() }()
	err = downloadTo(context.Background(), "http://example.com/update", root, "download", 1024, func(int) {})
	if err == nil {
		t.Fatal("expected insecure remote URL to fail")
	}
}

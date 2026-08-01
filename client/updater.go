// updater.go implements GitHub-based auto-update for the client: check the
// latest release, download the Windows asset with progress, verify its
// SHA-256 against checksums.txt, and self-apply via minio/selfupdate.
//
// SECURITY NOTE: SHA-256 verification guards against corruption and mirror
// tampering on the download path, NOT authenticity — anyone able to publish
// a release to the repo controls the binary. Signed releases (sigstore or
// minisign) are future work.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/selfupdate"
	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"voicx/internal/version"
)

// updateAPIBase is the GitHub API base URL (overridable in tests).
var updateAPIBase = "https://api.github.com"

// UpdateInfo describes an available update (or the absence of one).
type UpdateInfo struct {
	Available bool   `json:"available"`
	Version   string `json:"version"`
	URL       string `json:"url"`
	SHA256URL string `json:"sha256url"`
	Size      int64  `json:"size"`
}

// githubRelease is the subset of the GitHub release API response we use.
type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

const (
	clientAssetName = "voicx-client-windows-amd64.exe"
	checksumsName   = "checksums.txt"
	updateTimeout   = 15 * time.Second
)

// fetchLatestRelease queries the latest release from the configured repo.
func fetchLatestRelease(apiBase string) (*githubRelease, error) {
	if version.UpdateRepo == "" || version.UpdateRepo == "voicx/voicx" {
		return nil, fmt.Errorf("no update source configured (UpdateRepo placeholder)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/repos/%s/releases/latest", apiBase, version.UpdateRepo), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "voicx-client/"+version.Short())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("checking for updates: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update check failed: GitHub returned %s", resp.Status)
	}
	var rel githubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("parsing release info: %w", err)
	}
	return &rel, nil
}

// CheckForUpdate compares the latest GitHub release against the embedded
// version and returns update metadata.
func (a *App) CheckForUpdate() (UpdateInfo, error) {
	rel, err := fetchLatestRelease(updateAPIBase)
	if err != nil {
		return UpdateInfo{}, err
	}
	if !version.Compare(rel.TagName, version.Short()) {
		return UpdateInfo{Available: false, Version: version.String()}, nil
	}

	info := UpdateInfo{Available: true, Version: rel.TagName}
	for _, asset := range rel.Assets {
		switch asset.Name {
		case clientAssetName:
			info.URL = asset.URL
			info.Size = asset.Size
		case checksumsName:
			info.SHA256URL = asset.URL
		}
	}
	if info.URL == "" {
		return UpdateInfo{}, fmt.Errorf("release %s has no %s asset", rel.TagName, clientAssetName)
	}
	if info.SHA256URL == "" {
		return UpdateInfo{}, fmt.Errorf("release %s has no %s", rel.TagName, checksumsName)
	}
	return info, nil
}

// downloadTo downloads url to destPath, reporting progress via onProgress
// (percent 0..100, or -1 when the total is unknown).
func downloadTo(ctx context.Context, url, destPath string, onProgress func(percent int)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: %s", resp.Status)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()

	total := resp.ContentLength
	var written int64
	buf := make([]byte, 64*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
			written += int64(n)
			if total > 0 {
				onProgress(int(written * 100 / total))
			} else {
				onProgress(-1)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// verifyChecksum checks that the file at filePath matches the SHA-256 line
// for assetName in checksumsText ("<hex>  <name>" per line).
func verifyChecksum(filePath, checksumsText, assetName string) error {
	var want string
	for _, line := range strings.Split(checksumsText, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == assetName {
			want = fields[0]
			break
		}
	}
	if want == "" {
		return fmt.Errorf("no checksum found for %s", assetName)
	}

	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, want)
	}
	return nil
}

// DownloadAndApply downloads the update asset, verifies its checksum, and
// self-applies it (renaming the running exe via minio/selfupdate). It emits
// update_progress events with percent values. Returns "" on success or the
// failure reason; on failure the old version keeps running untouched.
func (a *App) DownloadAndApply(info UpdateInfo) string {
	ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
	defer cancel()

	tmpDir, err := os.MkdirTemp("", "voicx-update-*")
	if err != nil {
		return err.Error()
	}
	defer os.RemoveAll(tmpDir)
	exePath := filepath.Join(tmpDir, clientAssetName)
	sumsPath := filepath.Join(tmpDir, checksumsName)

	a.emitUpdateProgress(0)
	if err := downloadTo(ctx, info.URL, exePath, a.emitUpdateProgress); err != nil {
		return err.Error()
	}
	if err := downloadTo(ctx, info.SHA256URL, sumsPath, func(int) {}); err != nil {
		return err.Error()
	}
	sums, err := os.ReadFile(sumsPath)
	if err != nil {
		return err.Error()
	}
	if err := verifyChecksum(exePath, string(sums), clientAssetName); err != nil {
		return "update rejected: " + err.Error()
	}

	f, err := os.Open(exePath)
	if err != nil {
		return err.Error()
	}
	defer f.Close()
	if err := selfupdate.Apply(f, selfupdate.Options{}); err != nil {
		return fmt.Sprintf("self-update failed (old version still running): %v", err)
	}
	return ""
}

// emitUpdateProgress sends an update_progress event to the frontend.
func (a *App) emitUpdateProgress(percent int) {
	if a.ctx != nil {
		wailsRuntime.EventsEmit(a.ctx, "update_progress", percent)
	}
}

// ApplyAndRestart relaunches the (just-updated) executable and quits the
// current process. Only call after user confirmation.
func (a *App) ApplyAndRestart() string {
	exe, err := os.Executable()
	if err != nil {
		return err.Error()
	}
	cmd := exec.Command(exe)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err.Error()
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		os.Exit(0)
	}()
	return ""
}

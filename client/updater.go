// updater.go implements GitHub-based auto-update for the client: check the
// latest release, authenticate its signed manifest, download the Windows
// asset with progress, verify its SHA-256, and self-apply via minio/selfupdate.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
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
	Available    bool   `json:"available"`
	Version      string `json:"version"`
	URL          string `json:"url"`
	SHA256URL    string `json:"sha256url"`
	SignatureURL string `json:"signatureUrl"`
	Size         int64  `json:"size"`
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
	clientAssetName        = "voicx-client-windows-amd64.exe"
	checksumsName          = "checksums.txt"
	checksumsSignatureName = "checksums.txt.sig"
	manifestVersionPrefix  = "# voicx-version: "
	metadataTimeout        = 10 * time.Second
	updateTimeout          = 10 * time.Minute
	maxReleaseMetadata     = 1 << 20
	maxManifestSize        = 1 << 20
	maxSignatureSize       = 16 << 10
	maxClientAssetSize     = 512 << 20
)

var updateHTTPClient = &http.Client{
	Timeout: updateTimeout,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many update redirects")
		}
		return validateUpdateURL(req.URL)
	},
}

func validateUpdateURL(u *url.URL) error {
	if u == nil || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("invalid update URL")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
		ip := net.ParseIP(host)
		if host == "localhost" || ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return fmt.Errorf("update URL must use HTTPS")
}

func parseUpdateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse update URL: %w", err)
	}
	if err := validateUpdateURL(u); err != nil {
		return nil, err
	}
	return u, nil
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("response exceeds %d-byte limit", limit)
	}
	return b, nil
}

// fetchLatestRelease queries the latest release from the configured repo.
func fetchLatestRelease(apiBase string) (*githubRelease, error) {
	if version.UpdateRepo == "" || version.UpdateRepo == "voicx/voicx" {
		return nil, fmt.Errorf("no update source configured (UpdateRepo placeholder)")
	}
	owner, repo, ok := strings.Cut(version.UpdateRepo, "/")
	if !ok || !validRepoPart(owner) || !validRepoPart(repo) {
		return nil, fmt.Errorf("invalid update repository")
	}
	base, err := parseUpdateURL(apiBase)
	if err != nil {
		return nil, fmt.Errorf("invalid update API base: %w", err)
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/repos/" +
		url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/releases/latest"
	base.RawQuery = ""
	base.Fragment = ""

	ctx, cancel := context.WithTimeout(context.Background(), metadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create update request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "voicx-client/"+version.Short())

	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("checking for updates: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update check failed: GitHub returned %s", resp.Status)
	}
	body, err := readLimited(resp.Body, maxReleaseMetadata)
	if err != nil {
		return nil, fmt.Errorf("reading release info: %w", err)
	}
	var rel githubRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, fmt.Errorf("parsing release info: %w", err)
	}
	return &rel, nil
}

func validRepoPart(part string) bool {
	if part == "" || part == "." || part == ".." || len(part) > 100 {
		return false
	}
	for _, r := range part {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
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
		case checksumsSignatureName:
			info.SignatureURL = asset.URL
		}
	}
	if info.URL == "" {
		return UpdateInfo{}, fmt.Errorf("release %s has no %s asset", rel.TagName, clientAssetName)
	}
	if info.SHA256URL == "" {
		return UpdateInfo{}, fmt.Errorf("release %s has no %s", rel.TagName, checksumsName)
	}
	if info.SignatureURL == "" {
		return UpdateInfo{}, fmt.Errorf("release %s has no %s", rel.TagName, checksumsSignatureName)
	}
	if info.Size <= 0 || info.Size > maxClientAssetSize {
		return UpdateInfo{}, fmt.Errorf("release %s has invalid client size %d", rel.TagName, info.Size)
	}
	for _, rawURL := range []string{info.URL, info.SHA256URL, info.SignatureURL} {
		if _, err := parseUpdateURL(rawURL); err != nil {
			return UpdateInfo{}, fmt.Errorf("release %s has unsafe asset URL: %w", rel.TagName, err)
		}
	}
	return info, nil
}

// downloadTo downloads rawURL to destPath, enforcing maxBytes and reporting
// progress as 0..100 (or -1 when the total is unknown).
func downloadTo(
	ctx context.Context,
	rawURL string,
	destRoot *os.Root,
	destName string,
	maxBytes int64,
	onProgress func(percent int),
) (retErr error) {
	u, err := parseUpdateURL(rawURL)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("create download request: %w", err)
	}
	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: %s", resp.Status)
	}
	if resp.ContentLength > maxBytes {
		return fmt.Errorf("download exceeds %d-byte limit", maxBytes)
	}

	out, err := destRoot.OpenFile(destName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create update file: %w", err)
	}
	defer func() {
		if err := out.Close(); retErr == nil && err != nil {
			retErr = fmt.Errorf("close update file: %w", err)
		}
		if retErr != nil {
			_ = destRoot.Remove(destName)
		}
	}()

	total := resp.ContentLength
	var written int64
	buf := make([]byte, 64*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if written > maxBytes-int64(n) {
				return fmt.Errorf("download exceeds %d-byte limit", maxBytes)
			}
			if _, werr := out.Write(buf[:n]); werr != nil {
				return fmt.Errorf("write update file: %w", werr)
			}
			written += int64(n)
			if total > 0 {
				percent := written * 100 / total
				if percent > 100 {
					percent = 100
				}
				onProgress(int(percent))
			} else {
				onProgress(-1)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read update response: %w", err)
		}
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("sync update file: %w", err)
	}
	return nil
}

func verifySignedManifest(manifest, signature []byte, expectedVersion string) error {
	keys, err := updatePublicKeys()
	if err != nil {
		return err
	}
	sig, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(string(signature)))
	if err != nil {
		return fmt.Errorf("decode update signature: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("invalid update signature length")
	}
	authenticated := false
	for _, key := range keys {
		if ed25519.Verify(key, manifest, sig) {
			authenticated = true
			break
		}
	}
	if !authenticated {
		return fmt.Errorf("update manifest signature is not trusted")
	}

	for _, line := range strings.Split(string(manifest), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, manifestVersionPrefix) {
			if got := strings.TrimSpace(strings.TrimPrefix(line, manifestVersionPrefix)); got != expectedVersion {
				return fmt.Errorf("signed manifest version %q does not match release %q", got, expectedVersion)
			}
			return nil
		}
	}
	return fmt.Errorf("signed manifest has no release version")
}

func updatePublicKeys() ([]ed25519.PublicKey, error) {
	if strings.TrimSpace(version.UpdatePublicKeys) == "" {
		return nil, fmt.Errorf("no trusted update signing key is configured")
	}
	encodedKeys := strings.Split(version.UpdatePublicKeys, ",")
	keys := make([]ed25519.PublicKey, 0, len(encodedKeys))
	for _, encoded := range encodedKeys {
		decoded, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(encoded))
		if err != nil {
			return nil, fmt.Errorf("decode trusted update signing key: %w", err)
		}
		if len(decoded) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("invalid trusted update signing key length")
		}
		keys = append(keys, ed25519.PublicKey(decoded))
	}
	return keys, nil
}

// verifyChecksum checks that the file at filePath matches the SHA-256 line
// for assetName in checksumsText ("<hex>  <name>" per line).
func verifyChecksum(root *os.Root, fileName, checksumsText, assetName string) error {
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

	f, err := root.Open(fileName)
	if err != nil {
		return fmt.Errorf("open update for checksum: %w", err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash update: %w", err)
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
	fresh, err := a.CheckForUpdate()
	if err != nil {
		return "update metadata could not be revalidated: " + err.Error()
	}
	if !fresh.Available {
		return "update is no longer available"
	}
	if fresh.Version != info.Version {
		return fmt.Sprintf("update changed from %s to %s; confirm the new version", info.Version, fresh.Version)
	}
	info = fresh

	ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
	defer cancel()

	tmpDir, err := os.MkdirTemp("", "voicx-update-*")
	if err != nil {
		return err.Error()
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	tmpRoot, err := os.OpenRoot(tmpDir)
	if err != nil {
		return err.Error()
	}
	defer func() { _ = tmpRoot.Close() }()

	a.emitUpdateProgress(0)
	if err := downloadTo(ctx, info.SHA256URL, tmpRoot, checksumsName, maxManifestSize, func(int) {}); err != nil {
		return err.Error()
	}
	if err := downloadTo(ctx, info.SignatureURL, tmpRoot, checksumsSignatureName, maxSignatureSize, func(int) {}); err != nil {
		return err.Error()
	}
	sums, err := tmpRoot.ReadFile(checksumsName)
	if err != nil {
		return err.Error()
	}
	signature, err := tmpRoot.ReadFile(checksumsSignatureName)
	if err != nil {
		return err.Error()
	}
	if err := verifySignedManifest(sums, signature, info.Version); err != nil {
		return "update rejected: " + err.Error()
	}
	if err := downloadTo(ctx, info.URL, tmpRoot, clientAssetName, maxClientAssetSize, a.emitUpdateProgress); err != nil {
		return err.Error()
	}
	if err := verifyChecksum(tmpRoot, clientAssetName, string(sums), clientAssetName); err != nil {
		return "update rejected: " + err.Error()
	}

	f, err := tmpRoot.Open(clientAssetName)
	if err != nil {
		return err.Error()
	}
	if err := selfupdate.Apply(f, selfupdate.Options{}); err != nil {
		_ = f.Close()
		return fmt.Sprintf("self-update failed (old version still running): %v", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Sprintf("close applied update: %v", err)
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
	// #nosec G204 -- exe is the current executable path returned by the OS, not user input.
	cmd := exec.Command(exe)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err.Error()
	}
	// recover is per-goroutine: the restart worker needs its own guard (331).
	go guardCrash("updater", func() {
		time.Sleep(500 * time.Millisecond)
		os.Exit(0)
	})
	return ""
}

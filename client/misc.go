// misc.go holds the smaller Wails bindings: chat logging, folder pickers,
// log-folder reveal, and identity management.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

var (
	miscUserConfigDir = os.UserConfigDir
	chatLogNow        = time.Now
)

// configDir returns the voicx config directory, creating it if needed.
func configDir() (string, error) {
	dir, err := miscUserConfigDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "voicx")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	return dir, nil
}

// LogChat appends one line to the chat log (the frontend only calls this for
// messages whose scope is enabled in settings).
func (a *App) LogChat(line string) {
	dir, err := configDir()
	if err != nil {
		return
	}
	appendDailyLog(dir, "chat.log", fmt.Sprintf("[%s] %s\n", chatLogNow().Format("2006-01-02 15:04:05"), line))
}

// OpenLogFolder reveals the config folder in the OS file manager
// (Windows-only; errors are returned to the UI).
func (a *App) OpenLogFolder() string {
	dir, err := configDir()
	if err != nil {
		return err.Error()
	}
	// #nosec G204 -- explorer.exe is fixed and the application-owned config
	// directory is passed as one argument; no command shell parses it.
	if err := exec.CommandContext(context.Background(), "explorer.exe", dir).Start(); err != nil {
		return err.Error()
	}
	return ""
}

// PickDownloadFolder opens a directory picker and returns the chosen path
// ("" when cancelled). The frontend stores it in settings.
func (a *App) PickDownloadFolder() string {
	dir, err := wailsRuntime.OpenDirectoryDialog(a.ctx, wailsRuntime.OpenDialogOptions{
		Title: "Choose download folder",
	})
	if err != nil {
		log.Printf("directory dialog: %v", err)
		return ""
	}
	return dir
}

// identityInfo describes the current identity for the Security settings page.
type identityInfo struct {
	UniqueID  string `json:"unique_id"`
	CreatedAt string `json:"created_at,omitempty"`
	Path      string `json:"path"`
}

// IdentityInfo returns the current identity's unique ID and file info.
func (a *App) IdentityInfo() identityInfo {
	a.identityMu.Lock()
	defer a.identityMu.Unlock()
	out := identityInfo{}
	id, path, err := a.activeIdentityLocked()
	if err != nil {
		return out
	}
	out.Path = path
	if info, err := os.Stat(path); err == nil {
		out.CreatedAt = info.ModTime().Format("2006-01-02 15:04:05")
	}
	out.UniqueID, _ = id.uniqueID()
	return out
}

// RegenerateIdentity replaces the identity with a fresh key pair and returns
// the new unique ID (or an error). A reconnect is required afterwards.
func (a *App) RegenerateIdentity() string {
	a.identityMu.Lock()
	defer a.identityMu.Unlock()
	_, path, err := a.resolveActive()
	if err != nil {
		return err.Error()
	}
	id, err := newIdentity(strings.TrimSuffix(filepath.Base(path), ".json"))
	if err != nil {
		return err.Error()
	}
	if err := saveIdentityAt(path, id); err != nil {
		return err.Error()
	}
	a.forgetCachedIdentity(id)
	uid, err := id.uniqueID()
	if err != nil {
		return err.Error()
	}
	log.Printf("identity regenerated: %s", uid)
	return uid
}

// ExportIdentity writes a PORTABLE copy of one identity ("" = the active
// one) to a user-chosen destination and stamps its backup marker (351/353).
// The copy carries the private key in the clear so it still opens on a new
// machine even when the stored file is OS-protected (354).
func (a *App) ExportIdentity(id string) string {
	// Prepare a detached snapshot under identityMu, but never hold that lock
	// across the native dialog or destination write.
	a.identityMu.Lock()
	if id == "" {
		var err error
		if id, _, err = a.resolveActive(); err != nil {
			a.identityMu.Unlock()
			return err.Error()
		}
	}
	src, err := identityPathFor(id)
	if err != nil {
		a.identityMu.Unlock()
		return err.Error()
	}
	loaded, err := loadIdentityAtStrict(src)
	if err != nil {
		a.identityMu.Unlock()
		return err.Error()
	}
	a.identityMu.Unlock()
	dest, err := wailsRuntime.SaveFileDialog(a.ctx, wailsRuntime.SaveDialogOptions{
		Title:           "Export identity",
		DefaultFilename: "voicx-identity-" + id + ".json",
	})
	if err != nil || dest == "" {
		return "" // cancelled
	}
	out := *loaded
	out.Protection = ""
	out.ExportedAt = time.Now().Unix()
	// #nosec G117 -- portable identity backup intentionally contains its private key and is written owner-only.
	raw, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return err.Error()
	}
	if err := writePrivateFileAtomic(dest, raw); err != nil {
		return err.Error()
	}
	// Commit the backup marker only after re-reading the live source while
	// serialized with regenerate/import/delete. stampIdentityExported compares
	// UIDs, so a stale exported snapshot can never overwrite a new key.
	a.identityMu.Lock()
	err = stampIdentityExported(src, loaded, out.ExportedAt)
	a.identityMu.Unlock()
	if err != nil {
		return err.Error()
	}
	return ""
}

// ImportIdentity adds a user-chosen identity file as a NEW identity and makes
// it active (351). Importing never overwrites a stored key: the file it would
// replace may be the only copy of an account the user still needs.
func (a *App) ImportIdentity() string {
	src, err := wailsRuntime.OpenFileDialog(a.ctx, wailsRuntime.OpenDialogOptions{
		Title: "Import identity",
		Filters: []wailsRuntime.FileFilter{
			{DisplayName: "Identity files (*.json)", Pattern: "*.json"},
		},
	})
	if err != nil || src == "" {
		return "" // cancelled
	}
	// #nosec G304 -- src is explicitly selected by the local user in the
	// native identity-import dialog and must be read to perform the import.
	data, err := os.ReadFile(src)
	if err != nil {
		return err.Error()
	}
	// Decode BEFORE anything is written: a protected file from another
	// machine has to be rejected with a message, not stored unreadable (354).
	loaded, err := decodeIdentity(data)
	if err != nil {
		return err.Error()
	}
	a.identityMu.Lock()
	msg := a.adoptImportedIdentityLocked(loaded, filepath.Base(src))
	a.identityMu.Unlock()
	if msg != "" {
		return msg
	}
	log.Printf("identity imported from %s", src)
	return ""
}

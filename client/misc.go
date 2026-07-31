// misc.go holds the smaller Wails bindings: chat logging, folder pickers,
// log-folder reveal, and identity management.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// configDir returns the voicx config directory, creating it if needed.
func configDir() (string, error) {
	dir, err := os.UserConfigDir()
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
	f, err := os.OpenFile(filepath.Join(dir, "chat.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), line)
}

// OpenLogFolder reveals the config folder in the OS file manager
// (Windows-only; errors are returned to the UI).
func (a *App) OpenLogFolder() string {
	dir, err := configDir()
	if err != nil {
		return err.Error()
	}
	if err := exec.Command("explorer", dir).Start(); err != nil {
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
	out := identityInfo{}
	path, err := identityPath()
	if err != nil {
		return out
	}
	out.Path = path
	if info, err := os.Stat(path); err == nil {
		out.CreatedAt = info.ModTime().Format("2006-01-02 15:04:05")
	}
	out.UniqueID = a.IdentityUID()
	return out
}

// RegenerateIdentity replaces the identity with a fresh key pair and returns
// the new unique ID (or an error). A reconnect is required afterwards.
func (a *App) RegenerateIdentity() string {
	path, err := identityPath()
	if err != nil {
		return err.Error()
	}
	_ = os.Remove(path)
	id, err := loadOrCreateIdentityAt(path)
	if err != nil {
		return err.Error()
	}
	if a.cm != nil {
		a.cm.id = id
	}
	uid, err := id.uniqueID()
	if err != nil {
		return err.Error()
	}
	log.Printf("identity regenerated: %s", uid)
	return uid
}

// ExportIdentity copies the identity file to a user-chosen destination.
func (a *App) ExportIdentity() string {
	src, err := identityPath()
	if err != nil {
		return err.Error()
	}
	dest, err := wailsRuntime.SaveFileDialog(a.ctx, wailsRuntime.SaveDialogOptions{
		Title:           "Export identity",
		DefaultFilename: "voicx-identity.json",
	})
	if err != nil || dest == "" {
		return "" // cancelled
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err.Error()
	}
	if err := os.WriteFile(dest, data, 0o600); err != nil {
		return err.Error()
	}
	return ""
}

// ImportIdentity replaces the identity file with a user-chosen one (a backup
// of the current file is kept alongside). A reconnect is required.
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
	data, err := os.ReadFile(src)
	if err != nil {
		return err.Error()
	}
	// Validate before replacing anything.
	var id identity
	if err := json.Unmarshal(data, &id); err != nil || id.PublicKey == "" || id.PrivateKey == "" {
		return "not a valid identity file"
	}
	dest, err := identityPath()
	if err != nil {
		return err.Error()
	}
	if _, err := os.Stat(dest); err == nil {
		_ = os.Rename(dest, dest+".bak")
	}
	if err := os.WriteFile(dest, data, 0o600); err != nil {
		return err.Error()
	}
	if _, err := loadOrCreateIdentityAt(dest); err != nil {
		return err.Error()
	}
	if a.cm != nil {
		a.cm.id = nil // reload on next connect
	}
	log.Printf("identity imported from %s", src)
	return ""
}

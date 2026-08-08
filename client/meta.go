// meta.go implements wave-8b meta features: log export (326), the debug
// console's frame tee (327), the crash handler (331), and what's-new
// tracking (330).
package main

import (
	"archive/zip"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"voicx/internal/version"
)

// logDir returns the client log directory (<UserConfigDir>/voicx).
func logDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "voicx"), nil
}

func openLogRoot() (*os.Root, error) {
	dir, err := logDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return os.OpenRoot(dir)
}

// ExportLogs writes client.log + chat.log into a zip via the save dialog
// (326). Returns "" on success or cancel, or the error.
func (a *App) ExportLogs() string {
	dest, err := wailsRuntime.SaveFileDialog(a.ctx, wailsRuntime.SaveDialogOptions{
		Title:           "Export logs",
		DefaultFilename: fmt.Sprintf("voicx-logs-%s.zip", time.Now().Format("20060102-150405")),
		Filters:         []wailsRuntime.FileFilter{{DisplayName: "Zip archives", Pattern: "*.zip"}},
	})
	if err != nil || dest == "" {
		return ""
	}
	root, err := openLogRoot()
	if err != nil {
		return err.Error()
	}
	if err := exportLogsTo(root, dest); err != nil {
		return err.Error()
	}
	return ""
}

func exportLogsTo(root *os.Root, dest string) (retErr error) {
	defer func() { retErr = errors.Join(retErr, root.Close()) }()
	// #nosec G304 -- dest is explicitly selected by the local user in the
	// native save dialog; exporting there is the requested operation.
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, out.Close()) }()
	zw := zip.NewWriter(out)
	entries, _ := fs.ReadDir(root.FS(), ".")
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".log" {
			continue
		}
		raw, err := root.ReadFile(name)
		if err != nil {
			continue // a missing log is fine
		}
		w, err := zw.Create(name)
		if err != nil {
			continue
		}
		_, _ = w.Write(raw)
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return nil
}

// SetDebugFrames toggles the debug console's frame tee (327).
func (a *App) SetDebugFrames(on bool) {
	if cm := a.cmLoad(); cm != nil {
		cm.mu.Lock()
		cm.debugFrames = on
		cm.mu.Unlock()
	}
}

// --- crash handler (331) -------------------------------------------------------

const crashLogName = "crash.log"

// guardCrash recovers from a panic in fn, writes a timestamped crash log,
// and re-panics so the process still exits (the log is offered at the next
// start).
func guardCrash(context string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			root, err := openLogRoot()
			if err == nil {
				f, err := root.OpenFile(crashLogName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
				if err == nil {
					_, _ = fmt.Fprintf(f, "\n=== %s: panic in %s: %v\n%s\n",
						time.Now().Format(time.RFC3339), context, r, debug.Stack())
					_ = f.Close()
				}
				_ = root.Close()
			}
			panic(r)
		}
	}()
	fn()
}

// LastCrash returns the crash log's tail when one exists (offered at
// startup; cleared after reading).
func (a *App) LastCrash() string {
	root, err := openLogRoot()
	if err != nil {
		return ""
	}
	defer func() { _ = root.Close() }()
	raw, err := root.ReadFile(crashLogName)
	if err != nil || len(raw) == 0 {
		return ""
	}
	_ = root.Remove(crashLogName) // report once
	const max = 4000
	if len(raw) > max {
		raw = raw[len(raw)-max:]
	}
	return string(raw)
}

// --- what's new (330) ----------------------------------------------------------

// whatsNewNotes are the bundled offline release notes shown when the client
// version changed since the last run (330). Newest first.
var whatsNewNotes = map[string]string{
	"0.4": `Wave 6-8 highlights:
• Permission Manager, group management, audit log viewer
• File browser with folders, versions, dedup, download links
• Multi-server tabs, tray icon, themes, hotkey profiles
• Presence status, pokes, contacts, debug console`,
}

// WhatsNew returns the release notes when the client version changed since
// the last run (and marks it seen). "" when nothing new.
func (a *App) WhatsNew() string {
	cur := version.Short()
	if a.settings.LastSeenVersion == cur {
		return ""
	}
	a.settings.LastSeenVersion = cur
	_ = a.save()
	for v, notes := range whatsNewNotes {
		if cur == v || len(cur) > len(v) && cur[:len(v)] == v {
			return notes
		}
	}
	// Unknown exact version: show the newest notes as a fallback.
	return whatsNewNotes["0.4"]
}

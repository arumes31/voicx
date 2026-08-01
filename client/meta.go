// meta.go implements wave-8b meta features: log export (326), the debug
// console's frame tee (327), the crash handler (331), and what's-new
// tracking (330).
package main

import (
	"archive/zip"
	"fmt"
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

// ExportLogs writes client.log + chat.log into a zip via the save dialog
// (326). Returns "" on success or cancel, or the error.
func (a *App) ExportLogs() string {
	dir, err := logDir()
	if err != nil {
		return err.Error()
	}
	dest, err := wailsRuntime.SaveFileDialog(a.ctx, wailsRuntime.SaveDialogOptions{
		Title:           "Export logs",
		DefaultFilename: fmt.Sprintf("voicx-logs-%s.zip", time.Now().Format("20060102-150405")),
		Filters:         []wailsRuntime.FileFilter{{DisplayName: "Zip archives", Pattern: "*.zip"}},
	})
	if err != nil || dest == "" {
		return ""
	}
	out, err := os.Create(dest)
	if err != nil {
		return err.Error()
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	for _, name := range []string{"client.log", "chat.log", "crash.log"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
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
		return err.Error()
	}
	return ""
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

// crashLogPath returns the crash log location inside the config dir.
func crashLogPath() (string, error) {
	dir, err := logDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "crash.log"), nil
}

// guardCrash recovers from a panic in fn, writes a timestamped crash log,
// and re-panics so the process still exits (the log is offered at the next
// start).
func guardCrash(context string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			path, err := crashLogPath()
			if err == nil {
				f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
				if err == nil {
					fmt.Fprintf(f, "\n=== %s: panic in %s: %v\n%s\n",
						time.Now().Format(time.RFC3339), context, r, debug.Stack())
					_ = f.Close()
				}
			}
			panic(r)
		}
	}()
	fn()
}

// LastCrash returns the crash log's tail when one exists (offered at
// startup; cleared after reading).
func (a *App) LastCrash() string {
	path, err := crashLogPath()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return ""
	}
	_ = os.Remove(path) // report once
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

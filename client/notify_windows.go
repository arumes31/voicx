// notify_windows.go implements native Windows notifications (345) via a
// PowerShell NotifyIcon balloon. On this machine (Windows, no packaged
// AppUserModelID) the WinRT toast API is unreliable for unpackaged apps,
// while the WinForms balloon works — hence this approach. The process
// overhead of a short PowerShell is accepted because notifications are
// rate-limited by the callers (mentions/pokes only).
package main

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"
)

// shellQuote escapes a string for safe embedding in a single-quoted
// PowerShell literal.
func shellQuote(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// Notify posts a native balloon notification (345). Errors are reported to
// the caller; the frontend falls back to its own toasts + FlashWindow.
func (a *App) Notify(title, text string) string {
	if len(text) > 240 {
		text = text[:240]
	}
	ps := fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms; Add-Type -AssemblyName System.Drawing; `+
		`$n = New-Object System.Windows.Forms.NotifyIcon; $n.Icon = [System.Drawing.SystemIcons]::Information; `+
		`$n.Visible = $true; $n.ShowBalloonTip(3000, '%s', '%s', [System.Windows.Forms.ToolTipIcon]::Info); `+
		`Start-Sleep -Milliseconds 500; $n.Dispose()`, shellQuote(title), shellQuote(text))
	cmd := exec.Command("powershell.exe", "-NoProfile", "-Command", ps)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Run(); err != nil {
		return "native notification failed: " + err.Error()
	}
	return ""
}

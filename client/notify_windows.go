// notify_windows.go implements native Windows notifications (345) via a
// PowerShell NotifyIcon balloon. On this machine (Windows, no packaged
// AppUserModelID) the WinRT toast API is unreliable for unpackaged apps,
// while the WinForms balloon works — hence this approach. The process
// overhead of a short PowerShell is accepted because notifications are
// rate-limited by the callers (mentions/pokes only).
package main

import (
	"encoding/base64"
	"os"
	"os/exec"
	"syscall"
)

const notifyPowerShell = `$title = [System.Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($env:VOICX_NOTIFY_TITLE_B64)); ` +
	`$text = [System.Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($env:VOICX_NOTIFY_TEXT_B64)); ` +
	`Add-Type -AssemblyName System.Windows.Forms; Add-Type -AssemblyName System.Drawing; ` +
	`$n = New-Object System.Windows.Forms.NotifyIcon; $n.Icon = [System.Drawing.SystemIcons]::Information; ` +
	`$n.Visible = $true; $n.ShowBalloonTip(3000, $title, $text, [System.Windows.Forms.ToolTipIcon]::Info); ` +
	`Start-Sleep -Milliseconds 500; $n.Dispose()`

// Notify posts a native balloon notification (345). Errors are reported to
// the caller; the frontend falls back to its own toasts + FlashWindow.
func (a *App) Notify(title, text string) string {
	if len(text) > 240 {
		text = text[:240]
	}
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", notifyPowerShell)
	cmd.Env = append(os.Environ(),
		"VOICX_NOTIFY_TITLE_B64="+base64.StdEncoding.EncodeToString([]byte(title)),
		"VOICX_NOTIFY_TEXT_B64="+base64.StdEncoding.EncodeToString([]byte(text)),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Run(); err != nil {
		return "native notification failed: " + err.Error()
	}
	return ""
}

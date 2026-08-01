//go:build !windows

// notify_other.go stubs native notifications on non-Windows platforms (345
// is a Windows feature; the frontend falls back to toasts + FlashWindow).
package main

// Notify reports unsupported on this platform.
func (a *App) Notify(title, text string) string {
	return "native notifications are only supported on Windows"
}

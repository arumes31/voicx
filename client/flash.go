// flash.go exposes the taskbar flash binding.
package main

// FlashWindow flashes the main window's taskbar button (Windows; no-op
// elsewhere). It returns "" on success or the failure reason.
func (a *App) FlashWindow() string {
	if err := flashWindowOnce(); err != nil {
		return err.Error()
	}
	return ""
}

//go:build windows

// flash_windows.go implements the taskbar flash for whisper notifications
// using the Win32 FlashWindow API. Wails doesn't expose the window handle,
// so we find our own top-level window by process ID via EnumWindows.
package main

import (
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32              = windows.NewLazySystemDLL("user32.dll")
	procEnumWindows     = user32.NewProc("EnumWindows")
	procFlashWindow     = user32.NewProc("FlashWindow")
	procGetWindowPID    = user32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible = user32.NewProc("IsWindowVisible")
)

// findMainWindow returns the first visible top-level window owned by this
// process, or 0.
func findMainWindow() uintptr {
	pid := uint32(os.Getpid())
	var hwnd uintptr
	cb := syscall.NewCallback(func(h uintptr, l uintptr) uintptr {
		var windowPID uint32
		_, _, _ = procGetWindowPID.Call(h, uintptr(unsafe.Pointer(&windowPID)))
		if windowPID == pid {
			visible, _, _ := procIsWindowVisible.Call(h)
			if visible != 0 {
				hwnd = h
				return 0 // stop enumerating
			}
		}
		return 1 // continue
	})
	_, _, _ = procEnumWindows.Call(cb, 0)
	return hwnd
}

// flashWindowOnce flashes the main window's taskbar button once.
func flashWindowOnce() error {
	hwnd := findMainWindow()
	if hwnd == 0 {
		return nil // headless or no window yet: nothing to flash
	}
	_, _, _ = procFlashWindow.Call(hwnd, 1)
	return nil
}

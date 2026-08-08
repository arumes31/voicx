//go:build windows && (amd64 || arm64)

// opacity_windows.go implements window transparency (292) with the Win32
// layered-window API. Wails v2 has no opacity option, but the window is an
// ordinary top-level HWND, so SetLayeredWindowAttributes applies to it — the
// handle comes from the same EnumWindows lookup the taskbar flash uses.
package main

import (
	"fmt"
)

const (
	wsExLayered  = 0x00080000
	lwaAlpha     = 0x00000002
	opacityFloor = 20
	// GWL_EXSTYLE is the signed Win32 index -20 represented in an ABI-sized
	// argument without a narrowing or sign-changing runtime conversion.
	gwlExStyleArg = ^uintptr(19)
)

var (
	procGetWindowLongPtr           = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtr           = user32.NewProc("SetWindowLongPtrW")
	procSetLayeredWindowAttributes = user32.NewProc("SetLayeredWindowAttributes")
)

// setWindowOpacity applies pct (20..100) to the main window. 0 means "not
// configured" and is treated as fully opaque.
func setWindowOpacity(pct int) error {
	if pct <= 0 || pct > 100 {
		pct = 100
	}
	if pct < opacityFloor {
		pct = opacityFloor
	}
	for name, proc := range map[string]interface{ Find() error }{
		"GetWindowLongPtrW":          procGetWindowLongPtr,
		"SetWindowLongPtrW":          procSetWindowLongPtr,
		"SetLayeredWindowAttributes": procSetLayeredWindowAttributes,
	} {
		if err := proc.Find(); err != nil {
			return fmt.Errorf("window opacity unavailable: %s: %w", name, err)
		}
	}
	hwnd := findMainWindow()
	if hwnd == 0 {
		return fmt.Errorf("no window yet")
	}
	style, _, _ := procGetWindowLongPtr.Call(hwnd, gwlExStyleArg)
	if style&wsExLayered == 0 {
		// Call surfaces the thread's last error even on success, so the
		// SetLayeredWindowAttributes result below is the only real check.
		_, _, _ = procSetWindowLongPtr.Call(hwnd, gwlExStyleArg, style|wsExLayered)
	}
	alpha := uintptr(pct * 255 / 100)
	ok, _, err := procSetLayeredWindowAttributes.Call(hwnd, 0, alpha, lwaAlpha)
	if ok == 0 {
		return fmt.Errorf("set opacity: %v", err)
	}
	return nil
}

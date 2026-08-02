//go:build !windows

// opacity_other.go is the non-Windows no-op for window transparency (292):
// neither the GTK nor the Cocoa Wails backend exposes the window handle.
package main

// setWindowOpacity is a no-op on non-Windows platforms.
func setWindowOpacity(int) error { return nil }

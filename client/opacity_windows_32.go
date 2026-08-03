//go:build windows && (386 || arm)

package main

// GetWindowLongPtrW is not available under these Wails/Win32 builds. Opacity
// is an optional visual feature, so unsupported 32-bit clients keep the window
// fully opaque.
func setWindowOpacity(int) error { return nil }

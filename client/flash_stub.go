//go:build !windows

// flash_stub.go is the non-Windows no-op for taskbar flashing.
package main

// flashWindowOnce is a no-op on non-Windows platforms.
func flashWindowOnce() error { return nil }

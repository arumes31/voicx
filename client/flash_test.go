package main

import "testing"

// TestFlashWindow verifies the flash binding is a best-effort no-op under
// test (headless CI has no window; it must not error or panic).
func TestFlashWindow(t *testing.T) {
	a := &App{}
	if err := a.FlashWindow(); err != "" {
		t.Fatalf("FlashWindow returned error: %s", err)
	}
}

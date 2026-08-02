//go:build windows

package main

import (
	"slices"
	"sync"
	"testing"
)

func TestRunWindowsDesktopLifecycle(t *testing.T) {
	var (
		mu     sync.Mutex
		events []string
	)
	record := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}
	trayQuit := make(chan struct{})

	runWindowsDesktop(windowsDesktopLifecycle{
		runWails: func() { record("wails") },
		runTray: func(ready chan<- struct{}) {
			record("tray-ready")
			close(ready)
			<-trayQuit
			record("tray-exit")
		},
		quitTray: func() {
			record("tray-quit")
			close(trayQuit)
		},
	})

	want := []string{"tray-ready", "wails", "tray-quit", "tray-exit"}
	if !slices.Equal(events, want) {
		t.Fatalf("lifecycle events = %v, want %v", events, want)
	}
}

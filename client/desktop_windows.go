//go:build windows

package main

import (
	"runtime"

	"github.com/getlantern/systray"
)

type windowsDesktopLifecycle struct {
	runWails func()
	runTray  func(chan<- struct{})
	quitTray func()
}

// runDesktop keeps Wails on the initial Windows OS thread. Wails' windowing
// backend records that thread during package initialisation and WebView2's COM
// apartment is thread-bound; moving wails.Run to a goroutine leaves its
// asynchronous environment callback without COM initialisation.
func runDesktop(app *App) {
	runWindowsDesktop(windowsDesktopLifecycle{
		runWails: func() { runWails(app) },
		runTray:  func(ready chan<- struct{}) { initTray(app, ready) },
		quitTray: systray.Quit,
	})
}

func runWindowsDesktop(lifecycle windowsDesktopLifecycle) {
	trayReady := make(chan struct{})
	trayDone := make(chan struct{})

	go func() {
		defer close(trayDone)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		guardCrash("tray", func() { lifecycle.runTray(trayReady) })
	}()

	<-trayReady
	defer func() {
		lifecycle.quitTray()
		<-trayDone
	}()

	lifecycle.runWails()
}

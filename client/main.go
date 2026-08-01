package main

import (
	"context"
	"embed"
	"log"
	"os"
	"path/filepath"

	"github.com/getlantern/systray"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// (331) crash handler: a panic anywhere in main writes a timestamped
	// crash log that is offered at the next start.
	guardCrash("main", runApp)
}

// runApp is main's body under the crash guard.
func runApp() {
	// Log to <UserConfigDir>/voicx/client.log — a GUI app has no console, so
	// hotkey/connection failures must be diagnosable from a file.
	if dir, err := os.UserConfigDir(); err == nil {
		logDir := filepath.Join(dir, "voicx")
		if err := os.MkdirAll(logDir, 0o750); err == nil {
			if f, err := os.OpenFile(filepath.Join(logDir, "client.log"),
				os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				log.SetOutput(f)
				defer f.Close()
			}
		}
	}

	// Create an instance of the app structure
	app := NewApp()

	// recover is per-goroutine, so every goroutine the client starts carries
	// its own guard (331) — main's covers only main's stack.
	go guardCrash("wails", func() {
		// Create application with options
		err := wails.Run(&options.App{
			Title:  "voicx-client",
			Width:  1024,
			Height: 768,
			AssetServer: &assetserver.Options{
				Assets: assets,
			},
			BackgroundColour: &options.RGBA{R: 27, G: 38, B: 54, A: 1},
			OnStartup:        app.startup,
			OnShutdown:       app.shutdown,
			// (287) close-to-tray: the close button hides the window instead
			// of quitting when the setting is on.
			OnBeforeClose: func(ctx context.Context) bool {
				if app.settings.CloseToTray {
					wailsRuntime.WindowHide(ctx)
					if trayCtl != nil {
						trayCtl.mu.Lock()
						trayCtl.visible = false
						trayCtl.mu.Unlock()
						trayCtl.miShowHide.SetTitle("Show voicx")
					}
					return true
				}
				return false
			},
			Bind: []interface{}{
				app,
			},
		})
		if err != nil {
			log.Printf("wails run error: %v", err)
			println("Error:", err.Error())
		}
		systray.Quit()
	})

	initTray(app)
}

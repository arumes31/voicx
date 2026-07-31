package main

import (
	"embed"
	"log"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
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
		Bind: []interface{}{
			app,
		},
	})

	if err != nil {
		log.Printf("wails run error: %v", err)
		println("Error:", err.Error())
	}
}

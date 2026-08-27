package main

import (
	"embed"
	"fmt"
	"os"

	"github.com/Hocsman/Relayer/internal/tmuxbackend"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

// The tracked placeholder keeps plain `go test` builds valid. Wails runs the
// frontend build before compiling a distributable application.
//
//go:embed all:frontend/dist
var frontendAssets embed.FS

func main() {
	if handled, exitCode := tmuxbackend.HelperMain(os.Args[1:], os.Stderr); handled {
		os.Exit(exitCode)
	}

	application := NewApp()
	if err := wails.Run(&options.App{
		Title:             "Relayer",
		Width:             1440,
		Height:            900,
		MinWidth:          980,
		MinHeight:         680,
		DisableResize:     false,
		Fullscreen:        false,
		Frameless:         false,
		StartHidden:       false,
		HideWindowOnClose: false,
		BackgroundColour:  &options.RGBA{R: 8, G: 12, B: 20, A: 255},
		AssetServer:       &assetserver.Options{Assets: frontendAssets},
		OnStartup:         application.startup,
		OnShutdown:        application.onShutdown,
		Bind:              []interface{}{application},
	}); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Relayer GUI:", err)
		os.Exit(1)
	}
}

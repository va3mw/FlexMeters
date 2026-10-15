package main

import (
	"context"
	"embed"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/va3mw/FlexMeters/internal/app"
	"github.com/va3mw/FlexMeters/internal/config"
)

//go:embed all:frontend/dist
var assets embed.FS

const version = "0.1.0"

func main() {
	// One log per run, next to settings.json.
	os.MkdirAll(config.Dir(), 0o755)
	var out io.Writer = os.Stderr
	if f, err := os.Create(filepath.Join(config.Dir(), "FlexMeters.log")); err == nil {
		defer f.Close()
		out = io.MultiWriter(os.Stderr, f)
	}
	l := log.New(out, "", log.LstdFlags|log.Lmicroseconds)
	l.Printf("FlexMeters %s starting; settings in %s", version, config.Dir())

	a := app.New(version, l, config.Load())
	err := wails.Run(&options.App{
		Title:            "FlexMeters " + version,
		Width:            640,
		Height:           520,
		MinWidth:         300,
		MinHeight:        200,
		AssetServer:      &assetserver.Options{Assets: assets},
		BackgroundColour: &options.RGBA{R: 22, G: 24, B: 28, A: 1},
		OnStartup: func(ctx context.Context) {
			app.Start(a, func(ev string, data any) { runtime.EventsEmit(ctx, ev, data) })
		},
		OnShutdown: func(context.Context) { app.Stop(a) },
		Bind:       []interface{}{a},
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: "va3mw-flexmeters-1",
		},
	})
	if err != nil {
		l.Printf("wails: %v", err)
	}
}

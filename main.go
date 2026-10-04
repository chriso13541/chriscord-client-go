package main

import (
	"embed"
	"net/http"
	goruntime "runtime"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

//go:embed all:frontend
var assets embed.FS

func main() {
	app := NewApp()

	err := wails.Run(&options.App{
		Title:     "Chriscord",
		Width:     1100,
		Height:    720,
		MinWidth:  700,
		MinHeight: 500,
		AssetServer: &assetserver.Options{
			Assets: assets,
			// Anything not in the bundled frontend: custom emoji images
			// (/cc-emoji/<id>), from a disk cache or the server.
			Handler: http.HandlerFunc(app.serveEmojiAsset),
		},
		BackgroundColour: &options.RGBA{R: 14, G: 17, B: 23, A: 255},
		// The window's title bar is drawn by the app itself, in the user's
		// theme (see #titlebar in index.html):
		//   Windows — no system frame at all; the app draws minimise /
		//             maximise / close, and the window still resizes from
		//             its edges and snaps like any other.
		//   macOS   — the system title bar is hidden but the traffic-light
		//             buttons stay, inset over the app's own bar.
		//   Linux   — the desktop's own title bar is kept: window managers
		//             differ too much for a drawn one to behave well.
		Frameless: goruntime.GOOS == "windows",
		Mac: &mac.Options{
			TitleBar:   mac.TitleBarHiddenInset(),
			Appearance: mac.NSAppearanceNameDarkAqua,
		},
		DragAndDrop: &options.DragAndDrop{
			EnableFileDrop: true,
		},
		OnStartup: app.startup,
		OnShutdown: app.shutdown, // leave any call and close cleanly when the window closes
		Bind: []interface{}{
			app,
		},
	})

	if err != nil {
		println("Error:", err.Error())
	}
}

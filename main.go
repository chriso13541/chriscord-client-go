package main

import (
	"embed"
	"net/http"
	"strings"
	"os"
	goruntime "runtime"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

//go:embed all:frontend
var assets embed.FS

// singleInstanceID names Chriscord's single-instance lock. Keep it the same
// in every build, or an old and a new copy could run side by side.
const singleInstanceID = "app.chriscord.client"

func main() {
	// The uninstaller asks the app to take out its own "open when you sign
	// in" entry (Settings > System): that code lives here, not in the
	// updater. Nothing else runs: no window, no single-instance lock.
	for _, arg := range os.Args[1:] {
		if arg == "--remove-startup" {
			_ = setStartup(false, "")
			return
		}
	}
	app := NewApp()
	allowOtherInstanceToFocus()

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
			// And encrypted attachments (/e2ee-file/<name>), decrypted here.
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, fileLocalPrefix) {
					app.serveE2EEFile(w, r)
					return
				}
				app.serveEmojiAsset(w, r)
			}),
		},
		BackgroundColour: &options.RGBA{R: 14, G: 17, B: 23, A: 255},
		// The window's title bar is drawn by the app itself, in the user's
		// theme (see #titlebar in index.html):
		//   Windows — no system frame at all; the app draws minimise /
		//             maximise / close, and the window still resizes from
		//             its edges and snaps like any other.
		//   macOS   — the system title bar is hidden but the traffic-light
		//             buttons stay, over the app's own bar. (Not the
		//             "inset" style: that adds a toolbar-height strip,
		//             putting the buttons ~26 px down — below a normal-
		//             height bar, over the app itself.)
		//   Linux   — the desktop's own title bar is kept: window managers
		//             differ too much for a drawn one to behave well.
		Frameless: goruntime.GOOS == "windows",
		Mac: &mac.Options{
			TitleBar:   mac.TitleBarHidden(),
			Appearance: mac.NSAppearanceNameDarkAqua,
		},
		DragAndDrop: &options.DragAndDrop{
			EnableFileDrop: true,
		},
		// Only one Chriscord at a time: starting it again (the shortcut, the
		// Start menu, the launcher after an update) brings the window that's
		// already open to the front instead of opening a second one, which
		// would log in twice and fight over the microphone.
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId:               singleInstanceID,
			OnSecondInstanceLaunch: app.onSecondInstanceLaunch,
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

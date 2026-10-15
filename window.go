package main

// window.go — the window between runs, and the tray (Settings > System).
//
//   - Where the window was — which monitor, its size, whether it was
//     maximised — is remembered and restored when Chriscord opens.
//   - Minimize to tray: closing the window (its X, Alt+F4, the taskbar)
//     hides it and leaves Chriscord running — in a call, getting messages —
//     with an icon in the notification area to open it again or quit.
//   - Start minimized: when Chriscord opens at sign-in, it starts in the
//     tray (or minimised, without the tray) instead of popping up.
//
// The window work itself is per system (window_windows.go; elsewhere it's
// not available yet and the window opens as before).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// windowPrefs is window.json in the config folder.
type windowPrefs struct {
	MinimizeToTray bool            `json:"minimize_to_tray"`
	StartMinimized bool            `json:"start_minimized"`
	Placement      *savedPlacement `json:"placement,omitempty"`
}

// savedPlacement: the window's normal (not maximised) rectangle in screen
// pixels, and whether it was maximised.
type savedPlacement struct {
	Left      int32 `json:"left"`
	Top       int32 `json:"top"`
	Right     int32 `json:"right"`
	Bottom    int32 `json:"bottom"`
	Maximized bool  `json:"maximized"`
}

var (
	windowPrefsMu sync.Mutex
	// appQuitting: Chriscord is really closing (Quit in the tray, an
	// update, the X without minimize to tray) — don't hide it instead.
	appQuitting atomic.Bool
	// pendingMaximise: it started in the tray and was maximised last time —
	// maximise it when it's first opened.
	pendingMaximise atomic.Bool
)

func windowPrefsPath() string { return filepath.Join(chriscordConfigDir(), "window.json") }

func loadWindowPrefs() windowPrefs {
	windowPrefsMu.Lock()
	defer windowPrefsMu.Unlock()
	var p windowPrefs
	if data, err := os.ReadFile(windowPrefsPath()); err == nil {
		json.Unmarshal(data, &p)
	}
	return p
}

// updateWindowPrefs changes window.json under the lock.
func updateWindowPrefs(change func(*windowPrefs)) error {
	windowPrefsMu.Lock()
	defer windowPrefsMu.Unlock()
	var p windowPrefs
	if data, err := os.ReadFile(windowPrefsPath()); err == nil {
		json.Unmarshal(data, &p)
	}
	change(&p)
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	os.MkdirAll(chriscordConfigDir(), 0700)
	return os.WriteFile(windowPrefsPath(), data, 0644)
}

// WindowSettings is what Settings > System shows.
type WindowSettings struct {
	Supported      bool `json:"supported"`
	MinimizeToTray bool `json:"minimize_to_tray"`
	StartMinimized bool `json:"start_minimized"`
}

func (a *App) GetWindowSettings() WindowSettings {
	p := loadWindowPrefs()
	return WindowSettings{Supported: windowManagerSupported, MinimizeToTray: p.MinimizeToTray, StartMinimized: p.StartMinimized}
}

func (a *App) SetWindowSettings(minimizeToTray, startMinimized bool) error {
	err := updateWindowPrefs(func(p *windowPrefs) {
		p.MinimizeToTray = minimizeToTray
		p.StartMinimized = startMinimized
	})
	trayShow(a, minimizeToTray)
	return err
}

// CloseWindow is the title bar's X: to the tray with minimize to tray on,
// otherwise Chriscord closes.
func (a *App) CloseWindow() {
	if windowManagerSupported && loadWindowPrefs().MinimizeToTray {
		a.hideToTray()
		return
	}
	a.quitApp()
}

// quitApp closes Chriscord for real (leaving any call; see shutdown).
func (a *App) quitApp() {
	appQuitting.Store(true)
	rememberPlacement()
	trayRemove()
	runtime.Quit(a.ctx)
}

// beforeClose is Wails' OnBeforeClose: the window's being closed by the
// system (Alt+F4, the taskbar). With minimize to tray, hide it instead.
func (a *App) beforeClose(ctx context.Context) bool {
	if !appQuitting.Load() && windowManagerSupported && loadWindowPrefs().MinimizeToTray {
		a.hideToTray()
		return true // keep running
	}
	rememberPlacement()
	trayRemove()
	return false
}

func (a *App) hideToTray() {
	rememberPlacement()
	trayShow(a, true)
	runtime.WindowHide(a.ctx)
}

// showWindow brings the window back (the tray icon, or starting Chriscord
// again while it's running).
func (a *App) showWindow() {
	runtime.WindowShow(a.ctx)
	runtime.WindowUnminimise(a.ctx)
	if pendingMaximise.Swap(false) {
		runtime.WindowMaximise(a.ctx)
	}
	bringToFront()
}

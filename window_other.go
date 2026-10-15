//go:build !windows

package main

// Remembering the window's place and the tray are Windows-only for now: on
// macOS and Linux the window opens as before.

const windowManagerSupported = false

func (a *App) placeWindowAtStart() {}
func rememberPlacement()           {}
func trayShow(*App, bool)          {}
func bringToFront()                {}
func trayRemove()                  {}

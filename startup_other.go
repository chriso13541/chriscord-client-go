//go:build !windows

package main

import "errors"

// Opening at sign-in is Windows-only for now (an autostart .desktop file
// on Linux, a login item on macOS would be the equivalents).

const startupSupported = false

func startupEnabled() bool { return false }
func setStartup(bool, string) error {
	return errors.New("opening at sign-in isn't available on this system yet")
}

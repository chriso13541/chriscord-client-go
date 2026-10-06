//go:build !windows

package main

import "errors"

// Sharing a screen from Linux (X11 through FFmpeg, Wayland through the
// desktop portal) comes later; watching other people's screens works
// everywhere.

func screenShareUnavailable() string {
	return "Sharing your screen from Linux isn't available yet — you can still watch other people's"
}
func screenWindowsSupported() bool                 { return false }
func listShareSources() ([]ShareSource, error)    { return nil, errors.New(screenShareUnavailable()) }
func screenInputArgs(ScreenStart) ([]string, int, int, error) {
	return nil, 0, 0, errors.New(screenShareUnavailable())
}

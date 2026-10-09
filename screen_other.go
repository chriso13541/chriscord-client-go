//go:build !windows

package main

import "errors"

// Sharing a screen from Linux (X11 through FFmpeg, Wayland through the
// desktop portal) comes later; watching other people's screens works
// everywhere.

func screenShareUnavailable() string {
	return "Sharing your screen from Linux isn't available yet — you can still watch other people's"
}
func screenWindowsSupported() bool             { return false }
func screenGDIFallback(string) bool            { return false }
func sharedWindowState(string) string          { return "" }
func listShareSources() ([]ShareSource, error) { return nil, errors.New(screenShareUnavailable()) }
func screenInput(ScreenStart, *frameTee) ([]string, int, int, screenFeed, error) {
	return nil, 0, 0, nil, errors.New(screenShareUnavailable())
}

func screenAudioTarget(string) (uint32, bool, error) {
	return 0, false, errors.New(screenShareUnavailable())
}

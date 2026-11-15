//go:build !linux

package main

import "github.com/wailsapp/wails/v2/pkg/options/linux"

// Linux-only window settings (webview_linux.go); nothing to do elsewhere.

func linuxOptions() *linux.Options { return nil }
func linuxCustomTitlebar() bool    { return false }

// childEnv: nil — programs Chriscord opens get its environment as is.
func childEnv() []string { return nil }

// readClipboardAttachments: Linux only (clipboard_linux.go); the web view
// elsewhere hands pasted pictures and files to the page itself.
func readClipboardAttachments() (ClipboardFiles, error) { return ClipboardFiles{}, nil }

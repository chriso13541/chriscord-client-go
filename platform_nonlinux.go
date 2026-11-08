//go:build !linux

package main

import "github.com/wailsapp/wails/v2/pkg/options/linux"

// Linux-only window settings (webview_linux.go); nothing to do elsewhere.

func linuxOptions() *linux.Options { return nil }
func linuxCustomTitlebar() bool    { return false }

// childEnv: nil — programs Chriscord opens get its environment as is.
func childEnv() []string { return nil }

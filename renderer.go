package main

// How the window is drawn on Linux (Settings > Performance > Drawing the
// window). WebKitGTK has several ways to draw the page, and which one is
// fast — or works at all — depends on the graphics card and its driver:
//
//	auto     the app picks (webview_linux.go: dmabufRendererRisky)
//	gpu      the graphics card, through DMA-BUF: the fastest where it works
//	gpu-cpu  the graphics card puts the window together, the CPU paints
//	         the page: for graphics drivers that are slow at painting
//	legacy   the graphics card, through WebKit's older path (no DMA-BUF)
//	cpu      no graphics card at all: slowest, but works everywhere
//
// It's read once, as the app starts (WebKitGTK reads these settings before
// the window opens), so a change applies after a restart. Kept in a file
// in the config folder rather than in the page's settings, which aren't
// readable that early. WEBKIT_* variables set by hand win over it.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
)

var rendererModes = []string{"auto", "gpu", "gpu-cpu", "legacy", "cpu"}

func rendererFile() string { return filepath.Join(chriscordConfigDir(), "renderer") }

// savedRendererMode: the mode chosen in Settings ("auto" if none).
func savedRendererMode() string {
	b, _ := os.ReadFile(rendererFile())
	m := strings.TrimSpace(string(b))
	for _, x := range rendererModes {
		if m == x {
			return m
		}
	}
	return "auto"
}

// rendererState is what the app started with (set in webview_linux.go).
var rendererState RendererInfo

// RendererInfo: for Settings > Performance.
type RendererInfo struct {
	Supported bool   `json:"supported"` // Linux only
	Mode      string `json:"mode"`      // as saved now (may differ from what's running until a restart)
	Running   string `json:"running"`   // the mode the app started with
	Active    string `json:"active"`    // how it's actually drawing, in words
	Reason    string `json:"reason"`    // why, in "auto"
	EnvSet    string `json:"envSet"`    // WEBKIT_* set by hand, which win
	GPUs      string `json:"gpus"`      // the graphics cards found, in words
}

// GetRendererInfo: how the window is drawn, and the saved choice.
func (a *App) GetRendererInfo() RendererInfo {
	if goruntime.GOOS != "linux" {
		return RendererInfo{}
	}
	r := rendererState
	r.Supported = true
	r.Mode = savedRendererMode()
	return r
}

// SetRendererMode saves how to draw the window from the next start.
func (a *App) SetRendererMode(mode string) error {
	if goruntime.GOOS != "linux" {
		return errors.New("only on Linux")
	}
	ok := false
	for _, x := range rendererModes {
		ok = ok || mode == x
	}
	if !ok {
		return errors.New("not a drawing mode")
	}
	if mode == "auto" {
		if err := os.Remove(rendererFile()); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(chriscordConfigDir(), 0700); err != nil {
		return err
	}
	return os.WriteFile(rendererFile(), []byte(mode+"\n"), 0600)
}

// RestartApp closes Chriscord and opens it again (Linux: to apply a new
// drawing mode). The new copy waits for this one to finish closing first,
// or it would find it still running and just hand over to it.
func (a *App) RestartApp() error {
	if goruntime.GOOS != "linux" {
		return errors.New("only on Linux")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := append([]string{"-c", `pid=$1; shift; i=0; while kill -0 "$pid" 2>/dev/null && [ $i -lt 100 ]; do sleep 0.1; i=$((i+1)); done; exec "$@"`,
		"chriscord-restart", strconv.Itoa(os.Getpid()), exe}, os.Args[1:]...)
	cmd := exec.Command("/bin/sh", args...)
	cmd.Env = childEnv() // as Chriscord was started: the new one decides its own settings
	if err := cmd.Start(); err != nil {
		return err
	}
	cmd.Process.Release()
	a.quitApp()
	return nil
}

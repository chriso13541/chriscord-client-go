//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
)

// systemDownloadsDir: ~/Downloads (or XDG_DOWNLOAD_DIR from user-dirs.dirs
// on Linux).
func systemDownloadsDir() string {
	home, _ := os.UserHomeDir()
	if goruntime.GOOS == "linux" {
		cfg := os.Getenv("XDG_CONFIG_HOME")
		if cfg == "" {
			cfg = filepath.Join(home, ".config")
		}
		if data, err := os.ReadFile(filepath.Join(cfg, "user-dirs.dirs")); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if v, ok := strings.CutPrefix(strings.TrimSpace(line), "XDG_DOWNLOAD_DIR="); ok {
					v = strings.Trim(v, `"`)
					v = strings.Replace(v, "$HOME", home, 1)
					if filepath.IsAbs(v) {
						return v
					}
				}
			}
		}
	}
	return filepath.Join(home, "Downloads")
}

func markDownloaded(path, server string) {}

// showInFolder shows the file in its folder (selected, on a Mac).
func showInFolder(path string) error {
	var cmd *exec.Cmd
	if goruntime.GOOS == "darwin" {
		cmd = exec.Command("open", "-R", path)
	} else {
		cmd = exec.Command("xdg-open", filepath.Dir(path))
	}
	cmd.Env = childEnv() // Linux: without Chriscord's own GTK theme (webview_linux.go)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

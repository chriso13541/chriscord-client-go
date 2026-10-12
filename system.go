package main

import (
	"os"
	"path/filepath"
)

// Settings > System.

// StartupSetting: whether "Open Chriscord when you sign in" can be offered
// here, and whether it's on.
type StartupSetting struct {
	Supported bool `json:"supported"`
	Enabled   bool `json:"enabled"`
}

func (a *App) GetOpenOnStartup() StartupSetting {
	return StartupSetting{Supported: startupSupported, Enabled: startupSupported && startupEnabled()}
}

// SetOpenOnStartup turns opening at sign-in on or off. An installed copy
// starts through the Chriscord launcher, so it's up to date when it opens;
// any other copy (a dev build) starts its own exe.
func (a *App) SetOpenOnStartup(on bool) error {
	if !startupSupported {
		return setStartup(on, "")
	}
	target := ""
	a.updates.mu.Lock()
	if a.updates.dir != "" {
		target = filepath.Join(a.updates.dir, launcherExeName)
	}
	a.updates.mu.Unlock()
	if target == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		target = exe
	}
	return setStartup(on, `"`+target+`"`) // quoted for spaces; Windows paths can't contain "
}

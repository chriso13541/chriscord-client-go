//go:build linux

package main

// "Open Chriscord when you sign in" on Linux: an XDG autostart entry,
// ~/.config/autostart/chriscord.desktop, which GNOME, KDE, Xfce, Cinnamon,
// MATE, LXQt… all run at sign-in. (Plain window managers — i3, dwm,
// Openbox without a session — don't read these on their own; dex or
// `exec chriscord` in their config does the same job there.)
//
// Turned off in the desktop's own startup settings (Hidden=true, or GNOME's
// X-GNOME-Autostart-enabled=false), it counts as off here too.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const startupSupported = true

func autostartPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "autostart", "chriscord.desktop")
}

func startupEnabled() bool {
	data, err := os.ReadFile(autostartPath())
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.ToLower(strings.TrimSpace(v))
		if (k == "Hidden" && v == "true") || (k == "X-GNOME-Autostart-enabled" && v == "false") {
			return false
		}
	}
	return true
}

// setStartup adds or removes the autostart entry. target is the program to
// start, possibly in double quotes (system.go quotes it for Windows).
func setStartup(on bool, target string) error {
	path := autostartPath()
	if !on {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	target = strings.Trim(strings.TrimSpace(target), `"`)
	if target == "" {
		return errors.New("don't know which program to start")
	}
	entry := "[Desktop Entry]\n" +
		"Type=Application\n" +
		"Name=Chriscord\n" +
		"Comment=Voice and text chat\n" +
		"Exec=" + desktopExecQuote(target) + "\n" +
		"Icon=chriscord\n" +
		"StartupWMClass=chriscord\n" +
		"Terminal=false\n" +
		"X-GNOME-Autostart-enabled=true\n"
	return writeFileAtomic(path, []byte(entry), 0644)
}

// desktopExecQuote quotes a path for a .desktop file's Exec line: inside
// double quotes, ", `, $ and \ are escaped with a backslash — and then, as
// the file format escapes backslashes once more, each \ is written twice.
func desktopExecQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\': // \\ in the Exec line, so \\\\ in the file
			b.WriteString(`\\\\`)
			continue
		case '"', '`', '$': // \" etc. in the Exec line, so \\" in the file
			b.WriteString(`\\`)
		case '%':
			b.WriteString("%%")
			continue
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

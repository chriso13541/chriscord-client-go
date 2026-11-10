//go:build linux

package main

// Chriscord's icon in the dock and the app menu on Linux.
//
// On Wayland a program can't give its window an icon: the dock (GNOME's,
// KDE's…) looks up the window's app id — "chriscord" (webview_linux.go) —
// as an installed chriscord.desktop entry, and shows that entry's icon.
// Without one it shows a generic gear. So at each start Chriscord makes
// sure there is one, for this user only:
//
//	~/.local/share/applications/chriscord.desktop
//	~/.local/share/icons/hicolor/{64x64,128x128,256x256,512x512}/apps/chriscord.png
//
// Exec points at this program — or at the launcher, when it was installed
// by one (update.go), so the menu entry updates first. Files are only
// rewritten when they'd change (Chriscord moved, a new icon). It also puts
// Chriscord in the app menu / Activities search.
//
// The very first start may still show the gear until the desktop notices
// the new entry; from the next start on, the icon is right.
// CHRISCORD_NO_DESKTOP_ENTRY=1 skips all this (packaged installs that ship
// their own entry).

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"strings"
)

func init() {
	go func() {
		if err := installDesktopEntry(); err != nil {
			log.Printf("desktop: couldn't add the app menu entry: %v", err)
		}
	}()
}

func xdgDataHome() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share")
}

func installDesktopEntry() error {
	if os.Getenv("CHRISCORD_NO_DESKTOP_ENTRY") == "1" {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	// Only a real build (not `wails dev`'s, or a copy renamed for a test).
	if filepath.Base(exe) != "chriscord" {
		return nil
	}
	target := exe
	if dir := filepath.Dir(exe); strings.EqualFold(filepath.Base(dir), "app") {
		if l := filepath.Join(filepath.Dir(dir), launcherExeName); fileExists(l) {
			target = l
		}
	}

	data := xdgDataHome()
	entry := "[Desktop Entry]\n" +
		"Type=Application\n" +
		"Name=Chriscord\n" +
		"GenericName=Voice and Text Chat\n" +
		"Comment=Self-hosted voice, video and text chat\n" +
		"Exec=" + desktopExecQuote(target) + "\n" +
		"TryExec=" + strings.ReplaceAll(target, `\`, `\\`) + "\n" +
		"Icon=chriscord\n" +
		"Terminal=false\n" +
		"Categories=Network;Chat;InstantMessaging;AudioVideo;\n" +
		"Keywords=chat;voice;discord;\n" +
		"StartupWMClass=chriscord\n" +
		"StartupNotify=true\n"
	entryPath := filepath.Join(data, "applications", "chriscord.desktop")
	iconPath := func(size int) string {
		return filepath.Join(data, "icons", "hicolor", fmt.Sprintf("%dx%d", size, size), "apps", "chriscord.png")
	}
	// Already there, from this program: nothing to do (the usual case —
	// no need to make the icons again at every start).
	stamp := filepath.Join(chriscordConfigDir(), "desktop-icon.id") // which icon was installed
	sum := sha256.Sum256(linuxAppIcon)
	iconID := hex.EncodeToString(sum[:8])
	if old, err := os.ReadFile(entryPath); err == nil && string(old) == entry {
		if id, err := os.ReadFile(stamp); err == nil && string(id) == iconID && fileExists(iconPath(512)) {
			return nil
		}
	}
	// The icons first, so the entry never points at a missing one.
	src, err := png.Decode(bytes.NewReader(linuxAppIcon))
	if err != nil {
		return fmt.Errorf("the built-in icon: %w", err)
	}
	for _, size := range []int{64, 128, 256, 512} {
		var buf bytes.Buffer
		if err := png.Encode(&buf, scaleIcon(src, size)); err != nil {
			return err
		}
		if err := writeIfChanged(iconPath(size), buf.Bytes(), 0644); err != nil {
			return err
		}
	}
	writeIfChanged(stamp, []byte(iconID), 0644)
	return writeIfChanged(entryPath, []byte(entry), 0644)
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func writeIfChanged(path string, data []byte, perm os.FileMode) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return writeFileAtomic(path, data, perm)
}

// scaleIcon: the icon at size x size, each pixel the average of the block
// it covers (with transparency weighted, so edges don't go dark).
func scaleIcon(src image.Image, size int) *image.NRGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		y0, y1 := b.Min.Y+y*sh/size, b.Min.Y+(y+1)*sh/size
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < size; x++ {
			x0, x1 := b.Min.X+x*sw/size, b.Min.X+(x+1)*sw/size
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, a, n uint64
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					pr, pg, pb, pa := src.At(xx, yy).RGBA() // premultiplied, 16-bit
					r, g, bl, a = r+uint64(pr), g+uint64(pg), bl+uint64(pb), a+uint64(pa)
					n++
				}
			}
			o := dst.PixOffset(x, y)
			if a == 0 {
				continue
			}
			dst.Pix[o] = uint8(r * 255 / a)
			dst.Pix[o+1] = uint8(g * 255 / a)
			dst.Pix[o+2] = uint8(bl * 255 / a)
			dst.Pix[o+3] = uint8(a / n >> 8)
		}
	}
	return dst
}

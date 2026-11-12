//go:build linux

package main

import (
	_ "embed"
	"log"
	"os"
	"strings"

	"github.com/wailsapp/wails/v2/pkg/options/linux"
)

// The window on Linux: a GTK window holding a WebKitGTK web view.
//
// Rendering. WebKitGTK's DMA-BUF renderer (2.42 and later) is its fast,
// graphics-card path — without it a weak computer draws everything on the
// CPU and the app crawls (an AMD A4 laptop: 100% CPU, smooth with it on).
// But on some systems it loses the page (the window turns plain grey):
// NVIDIA's own driver, and virtual machines' graphics. So it's left on,
// except there (dmabufRendererRisky). Set WEBKIT_DISABLE_DMABUF_RENDERER
// yourself (1 or 0) to decide. If the window still goes grey,
// WEBKIT_DISABLE_COMPOSITING_MODE=1 (drawing without the graphics card) is
// the next thing to try.
//
// Looks. Chriscord keeps its own look rather than the desktop's:
//   - The title bar is the app's own (#titlebar in index.html), as on
//     Windows: the window has no frame from the window manager or GTK.
//     CHRISCORD_SYSTEM_TITLEBAR=1 brings the desktop's bar back, for a
//     window manager that doesn't get along with frameless windows.
//   - The few things GTK still draws itself — <select> drop-down lists,
//     the right-click menu, file pickers, scroll bars WebKit doesn't style —
//     use GTK's own dark Adwaita, not whatever theme the desktop has (a
//     light theme, a bright custom one), so they match the app everywhere.
//     That's GTK_THEME, set for this process only: programs Chriscord opens
//     (xdg-open, the file manager) get the environment as it was
//     (childEnv). CHRISCORD_GTK_THEME picks another GTK theme, or "system"
//     leaves the desktop's.

// originalEnv: the environment as Chriscord was started, before any of the
// settings above — what programs it opens should get.
var originalEnv []string

//go:embed build/appicon.png
var linuxAppIcon []byte

func init() {
	originalEnv = os.Environ()

	if _, set := os.LookupEnv("WEBKIT_DISABLE_DMABUF_RENDERER"); !set {
		if why := dmabufRendererRisky(); why != "" {
			os.Setenv("WEBKIT_DISABLE_DMABUF_RENDERER", "1")
			log.Printf("webview: graphics-card rendering off (%s) — WEBKIT_DISABLE_DMABUF_RENDERER=0 turns it on", why)
		}
	}
	if os.Getenv("WEBKIT_DISABLE_COMPOSITING_MODE") != "" {
		log.Printf("webview: WEBKIT_DISABLE_COMPOSITING_MODE is set — drawing without the graphics card")
	}

	// H.264 is decoded by the app's FFmpeg, not the web view (videodec.go):
	// unless CHRISCORD_VIDEO_DECODE=webview.
	nativeH264 = !strings.EqualFold(strings.TrimSpace(os.Getenv("CHRISCORD_VIDEO_DECODE")), "webview")
	// GStreamer's OpenH264 decoder (Fedora's only H.264 decoder by default)
	// locks the window up when the web view uses it. Switched off for this
	// app, an H.264 video the distro can't otherwise play just counts as
	// unplayable, and is converted instead (localmedia.go).
	if rank := os.Getenv("GST_PLUGIN_FEATURE_RANK"); !strings.Contains(rank, "openh264dec") {
		if rank != "" {
			rank += ","
		}
		os.Setenv("GST_PLUGIN_FEATURE_RANK", rank+"openh264dec:NONE")
	}

	switch theme := strings.TrimSpace(os.Getenv("CHRISCORD_GTK_THEME")); {
	case strings.EqualFold(theme, "system"):
		// leave the desktop's theme
	case theme != "":
		os.Setenv("GTK_THEME", theme)
	default:
		os.Setenv("GTK_THEME", "Adwaita:dark")
	}
}

// dmabufRendererRisky: why WebKitGTK's DMA-BUF renderer shouldn't be used
// here ("" if it's fine): NVIDIA's own driver, or a virtual machine.
func dmabufRendererRisky() string {
	if _, err := os.Stat("/proc/driver/nvidia/version"); err == nil {
		return "NVIDIA's driver"
	}
	for _, f := range []string{"/sys/class/dmi/id/sys_vendor", "/sys/class/dmi/id/product_name"} {
		b, _ := os.ReadFile(f)
		v := strings.ToLower(string(b))
		for _, vm := range []string{"qemu", "vmware", "virtualbox", "innotek", "kvm", "bochs", "parallels", "virtual machine", "xen"} { // Hyper-V: "Virtual Machine"
			if strings.Contains(v, vm) {
				return "a virtual machine"
			}
		}
	}
	return ""
}

// childEnv: the environment for a program Chriscord starts for you
// (opening a file or a folder) — the one Chriscord itself was started with.
func childEnv() []string { return originalEnv }

// linuxCustomTitlebar: the app draws its own title bar (no frame from the
// window manager), unless CHRISCORD_SYSTEM_TITLEBAR=1.
func linuxCustomTitlebar() bool {
	v := strings.TrimSpace(os.Getenv("CHRISCORD_SYSTEM_TITLEBAR"))
	return v == "" || v == "0"
}

// linuxOptions: the window's icon (the taskbar, Alt+Tab) and its program
// name — WM_CLASS on X11, the app id on Wayland — which matches
// the chriscord.desktop entry desktop_linux.go installs, so docks show its
// icon. The program name also names WebKit's data folder
// (~/.local/share/chriscord: the page's saved settings), so it stays the
// same whatever the executable is called.
func linuxOptions() *linux.Options {
	return &linux.Options{
		Icon:        linuxAppIcon,
		ProgramName: "chriscord",
	}
}

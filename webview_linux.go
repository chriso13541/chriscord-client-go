//go:build linux

package main

import (
	_ "embed"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/wailsapp/wails/v2/pkg/options/linux"
)

// The window on Linux: a GTK window holding a WebKitGTK web view.
//
// Rendering. WebKitGTK's DMA-BUF renderer (2.42 and later) is its fast,
// graphics-card path — without it a weak computer draws everything on the
// CPU and the app crawls (an AMD A4 laptop: 100% CPU, smooth with it on).
// But on some systems it loses the page (the window turns plain grey):
// NVIDIA's own driver drawing the window, and virtual machines' graphics.
// Settings > Performance chooses (renderer.go); in "auto" it's on except
// there (dmabufRendererRisky). WEBKIT_* variables set by hand win.
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

	setupRenderer()

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
	// GStreamer's Vulkan elements aren't needed (nothing here plays through
	// Vulkan), and starting Vulkan on an old chip — Intel Haswell ("Haswell
	// Vulkan support is incomplete"), Ivy Bridge — can be slow or worse.
	// Ranked out so nothing picks them when a video or camera list is set up.
	if rank := os.Getenv("GST_PLUGIN_FEATURE_RANK"); !strings.Contains(rank, "vulkan") {
		os.Setenv("GST_PLUGIN_FEATURE_RANK", rank+",vulkandeviceprovider:NONE,vulkanh264dec:NONE,vulkanh265dec:NONE,vulkansink:NONE,vulkanupload:NONE,vulkancolorconvert:NONE")
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

// setupRenderer sets WebKitGTK's drawing variables from the mode chosen in
// Settings (renderer.go), before the web view starts.
func setupRenderer() {
	mode := savedRendererMode()
	rendererState.Running = mode
	rendererState.GPUs = describeGPUs()
	set := map[string]string{} // variable → value
	switch mode {
	case "gpu":
		set["WEBKIT_DISABLE_DMABUF_RENDERER"] = "0"
	case "gpu-cpu":
		set["WEBKIT_DISABLE_DMABUF_RENDERER"] = "0"
		set["WEBKIT_SKIA_ENABLE_CPU_RENDERING"] = "1"
	case "legacy":
		set["WEBKIT_DISABLE_DMABUF_RENDERER"] = "1"
	case "cpu":
		set["WEBKIT_DISABLE_DMABUF_RENDERER"] = "1"
		set["WEBKIT_DISABLE_COMPOSITING_MODE"] = "1"
	default: // auto
		if why := dmabufRendererRisky(); why != "" {
			set["WEBKIT_DISABLE_DMABUF_RENDERER"] = "1"
			rendererState.Reason = why
		} else {
			rendererState.Reason = "nothing known to have trouble with it"
		}
	}
	var byHand []string
	for _, k := range []string{"WEBKIT_DISABLE_DMABUF_RENDERER", "WEBKIT_DISABLE_COMPOSITING_MODE", "WEBKIT_SKIA_ENABLE_CPU_RENDERING"} {
		if v, ok := os.LookupEnv(k); ok {
			byHand = append(byHand, k+"="+v)
		} else if v, ok := set[k]; ok {
			os.Setenv(k, v)
		}
	}
	rendererState.EnvSet = strings.Join(byHand, " ")

	on := func(k string) bool { v := os.Getenv(k); return v != "" && v != "0" }
	switch {
	case on("WEBKIT_DISABLE_COMPOSITING_MODE"):
		rendererState.Active = "without the graphics card (CPU only)"
	case on("WEBKIT_DISABLE_DMABUF_RENDERER"):
		rendererState.Active = "with the graphics card, WebKit's older way (no DMA-BUF)"
	case on("WEBKIT_SKIA_ENABLE_CPU_RENDERING"):
		rendererState.Active = "with the graphics card (DMA-BUF), the page painted by the CPU"
	default:
		rendererState.Active = "with the graphics card (DMA-BUF)"
	}
	msg := "webview: drawing " + rendererState.Active + " — mode " + mode
	if mode == "auto" {
		msg += " (" + rendererState.Reason + ")"
	}
	if len(byHand) > 0 {
		msg += "; set by hand: " + rendererState.EnvSet
	}
	log.Printf("%s. Graphics: %s. Settings > Performance changes it.", msg, rendererState.GPUs)
}

// gpuVendors: the PCI vendor ids of the graphics cards the kernel drives
// (0x8086 Intel, 0x1002 AMD, 0x10de NVIDIA, …), one per card.
func gpuVendors() []string {
	ents, _ := os.ReadDir("/sys/class/drm")
	var out []string
	for _, e := range ents {
		n := e.Name()
		if !strings.HasPrefix(n, "card") || strings.Contains(n, "-") {
			continue // a connector (card0-eDP-1), not a card
		}
		b, err := os.ReadFile(filepath.Join("/sys/class/drm", n, "device", "vendor"))
		if err == nil {
			out = append(out, strings.ToLower(strings.TrimSpace(string(b))))
		}
	}
	return out
}

func describeGPUs() string {
	names := map[string]string{"0x8086": "Intel", "0x1002": "AMD", "0x10de": "NVIDIA", "0x1af4": "virtual (virtio)", "0x15ad": "VMware", "0x80ee": "VirtualBox", "0x1234": "QEMU", "0x1b36": "QEMU"}
	var parts []string
	for _, v := range gpuVendors() {
		n := names[v]
		if n == "" {
			n = v
		}
		parts = append(parts, n)
	}
	if _, err := os.Stat("/proc/driver/nvidia/version"); err == nil {
		parts = append(parts, "NVIDIA's own driver loaded")
	}
	if len(parts) == 0 {
		return "none found"
	}
	return strings.Join(parts, ", ")
}

// dmabufRendererRisky: why WebKitGTK's DMA-BUF renderer shouldn't be used
// here ("" if it's fine): NVIDIA's own driver drawing the window, or a
// virtual machine.
//
// A laptop with both Intel (or AMD) graphics and an NVIDIA card (Optimus,
// like a ThinkPad W540) draws the desktop — and so this window — on the
// Intel chip unless told otherwise; the NVIDIA card only runs programs
// sent to it. There the DMA-BUF renderer is fine (and without it the
// window is drawn slowly), so NVIDIA's driver only counts when there's no
// other graphics, or when this app was sent to the NVIDIA card.
func dmabufRendererRisky() string {
	if _, err := os.Stat("/proc/driver/nvidia/version"); err == nil {
		other := false
		for _, v := range gpuVendors() {
			other = other || v == "0x8086" || v == "0x1002"
		}
		offload := os.Getenv("__NV_PRIME_RENDER_OFFLOAD") == "1" || strings.EqualFold(os.Getenv("__GLX_VENDOR_LIBRARY_NAME"), "nvidia")
		switch {
		case !other:
			return "NVIDIA's driver"
		case offload:
			return "NVIDIA's driver, and this app was started on the NVIDIA card"
		}
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

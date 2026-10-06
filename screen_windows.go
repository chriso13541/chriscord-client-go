//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Screen sharing on Windows: monitors and windows are listed here with the
// Win32 API (and a small GDI snapshot of each for the picker); the capture
// itself is FFmpeg's gfxcapture (Windows Graphics Capture).

// (user32 and kernel32 are declared in idle_windows.go.)
var (
	gdi32  = syscall.NewLazyDLL("gdi32.dll")
	dwmapi = syscall.NewLazyDLL("dwmapi.dll")

	pEnumDisplayMonitors     = user32.NewProc("EnumDisplayMonitors")
	pGetMonitorInfoW         = user32.NewProc("GetMonitorInfoW")
	pEnumDisplaySettingsW    = user32.NewProc("EnumDisplaySettingsW")
	pEnumWindows             = user32.NewProc("EnumWindows")
	pIsWindow                = user32.NewProc("IsWindow")
	pIsWindowVisible         = user32.NewProc("IsWindowVisible")
	pIsIconic                = user32.NewProc("IsIconic")
	pGetWindowTextW          = user32.NewProc("GetWindowTextW")
	pGetWindowTextLengthW    = user32.NewProc("GetWindowTextLengthW")
	pGetClassNameW           = user32.NewProc("GetClassNameW")
	pGetWindow               = user32.NewProc("GetWindow")
	pGetWindowLongW          = user32.NewProc("GetWindowLongW")
	pGetWindowRect           = user32.NewProc("GetWindowRect")
	pGetWindowThreadProcId   = user32.NewProc("GetWindowThreadProcessId")
	pGetDC                   = user32.NewProc("GetDC")
	pReleaseDC               = user32.NewProc("ReleaseDC")
	pPrintWindow             = user32.NewProc("PrintWindow")
	pCreateCompatibleDC      = gdi32.NewProc("CreateCompatibleDC")
	pCreateCompatibleBitmap  = gdi32.NewProc("CreateCompatibleBitmap")
	pSelectObject            = gdi32.NewProc("SelectObject")
	pDeleteObject            = gdi32.NewProc("DeleteObject")
	pDeleteDC                = gdi32.NewProc("DeleteDC")
	pStretchBlt              = gdi32.NewProc("StretchBlt")
	pSetStretchBltMode       = gdi32.NewProc("SetStretchBltMode")
	pGetDIBits               = gdi32.NewProc("GetDIBits")
	pDwmGetWindowAttribute   = dwmapi.NewProc("DwmGetWindowAttribute")
	pOpenProcess             = kernel32.NewProc("OpenProcess")
	pQueryFullProcessImageNW = kernel32.NewProc("QueryFullProcessImageNameW")
	pCloseHandle             = kernel32.NewProc("CloseHandle")
)

type winRect struct{ Left, Top, Right, Bottom int32 }

type monitorInfoEx struct {
	CbSize    uint32
	RcMonitor winRect
	RcWork    winRect
	DwFlags   uint32
	SzDevice  [32]uint16
}

type bitmapInfoHeader struct {
	BiSize          uint32
	BiWidth         int32
	BiHeight        int32
	BiPlanes        uint16
	BiBitCount      uint16
	BiCompression   uint32
	BiSizeImage     uint32
	BiXPelsPerMeter int32
	BiYPelsPerMeter int32
	BiClrUsed       uint32
	BiClrImportant  uint32
}

type monitorEntry struct {
	handle  uintptr
	rect    winRect // desktop coordinates (as this app sees them)
	w, h    int     // real pixels
	primary bool
}

var (
	enumMu       sync.Mutex
	enumMonitors []monitorEntry
	enumWindows  []uintptr

	monitorCallback = syscall.NewCallback(func(hmon, hdc, rect, lparam uintptr) uintptr {
		var mi monitorInfoEx
		mi.CbSize = uint32(unsafe.Sizeof(mi))
		if r, _, _ := pGetMonitorInfoW.Call(hmon, uintptr(unsafe.Pointer(&mi))); r != 0 {
			m := monitorEntry{handle: hmon, rect: mi.RcMonitor, primary: mi.DwFlags&1 != 0}
			m.w, m.h = int(mi.RcMonitor.Right-mi.RcMonitor.Left), int(mi.RcMonitor.Bottom-mi.RcMonitor.Top)
			// Its real resolution (the rectangle above can be scaled for DPI).
			var dm [220]byte
			*(*uint16)(unsafe.Pointer(&dm[68])) = 220 // dmSize
			if r, _, _ := pEnumDisplaySettingsW.Call(uintptr(unsafe.Pointer(&mi.SzDevice[0])), ^uintptr(0) /* ENUM_CURRENT_SETTINGS */, uintptr(unsafe.Pointer(&dm[0]))); r != 0 {
				pw, ph := *(*uint32)(unsafe.Pointer(&dm[172])), *(*uint32)(unsafe.Pointer(&dm[176]))
				if pw > 0 && ph > 0 {
					m.w, m.h = int(pw), int(ph)
				}
			}
			enumMonitors = append(enumMonitors, m)
		}
		return 1
	})
	windowCallback = syscall.NewCallback(func(hwnd, lparam uintptr) uintptr {
		enumWindows = append(enumWindows, hwnd)
		return 1
	})
)

// monitors, left to right (that's how they're numbered: Screen 1, 2, …).
func listMonitors() []monitorEntry {
	enumMu.Lock()
	defer enumMu.Unlock()
	enumMonitors = nil
	pEnumDisplayMonitors.Call(0, 0, monitorCallback, 0)
	out := append([]monitorEntry(nil), enumMonitors...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].rect.Left != out[j].rect.Left {
			return out[i].rect.Left < out[j].rect.Left
		}
		return out[i].rect.Top < out[j].rect.Top
	})
	return out
}

func topLevelWindows() []uintptr {
	enumMu.Lock()
	defer enumMu.Unlock()
	enumWindows = nil
	pEnumWindows.Call(windowCallback, 0)
	return append([]uintptr(nil), enumWindows...)
}

func windowText(hwnd uintptr) string {
	n, _, _ := pGetWindowTextLengthW.Call(hwnd)
	if n == 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	pGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), n+1)
	return syscall.UTF16ToString(buf)
}

func windowClass(hwnd uintptr) string {
	buf := make([]uint16, 256)
	pGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), 256)
	return syscall.UTF16ToString(buf)
}

func windowProcess(hwnd uintptr) (pid uint32, exe string) {
	pGetWindowThreadProcId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	h, _, _ := pOpenProcess.Call(0x1000 /* PROCESS_QUERY_LIMITED_INFORMATION */, 0, uintptr(pid))
	if h == 0 {
		return pid, ""
	}
	defer pCloseHandle.Call(h)
	buf := make([]uint16, 1024)
	n := uint32(len(buf))
	if r, _, _ := pQueryFullProcessImageNW.Call(h, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n))); r == 0 {
		return pid, ""
	}
	return pid, filepath.Base(syscall.UTF16ToString(buf[:n]))
}

// windowSize: the window's size in real pixels (DWM's frame bounds), else
// its rectangle.
func windowSize(hwnd uintptr) (int, int, winRect) {
	var r winRect
	pGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	var fb winRect
	if hr, _, _ := pDwmGetWindowAttribute.Call(hwnd, 9 /* DWMWA_EXTENDED_FRAME_BOUNDS */, uintptr(unsafe.Pointer(&fb)), unsafe.Sizeof(fb)); hr == 0 && fb.Right > fb.Left {
		return int(fb.Right - fb.Left), int(fb.Bottom - fb.Top), r
	}
	return int(r.Right - r.Left), int(r.Bottom - r.Top), r
}

// shareableWindow: the windows Alt+Tab would show — visible, titled,
// not tool windows or hidden ("cloaked") ones, not this app's own.
func shareableWindow(hwnd uintptr, self uint32) bool {
	if v, _, _ := pIsWindowVisible.Call(hwnd); v == 0 {
		return false
	}
	if owner, _, _ := pGetWindow.Call(hwnd, 4 /* GW_OWNER */); owner != 0 {
		return false
	}
	gwlExStyle := -20
	ex, _, _ := pGetWindowLongW.Call(hwnd, uintptr(gwlExStyle))
	if ex&0x80 != 0 { // WS_EX_TOOLWINDOW
		return false
	}
	var cloaked uint32
	if hr, _, _ := pDwmGetWindowAttribute.Call(hwnd, 14 /* DWMWA_CLOAKED */, uintptr(unsafe.Pointer(&cloaked)), 4); hr == 0 && cloaked != 0 {
		return false
	}
	switch windowClass(hwnd) {
	case "Progman", "WorkerW", "Shell_TrayWnd", "Shell_SecondaryTrayWnd":
		return false
	}
	if strings.TrimSpace(windowText(hwnd)) == "" {
		return false
	}
	var pid uint32
	pGetWindowThreadProcId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	return pid != self
}

// snapshot: a thumbnail (at most tw wide) of part of the desktop, or of a
// window (hwnd != 0, drawn with PrintWindow so it works when covered).
func snapshot(hwnd uintptr, src winRect, tw int) image.Image {
	sw, sh := int(src.Right-src.Left), int(src.Bottom-src.Top)
	if sw <= 0 || sh <= 0 {
		return nil
	}
	th := tw * sh / sw
	if th <= 0 {
		return nil
	}
	screenDC, _, _ := pGetDC.Call(0)
	if screenDC == 0 {
		return nil
	}
	defer pReleaseDC.Call(0, screenDC)
	thumbDC, _, _ := pCreateCompatibleDC.Call(screenDC)
	defer pDeleteDC.Call(thumbDC)
	thumbBmp, _, _ := pCreateCompatibleBitmap.Call(screenDC, uintptr(tw), uintptr(th))
	defer pDeleteObject.Call(thumbBmp)
	old, _, _ := pSelectObject.Call(thumbDC, thumbBmp)
	pSetStretchBltMode.Call(thumbDC, 4) // HALFTONE: a smooth scale-down
	const srccopy, captureblt = 0x00CC0020, 0x40000000
	ok := false
	if hwnd == 0 {
		r, _, _ := pStretchBlt.Call(thumbDC, 0, 0, uintptr(tw), uintptr(th), screenDC,
			uintptr(src.Left), uintptr(src.Top), uintptr(sw), uintptr(sh), srccopy|captureblt)
		ok = r != 0
	} else {
		// The whole window at full size first, then scaled down.
		fullDC, _, _ := pCreateCompatibleDC.Call(screenDC)
		fullBmp, _, _ := pCreateCompatibleBitmap.Call(screenDC, uintptr(sw), uintptr(sh))
		fold, _, _ := pSelectObject.Call(fullDC, fullBmp)
		if r, _, _ := pPrintWindow.Call(hwnd, fullDC, 2 /* PW_RENDERFULLCONTENT */); r != 0 {
			r, _, _ := pStretchBlt.Call(thumbDC, 0, 0, uintptr(tw), uintptr(th), fullDC, 0, 0, uintptr(sw), uintptr(sh), srccopy)
			ok = r != 0
		}
		pSelectObject.Call(fullDC, fold)
		pDeleteObject.Call(fullBmp)
		pDeleteDC.Call(fullDC)
	}
	pSelectObject.Call(thumbDC, old)
	if !ok {
		return nil
	}
	bi := bitmapInfoHeader{BiWidth: int32(tw), BiHeight: -int32(th), BiPlanes: 1, BiBitCount: 32}
	bi.BiSize = uint32(unsafe.Sizeof(bi))
	pix := make([]byte, tw*th*4)
	if r, _, _ := pGetDIBits.Call(thumbDC, thumbBmp, 0, uintptr(th), uintptr(unsafe.Pointer(&pix[0])), uintptr(unsafe.Pointer(&bi)), 0); r == 0 {
		return nil
	}
	img := image.NewRGBA(image.Rect(0, 0, tw, th))
	for i := 0; i < len(pix); i += 4 { // BGRA → RGBA
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = pix[i+2], pix[i+1], pix[i], 255
	}
	return img
}

func thumbURL(img image.Image) string {
	if img == nil {
		return ""
	}
	var buf bytes.Buffer
	if jpeg.Encode(&buf, img, &jpeg.Options{Quality: 72}) != nil {
		return ""
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func listShareSources() ([]ShareSource, error) {
	var out []ShareSource
	type job struct {
		i    int
		hwnd uintptr
		r    winRect
	}
	var jobs []job
	for i, m := range listMonitors() {
		out = append(out, ShareSource{
			ID: fmt.Sprintf("m:%d", m.handle), Kind: "screen", Name: fmt.Sprintf("Screen %d", i+1),
			W: m.w, H: m.h, Primary: m.primary, Index: i,
		})
		jobs = append(jobs, job{len(out) - 1, 0, m.rect})
	}
	if screenWindowsSupported() {
		self := uint32(os.Getpid())
		for _, hwnd := range topLevelWindows() {
			if !shareableWindow(hwnd, self) {
				continue
			}
			w, h, r := windowSize(hwnd)
			if w < 40 || h < 40 {
				continue
			}
			_, exe := windowProcess(hwnd)
			s := ShareSource{ID: fmt.Sprintf("w:%d", hwnd), Kind: "window", Name: windowText(hwnd), App: exe, W: w, H: h}
			if m, _, _ := pIsIconic.Call(hwnd); m != 0 {
				s.Note = "Minimised"
			} else {
				jobs = append(jobs, job{len(out), hwnd, r})
			}
			out = append(out, s)
		}
	}
	// Pictures, a few at a time; one that takes too long (a frozen
	// program) is simply left without one.
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, 4)
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			done := make(chan string, 1)
			go func() { done <- thumbURL(snapshot(j.hwnd, j.r, 320)) }()
			select {
			case t := <-done:
				mu.Lock()
				out[j.i].Thumb = t
				mu.Unlock()
			case <-time.After(1500 * time.Millisecond):
			}
		}(j)
	}
	wg.Wait()
	return out, nil
}

func screenWindowsSupported() bool { return ffmpegFilters()["gfxcapture"] }

func screenShareUnavailable() string {
	if findFFmpeg() == "" {
		return "FFmpeg isn't available"
	}
	if !strings.Contains(ffmpegInputs()["flv"], "E") {
		return "this FFmpeg was built without the FLV muxer — rebuild it with ffmpeg/build-ffmpeg.sh"
	}
	f := ffmpegFilters()
	if !f["gfxcapture"] && !f["ddagrab"] {
		return "this FFmpeg was built without screen capture — rebuild it with ffmpeg/build-ffmpeg.sh"
	}
	if !f["hwdownload"] {
		return "this FFmpeg is missing the hwdownload filter — rebuild it with ffmpeg/build-ffmpeg.sh"
	}
	return ""
}

// screenInputArgs: the FFmpeg input for a share — Windows Graphics Capture
// of a monitor or window, scaled on the graphics card to the size to send,
// then brought into memory for the encoder.
func screenInputArgs(o ScreenStart) ([]string, int, int, error) {
	kind, handle, err := parseShareID(o.ID)
	if err != nil {
		return nil, 0, 0, err
	}
	f := ffmpegFilters()
	// Windows Graphics Capture draws a yellow border round what's being
	// captured unless told not to — and some Windows versions insist on it.
	border := 1
	if o.HideBorder {
		border = 0
	}
	log.Printf("screen: Windows build %d; hide border: %v", windowsBuild(), o.HideBorder)
	if kind == "screen" {
		for i, m := range listMonitors() {
			if uint64(m.handle) != handle {
				continue
			}
			w, h := shareSize(m.w, m.h, o.Height)
			// Desktop Duplication never draws a border: used for a whole
			// screen when the border should be hidden (or WGC is missing).
			if f["ddagrab"] && (o.HideBorder || !f["gfxcapture"]) {
				if adapter, output, ok := ddaFind(handle, m.w, m.h); ok {
					if adapter == 0 {
						// The default graphics adapter: ddagrab makes its own
						// device there. No device set up up front means
						// NVENC isn't handed it either, so no CUDA device is
						// needed to steer it — and on a laptop, setting CUDA
						// up first moves FFmpeg onto the NVIDIA card, where
						// the Intel-driven screen can't be duplicated
						// ("Selected output not supported").
						return []string{"-filter_complex", fmt.Sprintf(
							"ddagrab=output_idx=%d:framerate=%d:draw_mouse=1,hwdownload,format=bgra,scale=%d:%d",
							output, o.FPS, w, h)}, w, h, nil
					}
					return []string{"-init_hw_device", fmt.Sprintf("d3d11va=dda:%d", adapter), "-filter_hw_device", "dda",
						"-filter_complex", fmt.Sprintf(
							"ddagrab=output_idx=%d:framerate=%d:draw_mouse=1,hwdownload,format=bgra,scale=%d:%d",
							output, o.FPS, w, h)}, w, h, nil
				}
				if !f["gfxcapture"] {
					// Couldn't map it: outputs counted left to right, which
					// matches most single-graphics-card setups.
					return []string{"-filter_complex", fmt.Sprintf(
						"ddagrab=output_idx=%d:framerate=%d:draw_mouse=1,hwdownload,format=bgra,scale=%d:%d",
						i, o.FPS, w, h)}, w, h, nil
				}
				if o.HideBorder {
					return nil, 0, 0, errors.New("Desktop Duplication couldn’t capture this screen (the tries are in the log)")
				}
			}
			return []string{"-filter_complex", fmt.Sprintf(
				"gfxcapture=hmonitor=%d:max_framerate=%d:capture_cursor=1:display_border=%d:width=%d:height=%d:resize_mode=scale_aspect,hwdownload,format=bgra",
				handle, o.FPS, border, w, h)}, w, h, nil
		}
		return nil, 0, 0, errors.New("that screen isn't connected any more")
	}
	if !f["gfxcapture"] {
		return nil, 0, 0, errors.New("sharing a single window needs an FFmpeg built with gfxcapture — rebuild it under MSYS2 with ffmpeg/build-ffmpeg.sh")
	}
	hwnd := uintptr(handle)
	if ok, _, _ := pIsWindow.Call(hwnd); ok == 0 {
		return nil, 0, 0, errors.New("that window has been closed")
	}
	if m, _, _ := pIsIconic.Call(hwnd); m != 0 {
		return nil, 0, 0, errors.New("that window is minimised — restore it, then share it")
	}
	if o.HideBorder && !isWindows11() {
		log.Printf("screen: this version of Windows always draws the yellow border round a shared window (only Windows 11 lets it be hidden)")
	}
	sw, sh, _ := windowSize(hwnd)
	w, h := shareSize(sw, sh, o.Height)
	// (On Windows 10 the border can't be hidden for a single window: the
	// older GDI copy that avoids it shows most modern apps as black.)
	return []string{"-filter_complex", fmt.Sprintf(
		"gfxcapture=hwnd=%d:max_framerate=%d:capture_cursor=1:display_border=%d:width=%d:height=%d:resize_mode=scale_aspect,hwdownload,format=bgra",
		handle, o.FPS, border, w, h)}, w, h, nil
}

// screenAudioTarget: whose sound goes with this share — for a whole screen,
// everything except this app (exclude this process); for a window, only
// the program that owns it.
func screenAudioTarget(id string) (pid uint32, exclude bool, err error) {
	kind, handle, err := parseShareID(id)
	if err != nil {
		return 0, false, err
	}
	if kind == "screen" {
		return uint32(os.Getpid()), true, nil
	}
	p, _ := windowProcess(uintptr(handle))
	if p == 0 {
		return 0, false, errors.New("couldn't tell which program owns that window")
	}
	return p, false, nil
}

// isWindows11: build 22000 or later — where Windows Graphics Capture can
// leave out its yellow border.
func isWindows11() bool { return windowsBuild() >= 22000 }

// windowsBuild: Windows' build number (19045 is Windows 10 22H2; 22000 and
// up are Windows 11).
func windowsBuild() uint32 {
	type osVersionInfo struct {
		size, major, minor, build, platform uint32
		csd                                 [128]uint16
	}
	v := osVersionInfo{}
	v.size = uint32(unsafe.Sizeof(v))
	r, _, _ := syscall.NewLazyDLL("ntdll.dll").NewProc("RtlGetVersion").Call(uintptr(unsafe.Pointer(&v)))
	if r != 0 || v.major < 10 {
		return 0
	}
	return v.build
}

// Where Desktop Duplication finds each monitor, as FFmpeg sees it: found by
// trying (ddaFind), then remembered.
var (
	ddaMu    sync.Mutex
	ddaCache = map[uint64][2]int{}
	reDDAOut = regexp.MustCompile(`Opened dxgi output (\d+) with dimensions (\d+)x(\d+)`)
)

// ddaFind: which graphics adapter and output FFmpeg's ddagrab must use to
// capture this monitor. It can't simply be worked out here: Windows numbers
// the adapters, and even says which one a laptop's screens belong to,
// differently for each program (by its graphics preference). So FFmpeg is
// asked directly — each adapter's outputs are opened in turn, briefly, and
// the one with this monitor's size is it (where this app's own view of it,
// dxgiOutputFor, is tried first and breaks any tie).
func ddaFind(hmonitor uint64, w, h int) (adapter, output int, ok bool) {
	ddaMu.Lock()
	defer ddaMu.Unlock()
	ffmpegDPIAware()
	if v, ok := ddaCache[hmonitor]; ok {
		return v[0], v[1], true
	}
	probe := func(a, o int) (opened bool, more bool, adapterExists bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, findFFmpeg(), "-hide_banner", "-nostdin", "-v", "verbose",
			"-init_hw_device", fmt.Sprintf("d3d11va=p:%d", a), "-filter_hw_device", "p",
			"-filter_complex", fmt.Sprintf("ddagrab=output_idx=%d:framerate=5,hwdownload,format=bgra,scale,format=yuv420p", o),
			"-frames:v", "1", "-c:v", "libx264", "-preset", "ultrafast", "-f", "null", "-")
		captureProcAttr(cmd)
		out, _ := cmd.CombinedOutput()
		text := string(out)
		adapterExists = strings.Contains(text, "Using device")
		// One line per try in the log: which card, and what happened.
		dev, what := "", ""
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if i := strings.Index(line, "Using device"); i >= 0 && dev == "" {
				dev = line[i+len("Using device "):]
			}
			low := strings.ToLower(line)
			if what == "" && (strings.Contains(low, "opened dxgi output") || strings.Contains(low, "failed") ||
				strings.Contains(low, "error") || strings.Contains(low, "denied") || strings.Contains(low, "not supported")) {
				if j := strings.Index(line, "] "); strings.HasPrefix(line, "[") && j > 0 {
					line = line[j+2:]
				}
				what = line
			}
		}
		log.Printf("screen: Desktop Duplication try: adapter %d output %d (%s): %s", a, o, dev, what)
		if m := reDDAOut.FindStringSubmatch(text); m != nil {
			gw, _ := strconv.Atoi(m[2])
			gh, _ := strconv.Atoi(m[3])
			if gw != w && gw*h == gh*w {
				log.Printf("screen: Desktop Duplication: FFmpeg sees this screen scaled to %dx%d (Windows display scaling) — not used, it would only capture part of it", gw, gh)
			}
			return gw == w && gh == h, true, adapterExists
		}
		return false, !strings.Contains(text, "Failed to enumerate DXGI output"), adapterExists
	}
	found := func(a, o int) (int, int, bool) {
		log.Printf("screen: Desktop Duplication: monitor %d is output %d of FFmpeg's adapter %d", hmonitor, o, a)
		ddaCache[hmonitor] = [2]int{a, o}
		return a, o, true
	}
	if d, ok := dxgiOutputFor(hmonitor); ok {
		if opened, _, _ := probe(d.adapter, d.output); opened {
			return found(d.adapter, d.output)
		}
	}
	for a := 0; a < 4; a++ {
		for o := 0; o < 6; o++ {
			opened, more, exists := probe(a, o)
			if !exists {
				log.Printf("screen: Desktop Duplication: no monitor of %dx%d found", w, h)
				return 0, 0, false
			}
			if opened {
				return found(a, o)
			}
			if !more {
				break
			}
		}
	}
	return 0, 0, false
}

// ffmpegDPIAware makes Windows give FFmpeg the screen's real size.
//
// With display scaling on (125% on a 1920x1080 laptop screen, say), Windows
// tells programs that don't declare themselves "DPI aware" a shrunken size
// (1536x864) — and Desktop Duplication in such a program captures only that
// much of the screen, the top-left part. FFmpeg doesn't declare it, so this
// sets the same switch as its Properties → Compatibility → "Change high DPI
// settings" → "Override high DPI scaling behaviour: Application", for this
// user and this FFmpeg only. Takes effect for FFmpeg runs started after it.
var ffmpegDPIOnce sync.Once

func ffmpegDPIAware() {
	ffmpegDPIOnce.Do(func() {
		exe := findFFmpeg()
		if exe == "" {
			return
		}
		if abs, err := filepath.Abs(exe); err == nil {
			exe = abs
		}
		advapi := syscall.NewLazyDLL("advapi32.dll")
		create := advapi.NewProc("RegCreateKeyExW")
		query := advapi.NewProc("RegQueryValueExW")
		set := advapi.NewProc("RegSetValueExW")
		closeKey := advapi.NewProc("RegCloseKey")
		const (
			hkcu     = 0x80000001
			keyRW    = 0x20019 | 0x20006 // KEY_READ | KEY_WRITE
			regSZ    = 1
			errOK    = 0
			wantFlag = "HIGHDPIAWARE"
		)
		sub, _ := syscall.UTF16PtrFromString(`Software\Microsoft\Windows NT\CurrentVersion\AppCompatFlags\Layers`)
		var key syscall.Handle
		if r, _, _ := create.Call(hkcu, uintptr(unsafe.Pointer(sub)), 0, 0, 0, keyRW, 0,
			uintptr(unsafe.Pointer(&key)), 0); r != errOK {
			log.Printf("screen: couldn't open the compatibility settings to make FFmpeg DPI aware (%d)", r)
			return
		}
		defer closeKey.Call(uintptr(key))
		name, _ := syscall.UTF16PtrFromString(exe)

		cur := ""
		buf := make([]uint16, 512)
		size := uint32(len(buf) * 2)
		var typ uint32
		if r, _, _ := query.Call(uintptr(key), uintptr(unsafe.Pointer(name)), 0,
			uintptr(unsafe.Pointer(&typ)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r == errOK && typ == regSZ {
			cur = syscall.UTF16ToString(buf)
		}
		for _, f := range strings.Fields(cur) {
			if strings.EqualFold(f, wantFlag) {
				return // already set
			}
		}
		val := "~ " + wantFlag
		if f := strings.Fields(cur); len(f) > 0 {
			if f[0] == "~" {
				f = f[1:]
			}
			val = "~ " + strings.Join(append(f, wantFlag), " ")
		}
		data, _ := syscall.UTF16FromString(val)
		if r, _, _ := set.Call(uintptr(key), uintptr(unsafe.Pointer(name)), 0, regSZ,
			uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)*2)); r != errOK {
			log.Printf("screen: couldn't make FFmpeg DPI aware (%d)", r)
			return
		}
		log.Printf("screen: marked %s as DPI aware (%s), so it sees screens at their real size", exe, val)
	})
}

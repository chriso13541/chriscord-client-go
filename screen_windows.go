//go:build windows

package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"sort"
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
	if kind == "screen" {
		for i, m := range listMonitors() {
			if uint64(m.handle) != handle {
				continue
			}
			w, h := shareSize(m.w, m.h, o.Height)
			if f["gfxcapture"] {
				return []string{"-filter_complex", fmt.Sprintf(
					"gfxcapture=hmonitor=%d:max_framerate=%d:capture_cursor=1:width=%d:height=%d:resize_mode=scale_aspect,hwdownload,format=bgra",
					handle, o.FPS, w, h)}, w, h, nil
			}
			// Older FFmpeg: Desktop Duplication (outputs counted left to right,
			// which matches most setups).
			return []string{"-filter_complex", fmt.Sprintf(
				"ddagrab=output_idx=%d:framerate=%d:draw_mouse=1,hwdownload,format=bgra,scale=%d:%d",
				i, o.FPS, w, h)}, w, h, nil
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
	sw, sh, _ := windowSize(hwnd)
	w, h := shareSize(sw, sh, o.Height)
	return []string{"-filter_complex", fmt.Sprintf(
		"gfxcapture=hwnd=%d:max_framerate=%d:capture_cursor=1:width=%d:height=%d:resize_mode=scale_aspect,hwdownload,format=bgra",
		handle, o.FPS, w, h)}, w, h, nil
}

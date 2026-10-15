//go:build windows

package main

// The window between runs, and the tray icon, on Windows (see window.go).
//
// Placement: GetWindowPlacement / SetWindowPlacement, which keep the
// window's normal rectangle (on whichever monitor) and whether it's
// maximised — the same thing Windows uses for its own apps. The window
// starts hidden (StartHidden in main.go), is put where it was, then shown,
// so it doesn't flash on the primary monitor first. If that monitor isn't
// there any more, it opens where Windows puts it.
//
// Tray: a Shell_NotifyIcon icon owned by a small hidden window on its own
// thread, with its own message loop. Left-click opens Chriscord; right-
// click has Open and Quit.

import (
	"os"
	goruntime "runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const windowManagerSupported = true

// (user32 and kernel32 are declared in idle_windows.go; pEnumWindows,
// pGetClassNameW, pGetWindowThreadProcId, pIsIconic, pOpenProcess and
// pCloseHandle in screen_windows.go.)
var (
	twShell32                = syscall.NewLazyDLL("shell32.dll")
	twShellNotifyIconW       = twShell32.NewProc("Shell_NotifyIconW")
	twExtractIconExW         = twShell32.NewProc("ExtractIconExW")
	twGetWindowPlacement     = user32.NewProc("GetWindowPlacement")
	twSetWindowPlacement     = user32.NewProc("SetWindowPlacement")
	twMonitorFromRect        = user32.NewProc("MonitorFromRect")
	twIsZoomed               = user32.NewProc("IsZoomed")
	twSetForegroundWindow    = user32.NewProc("SetForegroundWindow")
	twGetShellWindow         = user32.NewProc("GetShellWindow")
	twRegisterClassExW       = user32.NewProc("RegisterClassExW")
	twCreateWindowExW        = user32.NewProc("CreateWindowExW")
	twDefWindowProcW         = user32.NewProc("DefWindowProcW")
	twGetMessageW            = user32.NewProc("GetMessageW")
	twTranslateMessage       = user32.NewProc("TranslateMessage")
	twDispatchMessageW       = user32.NewProc("DispatchMessageW")
	twPostMessageW           = user32.NewProc("PostMessageW")
	twRegisterWindowMessageW = user32.NewProc("RegisterWindowMessageW")
	twCreatePopupMenu        = user32.NewProc("CreatePopupMenu")
	twAppendMenuW            = user32.NewProc("AppendMenuW")
	twTrackPopupMenu         = user32.NewProc("TrackPopupMenu")
	twDestroyMenu            = user32.NewProc("DestroyMenu")
	twGetCursorPos           = user32.NewProc("GetCursorPos")
	twLoadIconW              = user32.NewProc("LoadIconW")
	twGetModuleHandleW       = kernel32.NewProc("GetModuleHandleW")
	twGetProcessTimes        = kernel32.NewProc("GetProcessTimes")
)

const (
	twSWHide             = 0
	twWPFRestoreToMax    = 2
	twMonitorDefaultNull = 0
	twMinWindowSide      = 200 // a saved rectangle smaller than this isn't used
)

type twPoint struct{ X, Y int32 }

type twPlacement struct {
	Length, Flags, ShowCmd uint32
	PtMin, PtMax           twPoint
	RcNormal               winRect
}

// ── Finding our window ────────────────────────────────────────────────────────

var (
	mainHWNDMu   sync.Mutex
	mainHWND     uintptr
	findMu       sync.Mutex // one search at a time (findResult below)
	findResult   uintptr
	twEnumWindow = syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
		var pid uint32
		pGetWindowThreadProcId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		if int(pid) != os.Getpid() {
			return 1
		}
		var cls [64]uint16
		pGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&cls[0])), uintptr(len(cls)))
		if syscall.UTF16ToString(cls[:]) == "wailsWindow" { // Wails' main window class
			findResult = hwnd
			return 0
		}
		return 1
	})
)

func findMainWindow() uintptr {
	mainHWNDMu.Lock()
	h := mainHWND
	mainHWNDMu.Unlock()
	if h != 0 {
		return h
	}
	findMu.Lock()
	findResult = 0
	pEnumWindows.Call(twEnumWindow, 0)
	h = findResult
	findMu.Unlock()
	if h != 0 {
		mainHWNDMu.Lock()
		mainHWND = h
		mainHWNDMu.Unlock()
	}
	return h
}

// ── Placement ─────────────────────────────────────────────────────────────────

var (
	lastSavedMu sync.Mutex
	lastSaved   savedPlacement
)

// rememberPlacement saves where the window is now (if it's changed).
func rememberPlacement() {
	lastSavedMu.Lock()
	defer lastSavedMu.Unlock()
	h := findMainWindow()
	if h == 0 {
		return
	}
	p := twPlacement{Length: uint32(unsafe.Sizeof(twPlacement{}))}
	if r, _, _ := twGetWindowPlacement.Call(h, uintptr(unsafe.Pointer(&p))); r == 0 {
		return
	}
	zoomed, _, _ := twIsZoomed.Call(h)
	iconic, _, _ := pIsIconic.Call(h)
	sp := savedPlacement{
		Left: p.RcNormal.Left, Top: p.RcNormal.Top, Right: p.RcNormal.Right, Bottom: p.RcNormal.Bottom,
		Maximized: zoomed != 0 || (iconic != 0 && p.Flags&twWPFRestoreToMax != 0),
	}
	if sp.Right-sp.Left < twMinWindowSide || sp.Bottom-sp.Top < twMinWindowSide || sp == lastSaved {
		return
	}
	if updateWindowPrefs(func(w *windowPrefs) { w.Placement = &sp }) == nil {
		lastSaved = sp
	}
}

// placementFits: the saved rectangle is (at least partly) on a monitor
// that's connected now.
func placementFits(sp *savedPlacement) bool {
	if sp == nil || sp.Right-sp.Left < twMinWindowSide || sp.Bottom-sp.Top < twMinWindowSide {
		return false
	}
	r := winRect{Left: sp.Left, Top: sp.Top, Right: sp.Right, Bottom: sp.Bottom}
	m, _, _ := twMonitorFromRect.Call(uintptr(unsafe.Pointer(&r)), twMonitorDefaultNull)
	return m != 0
}

// placeWindowAtStart puts the (still hidden) window where it was last time
// and shows it — or leaves it in the tray / minimised, for Start minimized
// at sign-in. Then keeps the saved place up to date while it runs.
func (a *App) placeWindowAtStart() {
	var h uintptr
	for i := 0; i < 200 && h == 0; i++ { // up to 10 s for the window to exist
		if h = findMainWindow(); h == 0 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	prefs := loadWindowPrefs()
	if h == 0 {
		runtime.WindowShow(a.ctx) // never leave it hidden
		return
	}
	maximized := false
	if sp := prefs.Placement; placementFits(sp) {
		p := twPlacement{Length: uint32(unsafe.Sizeof(twPlacement{})), ShowCmd: twSWHide,
			RcNormal: winRect{Left: sp.Left, Top: sp.Top, Right: sp.Right, Bottom: sp.Bottom}}
		twSetWindowPlacement.Call(h, uintptr(unsafe.Pointer(&p)))
		maximized = sp.Maximized
		lastSavedMu.Lock()
		lastSaved = *sp
		lastSavedMu.Unlock()
	}
	if prefs.MinimizeToTray {
		trayShow(a, true)
	}
	startHidden := prefs.StartMinimized && launchedAtSignIn()
	switch {
	case startHidden && prefs.MinimizeToTray:
		// Stays hidden: the tray icon opens it (maximised, if it was).
		pendingMaximise.Store(maximized)
	case startHidden:
		// Minimised on the taskbar, without taking the focus at sign-in;
		// restoring it brings it back maximised if it was.
		const swShowMinNoActive = 7
		p := twPlacement{Length: uint32(unsafe.Sizeof(twPlacement{}))}
		if r, _, _ := twGetWindowPlacement.Call(h, uintptr(unsafe.Pointer(&p))); r != 0 {
			p.ShowCmd = swShowMinNoActive
			if maximized {
				p.Flags |= twWPFRestoreToMax
			}
			twSetWindowPlacement.Call(h, uintptr(unsafe.Pointer(&p)))
		} else {
			runtime.WindowShow(a.ctx)
			runtime.WindowMinimise(a.ctx)
		}
	default:
		runtime.WindowShow(a.ctx)
		if maximized {
			runtime.WindowMaximise(a.ctx)
		}
		bringToFront()
	}
	go func() {
		for range time.Tick(5 * time.Second) {
			if appQuitting.Load() {
				return
			}
			rememberPlacement()
		}
	}()
}

func bringToFront() {
	if h := findMainWindow(); h != 0 {
		twSetForegroundWindow.Call(h)
	}
}

// launchedAtSignIn: Chriscord was started by "Open when you sign in" —
// it's switched on and Chriscord started within a few minutes of the
// desktop (Explorer). (The sign-in entry starts the launcher, which starts
// Chriscord, so this can't be told from its command line.)
func launchedAtSignIn() bool {
	for _, a := range os.Args[1:] {
		if a == "--startup" {
			return true
		}
	}
	if !startupEnabled() {
		return false
	}
	shell, _, _ := twGetShellWindow.Call()
	if shell == 0 {
		return true // no desktop yet: as early as it gets
	}
	var pid uint32
	pGetWindowThreadProcId.Call(shell, uintptr(unsafe.Pointer(&pid)))
	explorer, ok := processStart(uint32(pid))
	if !ok {
		return false
	}
	self, ok := processStart(uint32(os.Getpid()))
	if !ok {
		return false
	}
	return self-explorer < uint64(3*time.Minute/100) // FILETIME: 100 ns units
}

// processStart is when a process started (FILETIME, 100 ns since 1601).
func processStart(pid uint32) (uint64, bool) {
	const processQueryLimited = 0x1000
	h, _, _ := pOpenProcess.Call(processQueryLimited, 0, uintptr(pid))
	if h == 0 {
		return 0, false
	}
	defer pCloseHandle.Call(h)
	var create, exit, kern, user [2]uint32
	if r, _, _ := twGetProcessTimes.Call(h, uintptr(unsafe.Pointer(&create)), uintptr(unsafe.Pointer(&exit)),
		uintptr(unsafe.Pointer(&kern)), uintptr(unsafe.Pointer(&user))); r == 0 {
		return 0, false
	}
	return uint64(create[1])<<32 | uint64(create[0]), true
}

// ── Tray icon ─────────────────────────────────────────────────────────────────

type twNotifyIconData struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         [16]byte
	HBalloonIcon     uintptr
}

type twWndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type twMsg struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      twPoint
	_       uint32
}

const (
	twWMApp          = 0x8000
	twMsgTrayEvent   = twWMApp + 1 // from the icon (clicks)
	twMsgTraySet     = twWMApp + 2 // wParam 1: show the icon, 0: remove it
	twWMNull         = 0x0000
	twWMLButtonUp    = 0x0202
	twWMLButtonDbl   = 0x0203
	twWMRButtonUp    = 0x0205
	twNIMAdd         = 0
	twNIMDelete      = 2
	twNIFMessage     = 1
	twNIFIcon        = 2
	twNIFTip         = 4
	twMFString       = 0
	twMFSeparator    = 0x800
	twTPMReturnCmd   = 0x100
	twTPMRightButton = 0x2
	twMenuOpen       = 1
	twMenuQuit       = 2
)

var tray struct {
	mu        sync.Mutex
	app       *App
	hwnd      uintptr // the tray thread's window, once it's running
	started   bool
	shown     bool // the icon is in the notification area
	icon      uintptr
	created   uint32 // "TaskbarCreated": Explorer restarted, add the icon again
	wantShown bool   // asked for before the thread was ready
}

// trayShow shows or removes the tray icon (starting the tray thread the
// first time it's wanted).
func trayShow(a *App, on bool) {
	tray.mu.Lock()
	tray.app = a
	tray.wantShown = on
	if !tray.started {
		if !on {
			tray.mu.Unlock()
			return
		}
		tray.started = true
		tray.mu.Unlock()
		go trayThread()
		return
	}
	h := tray.hwnd
	tray.mu.Unlock()
	if h != 0 {
		var w uintptr
		if on {
			w = 1
		}
		twPostMessageW.Call(h, twMsgTraySet, w, 0)
	}
}

var twTrayWndProc = syscall.NewCallback(func(hwnd, msg, wparam, lparam uintptr) uintptr {
	switch uint32(msg) {
	case twMsgTrayEvent:
		switch uint32(lparam & 0xFFFF) {
		case twWMLButtonUp, twWMLButtonDbl:
			if a := trayApp(); a != nil {
				go a.showWindow()
			}
		case twWMRButtonUp:
			trayMenu(hwnd)
		}
		return 0
	case twMsgTraySet:
		trayIcon(hwnd, wparam != 0)
		return 0
	}
	tray.mu.Lock()
	created, shown := tray.created, tray.shown
	tray.mu.Unlock()
	if created != 0 && uint32(msg) == created && shown {
		tray.mu.Lock()
		tray.shown = false
		tray.mu.Unlock()
		trayIcon(hwnd, true)
		return 0
	}
	r, _, _ := twDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
})

func trayApp() *App {
	tray.mu.Lock()
	defer tray.mu.Unlock()
	return tray.app
}

// trayThread owns the tray's window and runs its messages, for as long as
// Chriscord runs.
func trayThread() {
	goruntime.LockOSThread()
	hinst, _, _ := twGetModuleHandleW.Call(0)
	cls, _ := syscall.UTF16PtrFromString("ChriscordTray")
	wc := twWndClassEx{LpfnWndProc: twTrayWndProc, HInstance: hinst, LpszClassName: cls}
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	twRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	title, _ := syscall.UTF16PtrFromString("Chriscord tray")
	// A plain (never shown) window rather than a message-only one, so it
	// hears "TaskbarCreated" when Explorer restarts.
	h, _, _ := twCreateWindowExW.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(title)), 0,
		0, 0, 0, 0, 0, 0, hinst, 0)
	if h == 0 {
		tray.mu.Lock()
		tray.started = false
		tray.mu.Unlock()
		return
	}
	created, _ := syscall.UTF16PtrFromString("TaskbarCreated")
	tc, _, _ := twRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(created)))
	// The app's own icon, from the exe.
	var small uintptr
	if exe, err := os.Executable(); err == nil {
		p, _ := syscall.UTF16PtrFromString(exe)
		twExtractIconExW.Call(uintptr(unsafe.Pointer(p)), 0, 0, uintptr(unsafe.Pointer(&small)), 1)
	}
	if small == 0 {
		const idiApplication = 32512
		small, _, _ = twLoadIconW.Call(0, idiApplication)
	}
	tray.mu.Lock()
	tray.hwnd = h
	tray.icon = small
	tray.created = uint32(tc)
	want := tray.wantShown
	tray.mu.Unlock()
	if want {
		trayIcon(h, true)
	}
	var m twMsg
	for {
		r, _, _ := twGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return
		}
		twTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		twDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// trayIcon adds or removes the icon (on the tray thread).
func trayIcon(hwnd uintptr, on bool) {
	tray.mu.Lock()
	defer tray.mu.Unlock()
	if on == tray.shown {
		return
	}
	nid := twNotifyIconData{HWnd: hwnd, UID: 1}
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	if on {
		nid.UFlags = twNIFMessage | twNIFIcon | twNIFTip
		nid.UCallbackMessage = twMsgTrayEvent
		nid.HIcon = tray.icon
		tip, _ := syscall.UTF16FromString("Chriscord")
		copy(nid.SzTip[:], tip)
		if r, _, _ := twShellNotifyIconW.Call(twNIMAdd, uintptr(unsafe.Pointer(&nid))); r != 0 {
			tray.shown = true
		}
		return
	}
	twShellNotifyIconW.Call(twNIMDelete, uintptr(unsafe.Pointer(&nid)))
	tray.shown = false
}

// trayMenu is the right-click menu: Open Chriscord, Quit Chriscord.
func trayMenu(hwnd uintptr) {
	menu, _, _ := twCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer twDestroyMenu.Call(menu)
	open, _ := syscall.UTF16PtrFromString("Open Chriscord")
	quit, _ := syscall.UTF16PtrFromString("Quit Chriscord")
	twAppendMenuW.Call(menu, twMFString, twMenuOpen, uintptr(unsafe.Pointer(open)))
	twAppendMenuW.Call(menu, twMFSeparator, 0, 0)
	twAppendMenuW.Call(menu, twMFString, twMenuQuit, uintptr(unsafe.Pointer(quit)))
	var pt twPoint
	twGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// Needed for the menu to close when you click elsewhere (a long-standing
	// quirk of tray menus), with the WM_NULL after it.
	twSetForegroundWindow.Call(hwnd)
	cmd, _, _ := twTrackPopupMenu.Call(menu, twTPMReturnCmd|twTPMRightButton, uintptr(pt.X), uintptr(pt.Y), 0, hwnd, 0)
	twPostMessageW.Call(hwnd, twWMNull, 0, 0)
	a := trayApp()
	if a == nil {
		return
	}
	switch cmd {
	case twMenuOpen:
		go a.showWindow()
	case twMenuQuit:
		go a.quitApp()
	}
}

// trayRemove takes the icon away as Chriscord closes (otherwise it lingers
// until the mouse passes over it).
func trayRemove() {
	tray.mu.Lock()
	h := tray.hwnd
	shown := tray.shown
	tray.mu.Unlock()
	if h != 0 && shown {
		trayIcon(h, false)
	}
}

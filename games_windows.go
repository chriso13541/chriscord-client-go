//go:build windows

package main

// Finding the games you have open on Windows: the programs behind visible
// top-level windows, checked with identifyGame (games_match.go).

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

const gameDetectionSupported = true

var (
	gameWinMu   sync.Mutex
	gameWinPIDs map[uint32]string // pid → a window title, while enumerating
	gameEnumCB  = syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
		if v, _, _ := pIsWindowVisible.Call(hwnd); v == 0 {
			return 1
		}
		var pid uint32
		pGetWindowThreadProcId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		if pid == 0 || int(pid) == os.Getpid() {
			return 1
		}
		if _, seen := gameWinPIDs[pid]; !seen {
			gameWinPIDs[pid] = windowTitle(hwnd)
		}
		return 1
	})
)

func windowTitle(hwnd uintptr) string {
	n, _, _ := pGetWindowTextLengthW.Call(hwnd)
	if n == 0 || n > 512 {
		return ""
	}
	buf := make([]uint16, n+1)
	pGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), n+1)
	return syscall.UTF16ToString(buf)
}

func processPath(pid uint32) string {
	const processQueryLimited = 0x1000
	h, _, _ := pOpenProcess.Call(processQueryLimited, 0, uintptr(pid))
	if h == 0 {
		return ""
	}
	defer pCloseHandle.Call(h)
	buf := make([]uint16, 1024)
	size := uint32(len(buf))
	if r, _, _ := pQueryFullProcessImageNW.Call(h, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:size])
}

// detectGame: the game you're playing now ("" for none). current is the
// one being shown, kept when several are open.
func detectGame(current string) (string, uint32) {
	gameWinMu.Lock()
	gameWinPIDs = map[uint32]string{}
	pEnumWindows.Call(gameEnumCB, 0)
	pids := gameWinPIDs
	gameWinPIDs = nil
	gameWinMu.Unlock()

	var fgPID uint32
	if fg, _, _ := pGetForegroundWindow.Call(); fg != 0 {
		pGetWindowThreadProcId.Call(fg, uintptr(unsafe.Pointer(&fgPID)))
	}
	custom := customGameNames()
	seen := map[string]bool{}
	var found []string
	foreground := ""
	pidOf := map[string]uint32{}
	for pid, title := range pids {
		path := processPath(pid)
		if path == "" {
			continue
		}
		// One you've added yourself counts whatever it is, by your name for it.
		g, mine := custom[strings.ToLower(path)]
		if !mine {
			g = identifyGame(path, title)
		} // (one you've hidden is "", so it's skipped)
		if g == "" {
			continue
		}
		if pid == fgPID {
			foreground = g
		}
		if _, ok := pidOf[g]; !ok || pid == fgPID {
			pidOf[g] = pid
		}
		if !seen[g] {
			seen[g] = true
			found = append(found, g)
		}
	}
	sort.Strings(found)
	g := chooseGame(found, foreground, current)
	return g, pidOf[g]
}

// ── Watching for changes ──────────────────────────────────────────────────────

var (
	pSetWinEventHook     = user32.NewProc("SetWinEventHook")
	pWaitForSingleObject = kernel32.NewProc("WaitForSingleObject")
	pGetExitCodeProcess  = kernel32.NewProc("GetExitCodeProcess")
	foregroundKick       func()
	foregroundCB         = syscall.NewCallback(func(hook, event, hwnd, idObject, idChild, thread, when uintptr) uintptr {
		if foregroundKick != nil {
			foregroundKick()
		}
		return 0
	})
)

// startForegroundWatch calls kick whenever the window in front changes
// (Windows tells us: SetWinEventHook, no checking on a timer). False if it
// couldn't be set up.
func startForegroundWatch(kick func()) bool {
	foregroundKick = kick
	ready := make(chan bool, 1)
	go func() {
		goruntime.LockOSThread() // the hook's messages come to this thread
		const eventSystemForeground = 0x0003
		const winEventOutOfContext = 0x0000
		h, _, _ := pSetWinEventHook.Call(eventSystemForeground, eventSystemForeground, 0, foregroundCB, 0, 0, winEventOutOfContext)
		ready <- h != 0
		if h == 0 {
			return
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
	}()
	return <-ready
}

const (
	processSynchronize   = 0x00100000
	processQueryLimitedI = 0x1000
	stillActive          = 259
)

// processAlive: the process is still running.
func processAlive(pid uint32) bool {
	h, _, _ := pOpenProcess.Call(processQueryLimitedI, 0, uintptr(pid))
	if h == 0 {
		return false
	}
	defer pCloseHandle.Call(h)
	var code uint32
	if r, _, _ := pGetExitCodeProcess.Call(h, uintptr(unsafe.Pointer(&code))); r == 0 {
		return false
	}
	return code == stillActive
}

// watchGameExit calls kick when the game's process ends — or stops
// watching once it's no longer the game being shown.
func watchGameExit(pid uint32, kick func()) {
	h, _, _ := pOpenProcess.Call(processSynchronize|processQueryLimitedI, 0, uintptr(pid))
	if h == 0 {
		return
	}
	defer pCloseHandle.Call(h)
	const waitObject0 = 0
	for {
		r, _, _ := pWaitForSingleObject.Call(h, 5000)
		if r == waitObject0 {
			kick()
			return
		}
		gameState.mu.Lock()
		still := gameState.pid == pid
		gameState.mu.Unlock()
		if !still {
			return
		}
	}
}

// openPrograms: the programs with a window open now (for "Add a game"),
// one each, by path.
func openPrograms() []RunningProgram {
	gameWinMu.Lock()
	gameWinPIDs = map[uint32]string{}
	pEnumWindows.Call(gameEnumCB, 0)
	pids := gameWinPIDs
	gameWinPIDs = nil
	gameWinMu.Unlock()
	seen := map[string]bool{}
	var out []RunningProgram
	for pid, title := range pids {
		path := processPath(pid)
		lp := strings.ToLower(path)
		if path == "" || seen[lp] || title == "" || strings.HasPrefix(lp, strings.ToLower(os.Getenv("SystemRoot"))+`\`) {
			continue // Windows' own (Explorer, Settings, …) aren't offered
		}
		seen[lp] = true
		out = append(out, RunningProgram{Path: path, Name: programName(path), Title: title})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

var (
	modVersion               = syscall.NewLazyDLL("version.dll")
	pGetFileVersionInfoSizeW = modVersion.NewProc("GetFileVersionInfoSizeW")
	pGetFileVersionInfoW     = modVersion.NewProc("GetFileVersionInfoW")
	pVerQueryValueW          = modVersion.NewProc("VerQueryValueW")
)

// programName: what a program calls itself — its file description (what
// Task Manager shows), else its product name, else its file name.
func programName(path string) string {
	p, err := syscall.UTF16PtrFromString(path)
	if err == nil {
		if size, _, _ := pGetFileVersionInfoSizeW.Call(uintptr(unsafe.Pointer(p)), 0); size > 0 && size < 1<<20 {
			buf := make([]byte, size)
			if r, _, _ := pGetFileVersionInfoW.Call(uintptr(unsafe.Pointer(p)), 0, size, uintptr(unsafe.Pointer(&buf[0]))); r != 0 {
				for _, field := range []string{"FileDescription", "ProductName"} {
					if v := versionString(buf, field); v != "" {
						return v
					}
				}
			}
		}
	}
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// versionString reads one string from a program's version information, in
// the first language it lists (or US English).
func versionString(info []byte, field string) string {
	query := func(sub string) (uintptr, uint32) {
		s, _ := syscall.UTF16PtrFromString(sub)
		var ptr uintptr
		var n uint32
		if r, _, _ := pVerQueryValueW.Call(uintptr(unsafe.Pointer(&info[0])), uintptr(unsafe.Pointer(s)),
			uintptr(unsafe.Pointer(&ptr)), uintptr(unsafe.Pointer(&n))); r == 0 || ptr == 0 || n == 0 {
			return 0, 0
		}
		return ptr, n
	}
	langs := []string{"040904b0", "040904e4", "000004b0"}
	if ptr, n := query(`\VarFileInfo\Translation`); n >= 4 {
		tr := (*[2]uint16)(unsafe.Pointer(ptr))
		langs = append([]string{hex4(tr[0]) + hex4(tr[1])}, langs...)
	}
	for _, l := range langs {
		ptr, n := query(`\StringFileInfo\` + l + `\` + field)
		if n == 0 {
			continue
		}
		chars := unsafe.Slice((*uint16)(unsafe.Pointer(ptr)), n)
		if v := strings.TrimSpace(syscall.UTF16ToString(chars)); v != "" {
			return v
		}
	}
	return ""
}

func hex4(v uint16) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[v>>12&15], digits[v>>8&15], digits[v>>4&15], digits[v&15]})
}

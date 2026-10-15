//go:build windows

package main

// Finding the games you have open on Windows: the programs behind visible
// top-level windows, checked with identifyGame (games_match.go).

import (
	"os"
	"sort"
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
func detectGame(current string) string {
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
	seen := map[string]bool{}
	var found []string
	foreground := ""
	for pid, title := range pids {
		path := processPath(pid)
		if path == "" {
			continue
		}
		g := identifyGame(path, title)
		if g == "" {
			continue
		}
		if pid == fgPID {
			foreground = g
		}
		if !seen[g] {
			seen[g] = true
			found = append(found, g)
		}
	}
	sort.Strings(found)
	return chooseGame(found, foreground, current)
}

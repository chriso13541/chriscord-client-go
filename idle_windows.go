//go:build windows

package main

import (
	"errors"
	"syscall"
	"time"
	"unsafe"
)

var errIdleUnsupported = errors.New("idle time unavailable")

var (
	user32               = syscall.NewLazyDLL("user32.dll")
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procGetLastInputInfo = user32.NewProc("GetLastInputInfo")
	procGetTickCount     = kernel32.NewProc("GetTickCount")
)

type lastInputInfo struct {
	cbSize uint32
	dwTime uint32
}

// GetLastInputInfo gives the tick count (ms since boot, 32-bit) of the last
// keyboard/mouse input in this session. Both counts are 32-bit, so the
// unsigned subtraction stays right across the 49.7-day wraparound.
func systemIdle() (time.Duration, error) {
	info := lastInputInfo{cbSize: uint32(unsafe.Sizeof(lastInputInfo{}))}
	if r, _, err := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&info))); r == 0 {
		return 0, err
	}
	now, _, _ := procGetTickCount.Call()
	return time.Duration(uint32(now)-info.dwTime) * time.Millisecond, nil
}

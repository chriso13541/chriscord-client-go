//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// "Open Chriscord when you sign in": the same entry any app adds under the
// current user's Run key, which is also what Task Manager > Startup apps
// and Settings > Apps > Startup list (and can turn off).

const startupSupported = true

const (
	hkeyCurrentUser    = 0x80000001
	runKeyPath         = `Software\Microsoft\Windows\CurrentVersion\Run`
	startupApprovedKey = `Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run`
	runValueName       = "Chriscord"
	regTypeSZ          = 1
	rrfRtRegSZ         = 0x2
	rrfRtRegBinary     = 0x8
)

var (
	modAdvapi32         = syscall.NewLazyDLL("advapi32.dll")
	procRegGetValueW    = modAdvapi32.NewProc("RegGetValueW")
	procRegSetKeyValueW = modAdvapi32.NewProc("RegSetKeyValueW")
	procRegDeleteKeyVal = modAdvapi32.NewProc("RegDeleteKeyValueW")
)

func regGet(key, name string, flags uintptr) ([]byte, bool) {
	k, _ := syscall.UTF16PtrFromString(key)
	n, _ := syscall.UTF16PtrFromString(name)
	var size uint32
	if r, _, _ := procRegGetValueW.Call(hkeyCurrentUser, uintptr(unsafe.Pointer(k)), uintptr(unsafe.Pointer(n)), flags, 0, 0, uintptr(unsafe.Pointer(&size))); r != 0 || size == 0 {
		return nil, false
	}
	buf := make([]byte, size)
	if r, _, _ := procRegGetValueW.Call(hkeyCurrentUser, uintptr(unsafe.Pointer(k)), uintptr(unsafe.Pointer(n)), flags, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r != 0 {
		return nil, false
	}
	return buf[:size], true
}

func regDeleteValue(key, name string) {
	k, _ := syscall.UTF16PtrFromString(key)
	n, _ := syscall.UTF16PtrFromString(name)
	procRegDeleteKeyVal.Call(hkeyCurrentUser, uintptr(unsafe.Pointer(k)), uintptr(unsafe.Pointer(n)))
}

// startupEnabled: there's a Run entry, and it hasn't been switched off in
// Task Manager / Settings (StartupApproved: first byte odd = disabled).
func startupEnabled() bool {
	if _, ok := regGet(runKeyPath, runValueName, rrfRtRegSZ); !ok {
		return false
	}
	if b, ok := regGet(startupApprovedKey, runValueName, rrfRtRegBinary); ok && len(b) > 0 && b[0]&1 == 1 {
		return false
	}
	return true
}

// setStartup adds or removes the Run entry. command is the full command
// line (quoted exe path). Turning it on also clears a "disabled" mark left
// by Task Manager, so it really starts.
func setStartup(on bool, command string) error {
	if !on {
		regDeleteValue(runKeyPath, runValueName)
		regDeleteValue(startupApprovedKey, runValueName)
		return nil
	}
	k, _ := syscall.UTF16PtrFromString(runKeyPath)
	n, _ := syscall.UTF16PtrFromString(runValueName)
	v, err := syscall.UTF16FromString(command)
	if err != nil {
		return err
	}
	if r, _, _ := procRegSetKeyValueW.Call(hkeyCurrentUser, uintptr(unsafe.Pointer(k)), uintptr(unsafe.Pointer(n)), regTypeSZ,
		uintptr(unsafe.Pointer(&v[0])), uintptr(len(v)*2)); r != 0 {
		return syscall.Errno(r)
	}
	regDeleteValue(startupApprovedKey, runValueName)
	return nil
}

//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	procSHGetKnownFolderPath = syscall.NewLazyDLL("shell32.dll").NewProc("SHGetKnownFolderPath")
	procCoTaskMemFree        = syscall.NewLazyDLL("ole32.dll").NewProc("CoTaskMemFree")
	// FOLDERID_Downloads {374DE290-123F-4565-9164-39C4925E467B}
	folderIDDownloads = syscall.GUID{Data1: 0x374DE290, Data2: 0x123F, Data3: 0x4565, Data4: [8]byte{0x91, 0x64, 0x39, 0xC4, 0x92, 0x5E, 0x46, 0x7B}}
)

// systemDownloadsDir: the Downloads folder, wherever Windows has it (it
// can be moved to another drive in its Properties).
func systemDownloadsDir() string {
	var p *uint16
	if r, _, _ := procSHGetKnownFolderPath.Call(uintptr(unsafe.Pointer(&folderIDDownloads)), 0, 0, uintptr(unsafe.Pointer(&p))); r == 0 && p != nil {
		n := 0
		for *(*uint16)(unsafe.Add(unsafe.Pointer(p), n*2)) != 0 {
			n++
		}
		dir := syscall.UTF16ToString(unsafe.Slice(p, n))
		procCoTaskMemFree.Call(uintptr(unsafe.Pointer(p)))
		if dir != "" {
			return dir
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Downloads")
}

// markDownloaded marks the file as from the internet, as browsers do, so
// Windows (SmartScreen, Office's Protected View) treats it with the same
// care as one downloaded in a browser.
func markDownloaded(path, server string) {
	zone := "[ZoneTransfer]\r\nZoneId=3\r\n"
	if server != "" {
		zone += "ReferrerUrl=" + normaliseHTTP(server) + "/\r\n"
	}
	os.WriteFile(path+":Zone.Identifier", []byte(zone), 0644) // best effort (not on FAT drives)
}

// showInFolder opens Explorer with the file selected.
func showInFolder(path string) error {
	cmd := exec.Command("explorer.exe")
	// explorer wants /select,"path" exactly; Go's own quoting would quote
	// the whole argument, which it doesn't understand.
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + path + `"`}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

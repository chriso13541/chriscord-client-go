//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// No console window flashing up while FFmpeg works — and, with "low
// priority" on, FFmpeg starts at below-normal priority so the rest of the
// computer (games, the call you're in) comes first.
func init() {
	hideWindow = func(cmd *exec.Cmd) {
		flags := uint32(0x08000000) // CREATE_NO_WINDOW
		if _, lowPrio, _ := transcodeSettings(); lowPrio {
			flags |= 0x00004000 // BELOW_NORMAL_PRIORITY_CLASS
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
	}
}

// Priority is set at creation on Windows (above).
func lowerPriority(cmd *exec.Cmd) {}

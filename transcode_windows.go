//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// No console window flashing up while FFmpeg works.
func init() {
	hideWindow = func(cmd *exec.Cmd) {
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	}
}

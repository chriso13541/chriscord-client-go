//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// lowerPriority "nices" FFmpeg so the rest of the computer (games, the call
// you're in) comes first while it converts.
func lowerPriority(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Setpriority(syscall.PRIO_PROCESS, cmd.Process.Pid, 10)
	}
}

//go:build !windows

package main

import "os/exec"

// boostCaptureProcess: only Windows needs it (gpu_priority_windows.go).
func boostCaptureProcess(*exec.Cmd) {}

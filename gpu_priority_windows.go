//go:build windows

package main

import (
	"log"
	"os/exec"
	"syscall"
)

// Capturing a screen while a game runs uncapped.
//
// A game with no frame cap keeps the graphics card 100% busy, and Windows
// shares the card between programs by their GPU scheduling priority — all
// "normal" by default. FFmpeg's capture (Desktop Duplication / Windows
// Graphics Capture) and the copy of each frame off the card then wait in
// line behind the game's frames, so the share falls behind and stutters,
// and it smooths out again as soon as the game is capped and leaves some
// room. OBS and Sunshine solve it the same way: their capture runs at a
// higher GPU priority than the game.
//
// "High" is used, not "realtime": realtime needs administrator rights, and
// NVIDIA's driver is known to freeze NVENC (or crash) with it, especially
// with hardware-accelerated GPU scheduling on.

const (
	d3dkmtPriorityHigh      = 4 // D3DKMT_SCHEDULINGPRIORITYCLASS_HIGH
	processSetInformation   = 0x0200
	processQueryInformation = 0x0400
	processQueryLimitedInfo = 0x1000
)

var pSetGPUPriority = syscall.NewLazyDLL("gdi32.dll").NewProc("D3DKMTSetProcessSchedulingPriorityClass")

// boostCaptureProcess raises a capture FFmpeg's GPU scheduling priority
// (right after it starts, before it has set up its capture). If Windows
// won't, the share still runs, as before.
func boostCaptureProcess(cmd *exec.Cmd) {
	if cmd.Process == nil || pSetGPUPriority.Find() != nil {
		return
	}
	h, err := syscall.OpenProcess(processSetInformation|processQueryInformation|processQueryLimitedInfo, false, uint32(cmd.Process.Pid))
	if err != nil {
		log.Printf("screen: couldn't raise FFmpeg's GPU priority: %v", err)
		return
	}
	defer syscall.CloseHandle(h)
	if status, _, _ := pSetGPUPriority.Call(uintptr(h), d3dkmtPriorityHigh); status != 0 {
		log.Printf("screen: Windows didn't raise FFmpeg's GPU priority (NTSTATUS 0x%08x); a game running uncapped may make the share stutter", uint32(status))
		return
	}
	log.Printf("screen: FFmpeg (pid %d) runs at high GPU priority", cmd.Process.Pid)
}

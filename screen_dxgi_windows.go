//go:build windows

package main

/*
#include "screen_dxgi_windows.h"
*/
import "C"

// dxgiOutput is where Desktop Duplication (ddagrab) finds a monitor.
type dxgiOutput struct {
	adapter    int    // index in THIS process's adapter list
	output     int    // output index on that adapter
	vendor     uint32 // the adapter's PCI vendor (0x8086 Intel, 0x10de NVIDIA, 0x1002 AMD)
	sameVendor int    // adapters with that vendor (1 = it identifies the adapter)
}

// dxgiOutputFor finds a monitor's adapter and output (screen_dxgi_windows.c).
func dxgiOutputFor(hmonitor uint64) (dxgiOutput, bool) {
	var a, o, same C.int
	var vendor C.uint
	if C.dxgi_find_output(C.ulonglong(hmonitor), &a, &o, &vendor, &same) != 0 {
		return dxgiOutput{}, false
	}
	return dxgiOutput{int(a), int(o), uint32(vendor), int(same)}, true
}

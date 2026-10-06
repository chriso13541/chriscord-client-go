//go:build windows

package main

/*
#include "screen_dxgi_windows.h"
*/
import "C"

// dxgiOutputFor: the adapter and output index Desktop Duplication (ddagrab)
// knows this monitor by (screen_dxgi_windows.c).
func dxgiOutputFor(hmonitor uint64) (adapter, output int, ok bool) {
	var a, o C.int
	if C.dxgi_find_output(C.ulonglong(hmonitor), &a, &o) != 0 {
		return 0, 0, false
	}
	return int(a), int(o), true
}

//go:build windows

package main

/*
#cgo LDFLAGS: -lole32
#include "sysaudio_windows.h"
*/
import "C"

import "errors"

// The computer's sound for a screen share, through WASAPI process loopback
// (sysaudio_windows.c — plain Win32, no extra libraries): exclude=true
// records everything except the given process (this app — so the call isn't
// sent back to itself), false only that process and what it started (one
// shared application).

func startSystemAudio(pid uint32, exclude bool) error {
	var msg [256]C.char
	ex := C.int(0)
	if exclude {
		ex = 1
	}
	if C.sa_start(C.uint(pid), ex, &msg[0], C.int(len(msg))) != 0 {
		return errors.New(C.GoString(&msg[0]))
	}
	return nil
}

// readSystemAudio fills out (interleaved stereo float32) and says how many
// frames it got.
func readSystemAudio(out []float32) int {
	if len(out) < 2 {
		return 0
	}
	return int(C.sa_read((*C.float)(&out[0]), C.int(len(out)/2)))
}

func systemAudioAvailable() int { return int(C.sa_available()) }

func stopSystemAudio() { C.sa_stop() }

//go:build windows

package main

/*
#cgo CFLAGS: -I${SRCDIR}/third_party/miniaudio
#cgo LDFLAGS: -lole32
#include "sysaudio_windows.h"
*/
import "C"

import (
	"errors"
	"strings"
)

// The computer's sound for a screen share, through WASAPI process loopback
// (sysaudio_windows.c): exclude=true records everything except the given
// process (this app — so the call isn't sent back to itself), false only
// that process and what it started (one shared application).

func startSystemAudio(pid uint32, exclude bool) error {
	var msg [256]C.char
	ex := C.int(0)
	if exclude {
		ex = 1
	}
	if C.sa_start(C.uint(pid), ex, &msg[0], C.int(len(msg))) != 0 {
		reason := C.GoString(&msg[0])
		if strings.Contains(strings.ToLower(reason), "not supported") || strings.Contains(strings.ToLower(reason), "invalid") {
			return errors.New("this version of Windows can't capture one app's sound separately — Windows 10 (21H2) or Windows 11 is needed (" + reason + ")")
		}
		return errors.New(reason)
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

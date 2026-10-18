// Package rnnoise is noise suppression for a voice: RNNoise 0.2
// (https://github.com/xiph/rnnoise, BSD licence, see COPYING), built in
// from source with its model. It takes 48 kHz mono sound in 10 ms frames
// (480 samples) and gives back the voice with background noise — fans,
// keyboards, hum, traffic — taken out, plus how likely it is that someone
// is speaking.
//
// The C files here are RNNoise's own, unchanged except that nnet.h no
// longer warns, while building, that only SSE2 is used.
package rnnoise

/*
#cgo CFLAGS: -O2 -I${SRCDIR} -DRNNOISE_BUILD -w
#cgo !windows LDFLAGS: -lm
#include <stdlib.h>
#include "rnnoise.h"
*/
import "C"

import (
	"runtime"
	"unsafe"
)

// FrameSize is how many samples go in and out at a time: 10 ms at 48 kHz.
const FrameSize = 480

// Denoiser cleans up one stream of sound (it learns the noise as it goes,
// so each microphone needs its own). Not safe for use from several
// goroutines at once.
type Denoiser struct {
	st      *C.DenoiseState
	in, out *C.float // C memory: FrameSize floats each
}

// New makes a Denoiser with RNNoise's built-in model.
func New() *Denoiser {
	d := &Denoiser{
		st:  C.rnnoise_create(nil),
		in:  (*C.float)(C.malloc(C.size_t(FrameSize * C.sizeof_float))),
		out: (*C.float)(C.malloc(C.size_t(FrameSize * C.sizeof_float))),
	}
	runtime.SetFinalizer(d, (*Denoiser).Close)
	return d
}

// Process cleans one 10 ms frame in place. Samples are scaled like 16-bit
// audio (−32768…32767), as RNNoise expects. It returns the probability
// (0…1) that the frame has speech in it.
func (d *Denoiser) Process(frame []float32) float32 {
	if d.st == nil || len(frame) < FrameSize {
		return 0
	}
	in := unsafe.Slice((*float32)(unsafe.Pointer(d.in)), FrameSize)
	out := unsafe.Slice((*float32)(unsafe.Pointer(d.out)), FrameSize)
	copy(in, frame[:FrameSize])
	vad := C.rnnoise_process_frame(d.st, d.out, d.in)
	copy(frame[:FrameSize], out)
	return float32(vad)
}

// Close frees it (also done when it's garbage collected).
func (d *Denoiser) Close() {
	if d.st != nil {
		C.rnnoise_destroy(d.st)
		C.free(unsafe.Pointer(d.in))
		C.free(unsafe.Pointer(d.out))
		d.st, d.in, d.out = nil, nil, nil
	}
}

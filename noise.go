package main

import (
	"log"
	"sync/atomic"

	"chriscord-client/rnnoise"
)

// Noise suppression for your microphone, powered by RNNoise (rnnoise/).
//
// It cleans up the microphone's sound on this computer — before the input
// volume, the speaking detection, Opus and the end-to-end encryption — so
// what everyone else receives is already clean, and nothing about it
// involves the server. Fans, keyboards, hum and other steady background
// noise are taken out while your voice comes through as it was; since
// what's left when you're not talking is close to silence, the speaking
// detection stops being opened by noise too.
//
// Each 20 ms frame (960 samples at 48 kHz) is two of RNNoise's 10 ms
// frames, so there's no extra delay or resampling. It costs well under 1%
// of one processor core.

var noiseSuppression atomic.Bool

// SetNoiseSuppression switches noise suppression on or off (the call's
// button and Settings → Voice & Video). It applies straight away, in a call
// and in the mic test.
func (a *App) SetNoiseSuppression(on bool) {
	if noiseSuppression.Swap(on) != on {
		log.Printf("voice: noise suppression (RNNoise) %s", map[bool]string{true: "on", false: "off"}[on])
	}
}

// NoiseSuppression reports whether it's on.
func (a *App) NoiseSuppression() bool { return noiseSuppression.Load() }

// micCleaner is one microphone's noise suppression (RNNoise learns the
// noise as it listens, so each capture has its own). Only used from the
// capture goroutine.
type micCleaner struct {
	d   *rnnoise.Denoiser
	buf [rnnoise.FrameSize]float32
}

// process cleans buf (a whole number of 10 ms frames) in place, when noise
// suppression is on.
func (m *micCleaner) process(buf []int16) {
	if !noiseSuppression.Load() {
		return
	}
	if m.d == nil {
		m.d = rnnoise.New()
	}
	for off := 0; off+rnnoise.FrameSize <= len(buf); off += rnnoise.FrameSize {
		frame := buf[off : off+rnnoise.FrameSize]
		for i, s := range frame {
			m.buf[i] = float32(s)
		}
		m.d.Process(m.buf[:])
		for i, v := range m.buf {
			switch {
			case v > 32767:
				frame[i] = 32767
			case v < -32768:
				frame[i] = -32768
			default:
				frame[i] = int16(v)
			}
		}
	}
}

// close frees it.
func (m *micCleaner) close() {
	if m.d != nil {
		m.d.Close()
		m.d = nil
	}
}

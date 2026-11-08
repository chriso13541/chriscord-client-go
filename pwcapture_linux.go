//go:build linux

package main

// The picture side of Wayland sharing: reading the PipeWire stream the
// portal handed out (portal_linux.go), in C (pwcapture_linux.c), and
// writing it to FFmpeg as raw BGRA at a steady frame rate — the same way
// a window copied with GDI is fed on Windows.
//
// Building needs PipeWire's headers (Ubuntu/Debian: libpipewire-0.3-dev;
// Arch: pipewire; Fedora: pipewire-devel). Running doesn't need anything
// extra: libpipewire is opened only when sharing on Wayland.

/*
#cgo CFLAGS: -I/usr/include/pipewire-0.3 -I/usr/include/spa-0.2 -D_REENTRANT
#cgo LDFLAGS: -ldl -lpthread
#include <stdlib.h>
#include "pwcapture_linux.h"
*/
import "C"

import (
	"errors"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// pwStreamAvailable: libpipewire can be opened ("" if so, else why not).
func pwStreamAvailable() string {
	var buf [256]C.char
	if C.pwcap_load(&buf[0], C.int(len(buf))) != 0 {
		return C.GoString(&buf[0])
	}
	return ""
}

type pwStream struct {
	mu    sync.Mutex // C calls and close
	c     *C.pwcap
	isEnd atomic.Bool
}

func openPWStream(fd int, node uint32, fps int) (*pwStream, error) {
	var buf [256]C.char
	c := C.pwcap_open(C.int(fd), C.uint32_t(node), C.int(fps), &buf[0], C.int(len(buf)))
	if c == nil {
		return nil, errors.New(C.GoString(&buf[0]))
	}
	return &pwStream{c: c}, nil
}

func (s *pwStream) ended() bool { return s.isEnd.Load() }
func (s *pwStream) markEnded()  { s.isEnd.Store(true) }

// size: the pictures' size so far (0x0 before the first).
func (s *pwStream) size() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c == nil {
		return 0, 0
	}
	var w, h C.int
	C.pwcap_size(s.c, &w, &h)
	return int(w), int(h)
}

// frame scales the newest picture into dst (w x h BGRA); see PWCAP_*.
func (s *pwStream) frame(dst []byte, w, h int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c == nil || s.isEnd.Load() {
		return C.PWCAP_ENDED
	}
	r := int(C.pwcap_frame(s.c, (*C.uint8_t)(unsafe.Pointer(&dst[0])), C.int(w), C.int(h)))
	if r == C.PWCAP_ENDED {
		s.isEnd.Store(true)
	}
	return r
}

func (s *pwStream) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c != nil {
		C.pwcap_close(s.c)
		s.c = nil
	}
	s.isEnd.Store(true)
}

// waitFirstFrame: the first picture's size, waiting up to d for it (the
// stream needs a moment after the dialog).
func (s *pwStream) waitFirstFrame(d time.Duration) (int, int, error) {
	deadline := time.Now().Add(d)
	for {
		if w, h := s.size(); w > 0 && h > 0 {
			return w, h, nil
		}
		if s.ended() {
			return 0, 0, errors.New("the screen's stream stopped straight away")
		}
		if time.Now().After(deadline) {
			return 0, 0, errors.New("no picture came from the screen")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// portalFeed: the shared picture at w x h, fps a second, to FFmpeg. Each
// fresh picture also goes to the smaller version of the share (tee). When
// it stops, the share's hold on the portal stream is let go (release).
func portalFeed(pc *portalCapture, w, h, fps int, tee *frameTee) screenFeed {
	return func(out io.Writer, stop <-chan struct{}) error {
		defer pc.release()
		frame := make([]byte, w*h*4)
		for i := 3; i < len(frame); i += 4 {
			frame[i] = 255 // black until the first picture
		}
		interval := time.Second / time.Duration(fps)
		timer := time.NewTimer(time.Hour)
		defer timer.Stop()
		start := time.Now()
		fresh, repeats := 0, 0
		statT := start
		for i := 0; ; i++ {
			due := start.Add(time.Duration(i) * interval)
			if d := time.Until(due); d > 0 {
				timer.Reset(d)
				select {
				case <-stop:
					return nil
				case <-timer.C:
				}
			} else {
				select {
				case <-stop:
					return nil
				default:
				}
			}
			switch pc.cap.frame(frame, w, h) {
			case C.PWCAP_FRESH:
				fresh++
				tee.publish(frame)
			case C.PWCAP_ENDED:
				return errors.New("you stopped sharing from the desktop")
			default: // nothing new (a still screen sends nothing): the last picture again
				repeats++
			}
			if _, err := out.Write(frame); err != nil {
				return nil // FFmpeg has stopped
			}
			if el := time.Since(statT); el >= 30*time.Second {
				log.Printf("screen: Wayland stream: %d fresh, %d repeated in %.0fs", fresh, repeats, el.Seconds())
				fresh, repeats, statT = 0, 0, time.Now()
			}
		}
	}
}

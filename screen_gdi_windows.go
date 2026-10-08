//go:build windows

package main

/*
#cgo LDFLAGS: -lgdi32 -luser32
#include "screen_gdi_windows.h"
*/
import "C"

import (
	"errors"
	"io"
	"log"
	"runtime"
	"time"
	"unsafe"
)

// videoArea: the size of the video player's picture inside this window
// (VLC's video area), if it's showing one — see screen_gdi_windows.c.
func videoArea(hwnd uintptr) (w, h int, ok bool) {
	var cw, ch C.int
	if C.gdicap_video_size(C.ulonglong(hwnd), &cw, &ch) == 0 {
		return 0, 0, false
	}
	return int(cw), int(ch), true
}

// gdiWindowFeed: frames of one window, copied without Windows Graphics
// Capture (so with no yellow border on Windows 10 — screen_gdi_windows.c),
// written to FFmpeg as raw BGRA at a steady fps.
//
// FFmpeg times raw frames by counting them, so exactly fps frames go out
// every second: when a copy takes longer than a frame's time, the last
// picture is sent again to catch up, keeping the timing (and the sound
// lined up with it) true.
//
// Each fresh picture is also offered to tee — the smaller version of the
// share, when one is being sent (screen_low.go), takes its frames from
// here rather than copying the window a second time.
func gdiWindowFeed(hwnd uintptr, w, h, fps int, tee *frameTee) screenFeed {
	return gdiFeed(func() *C.gdicap { return C.gdicap_open(C.ulonglong(hwnd), C.int(w), C.int(h)) },
		"window", w, h, fps, tee)
}

// gdiScreenFeed: the same, for a whole monitor (src, in desktop
// coordinates) copied with plain GDI — the last resort when neither
// Desktop Duplication nor Windows Graphics Capture works, as in most
// virtual machines.
func gdiScreenFeed(src winRect, w, h, fps int, tee *frameTee) screenFeed {
	return gdiFeed(func() *C.gdicap {
		return C.gdicap_open_screen(C.int(src.Left), C.int(src.Top),
			C.int(src.Right-src.Left), C.int(src.Bottom-src.Top), C.int(w), C.int(h))
	}, "screen", w, h, fps, tee)
}

func gdiFeed(open func() *C.gdicap, what string, w, h, fps int, tee *frameTee) screenFeed {
	return func(out io.Writer, stop <-chan struct{}) error {
		// GDI device contexts belong to the thread that made them.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		c := open()
		if c == nil {
			return errors.New("couldn't set up copying that " + what)
		}
		defer C.gdicap_close(c)
		frame := unsafe.Slice((*byte)(C.gdicap_bits(c)), w*h*4)

		interval := time.Second / time.Duration(fps)
		timer := time.NewTimer(time.Hour)
		defer timer.Stop()
		start := time.Now()
		fails, copies, repeats := 0, 0, 0
		var spent time.Duration
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
			if time.Since(due) < interval { // on time: a fresh picture
				t0 := time.Now()
				switch C.gdicap_frame(c, 1) {
				case C.GDICAP_OK:
					fails = 0
				case C.GDICAP_GONE:
					return errors.New("the window you were sharing was closed")
				case C.GDICAP_FAIL:
					fails++
					if i == 0 || (what == "window" && fails >= 3*fps) {
						return errors.New("Windows wouldn't copy that " + what)
					}
				} // GDICAP_HIDDEN (minimised): keep sending the last picture
				spent += time.Since(t0)
				copies++
				tee.publish(frame)
			} else {
				repeats++
			}
			if _, err := out.Write(frame); err != nil {
				return nil // FFmpeg has stopped
			}
			if el := time.Since(statT); el >= 30*time.Second {
				avg := time.Duration(0)
				if copies > 0 {
					avg = spent / time.Duration(copies)
				}
				log.Printf("screen: %s copy: %d fresh, %d repeated in %.0fs (%.1f ms per copy)",
					what, copies, repeats, el.Seconds(), float64(avg)/float64(time.Millisecond))
				copies, repeats, spent, statT = 0, 0, 0, time.Now()
			}
		}
	}
}

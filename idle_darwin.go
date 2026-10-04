//go:build darwin

package main

import (
	"errors"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

var errIdleUnsupported = errors.New("idle time unavailable")

var hidIdleRe = regexp.MustCompile(`"HIDIdleTime" = (\d+)`)

// macOS reports nanoseconds since the last input as IOHIDSystem's
// HIDIdleTime; ioreg reads it without needing cgo.
func systemIdle() (time.Duration, error) {
	out, err := exec.Command("ioreg", "-c", "IOHIDSystem", "-d", "4").Output()
	if err != nil {
		return 0, err
	}
	m := hidIdleRe.FindSubmatch(out)
	if m == nil {
		return 0, errIdleUnsupported
	}
	ns, err := strconv.ParseInt(string(m[1]), 10, 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(ns), nil
}

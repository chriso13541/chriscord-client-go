//go:build !windows && !linux && !darwin

package main

import (
	"errors"
	"time"
)

var errIdleUnsupported = errors.New("idle time unavailable")

func systemIdle() (time.Duration, error) { return 0, errIdleUnsupported }

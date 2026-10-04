package main

import (
	"sync"
	"time"
)

// System-wide idle time, for automatic Away.
//
// Away should mean "away from the computer", not "not looking at
// chriscord" — someone playing a game with chriscord in the background is
// still at their desk. So instead of only watching the window, we ask the
// OS how long it's been since the last keyboard/mouse input anywhere
// (systemIdle, one file per platform). Where the OS can't tell us, the
// frontend falls back to activity inside the window.

var (
	idleMu        sync.Mutex
	idleFailUntil time.Time // after a failure, don't ask again until then
)

// GetSystemIdleSeconds returns how long since the last keyboard or mouse
// input anywhere on the desktop, or an error if this desktop can't say.
func (a *App) GetSystemIdleSeconds() (float64, error) {
	idleMu.Lock()
	defer idleMu.Unlock()
	if time.Now().Before(idleFailUntil) {
		return 0, errIdleUnsupported
	}
	d, err := systemIdle()
	if err != nil {
		idleFailUntil = time.Now().Add(time.Minute)
		return 0, err
	}
	return d.Seconds(), nil
}

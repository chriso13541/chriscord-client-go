package main

// activity.go — what shows by your name while you're in a call.
//
// The member list and profile cards show, in this order: your custom
// status; "Sharing <window>" or "Sharing their screen" while you share in
// a call; "Playing <game>"; "In voice" while you're in a call. Who's in a
// call and who's sharing come from the server's voice_state (it knows
// that anyway: it carries the call); the window's title doesn't. It goes
// inside your encrypted status (presence.go, "w"), so the server never
// sees it, only the other members do, like the game you're playing.

import (
	"strings"
	"sync"
)

var sharing struct {
	mu    sync.Mutex
	title string // the shared window's title, "" when sharing a whole screen
	on    bool
}

// sharingStarted: a share began (or switched to another window/screen).
func (a *App) sharingStarted(opts ScreenStart) {
	title := ""
	if strings.HasPrefix(opts.ID, "w:") {
		title = cleanGameName(opts.Title)
	}
	sharing.mu.Lock()
	changed := !sharing.on || sharing.title != title
	sharing.on, sharing.title = true, title
	sharing.mu.Unlock()
	if changed {
		a.presenceChanged()
	}
}

// sharingStopped: the share ended, however it ended.
func (a *App) sharingStopped() {
	sharing.mu.Lock()
	was := sharing.on
	sharing.on, sharing.title = false, ""
	sharing.mu.Unlock()
	if was {
		a.presenceChanged()
	}
}

// sharingWindowTitle is the window being shared right now, or "".
func sharingWindowTitle() string {
	screenMu.Lock()
	active := activeScreen != nil
	screenMu.Unlock()
	sharing.mu.Lock()
	defer sharing.mu.Unlock()
	if !sharing.on || !active {
		return ""
	}
	return sharing.title
}

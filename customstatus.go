package main

// customstatus.go — a custom status: a line of your own (and an emoji)
// shown by your name, like "🍕 grabbing food, back soon". It travels inside
// the encrypted status blob (presence.go), so the server never sees it, and
// it can clear itself after a while. One for all your servers, kept in
// window.json with the other app-wide settings.

import (
	"strings"
	"time"
	"unicode"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// CustomStatus is yours: what it says, its emoji, and when it clears
// (RFC 3339; "" for never).
type CustomStatus struct {
	Text    string `json:"text"`
	Emoji   string `json:"emoji"`
	Expires string `json:"expires"`
}

// CustomStatusView is someone's custom status, as the member list shows it.
type CustomStatusView struct {
	Text  string `json:"text"`
	Emoji string `json:"emoji"`
}

// cleanEmoji: one emoji (or a short sequence), no control characters.
func cleanEmoji(e string) string {
	e = strings.TrimSpace(e)
	if len(e) > 64 {
		return ""
	}
	for _, r := range e {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return ""
		}
	}
	return e
}

// activeCustomStatus is yours, unless there's none or it has run out (then
// it's cleared, and the page told).
func (a *App) activeCustomStatus() *CustomStatus {
	cs := loadWindowPrefs().CustomStatus
	if cs == nil || (cs.Text == "" && cs.Emoji == "") {
		return nil
	}
	if cs.Expires != "" {
		if t, err := time.Parse(time.RFC3339, cs.Expires); err == nil && time.Now().After(t) {
			updateWindowPrefs(func(p *windowPrefs) { p.CustomStatus = nil })
			if a.ctx != nil {
				runtime.EventsEmit(a.ctx, "customstatus:changed", nil)
			}
			return nil
		}
	}
	return cs
}

func (a *App) GetCustomStatus() *CustomStatus { return a.activeCustomStatus() }

// SetCustomStatus sets yours ("" text and emoji clears it). expires is an
// RFC 3339 time, or "" for never.
func (a *App) SetCustomStatus(text, emoji, expires string) (*CustomStatus, error) {
	cs := &CustomStatus{Text: cleanGameName(text), Emoji: cleanEmoji(emoji)}
	if expires != "" {
		if t, err := time.Parse(time.RFC3339, expires); err == nil {
			cs.Expires = t.UTC().Format(time.RFC3339)
		}
	}
	if cs.Text == "" && cs.Emoji == "" {
		cs = nil
	}
	err := updateWindowPrefs(func(p *windowPrefs) { p.CustomStatus = cs })
	a.presenceChanged()
	// Make sure it clears on time even if nothing else is sent then.
	if cs != nil && cs.Expires != "" {
		if t, err := time.Parse(time.RFC3339, cs.Expires); err == nil {
			time.AfterFunc(time.Until(t)+time.Second, func() {
				if a.activeCustomStatus() == nil {
					a.presenceChanged()
				}
			})
		}
	}
	return cs, err
}

// CustomStatusSupported: the connected server carries encrypted statuses,
// which is where a custom status goes.
func (a *App) CustomStatusSupported() bool { return encPresence.Load() }

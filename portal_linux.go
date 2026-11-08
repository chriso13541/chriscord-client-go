//go:build linux

package main

// Sharing on Wayland: the desktop's ScreenCast portal (xdg-desktop-portal —
// on GNOME, xdg-desktop-portal-gnome) shows its own "share your screen"
// dialog, and hands back a PipeWire stream of what was picked
// (pwcapture_linux.go reads it). That's the only way a Wayland program may
// see the screen; it's how browsers, OBS and Discord do it too.
//
// The dialog shows once per share chosen from Chriscord's picker. A share
// that's only restarted — a new resolution or frame rate, picked back up
// after the connection dropped — keeps the same stream (portalCapture
// below) instead of asking again. While a stream is open the desktop shows
// its "you're sharing" indicator; stopping it there ends the share.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	portalDest  = "org.freedesktop.portal.Desktop"
	portalPath  = dbus.ObjectPath("/org/freedesktop/portal/desktop")
	portalCast  = "org.freedesktop.portal.ScreenCast"
	portalReqIf = "org.freedesktop.portal.Request"
)

// portalStream: what the user picked.
type portalStream struct {
	session dbus.ObjectPath
	node    uint32
	fd      int // PipeWire connection, owned by whoever opens the stream
	w, h    int // size, if the portal said
	window  bool
}

// portalAvailable: the ScreenCast portal is there ("" if so, else why not).
func portalAvailable() string {
	if !sessionBusUsable() {
		return "there's no D-Bus session"
	}
	bus, err := dbus.SessionBus()
	if err != nil {
		return "there's no D-Bus session"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var v dbus.Variant
	err = bus.Object(portalDest, portalPath).CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, portalCast, "AvailableSourceTypes").Store(&v)
	if err != nil {
		return "the desktop's screen-sharing service (xdg-desktop-portal) isn't running"
	}
	if t, _ := v.Value().(uint32); t&3 == 0 {
		return "the desktop's screen-sharing service offers nothing to share"
	}
	return ""
}

func portalUint(name string) uint32 {
	bus, err := dbus.SessionBus()
	if err != nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var v dbus.Variant
	if bus.Object(portalDest, portalPath).CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, portalCast, name).Store(&v) != nil {
		return 0
	}
	n, _ := v.Value().(uint32)
	return n
}

func portalToken() string { return fmt.Sprintf("chriscord%d", rand.Uint32()) }

// portalRequest calls a portal method that answers later with a Response
// signal on a Request object, and waits (up to wait) for that answer.
func portalRequest(bus *dbus.Conn, method string, wait time.Duration, args ...interface{}) (map[string]dbus.Variant, error) {
	// Where the answer will come: …/request/<our bus name>/<token>.
	token := portalToken()
	sender := strings.ReplaceAll(strings.TrimPrefix(bus.Names()[0], ":"), ".", "_")
	reqPath := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/" + sender + "/" + token)
	opts := args[len(args)-1].(map[string]dbus.Variant)
	opts["handle_token"] = dbus.MakeVariant(token)

	match := []dbus.MatchOption{dbus.WithMatchObjectPath(reqPath), dbus.WithMatchInterface(portalReqIf), dbus.WithMatchMember("Response")}
	if err := bus.AddMatchSignal(match...); err != nil {
		return nil, err
	}
	defer bus.RemoveMatchSignal(match...)
	ch := make(chan *dbus.Signal, 8)
	bus.Signal(ch)
	defer bus.RemoveSignal(ch)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	var handle dbus.ObjectPath
	err := bus.Object(portalDest, portalPath).CallWithContext(ctx, portalCast+"."+method, 0, args...).Store(&handle)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("the desktop's screen-sharing service failed (%s: %v)", method, err)
	}
	if handle != reqPath { // very old portals pick their own path
		bus.RemoveMatchSignal(match...)
		match[0] = dbus.WithMatchObjectPath(handle)
		bus.AddMatchSignal(match...)
		reqPath = handle
	}
	timeout := time.After(wait)
	for {
		select {
		case sig := <-ch:
			if sig == nil || sig.Path != reqPath || sig.Name != portalReqIf+".Response" || len(sig.Body) < 2 {
				continue
			}
			code, _ := sig.Body[0].(uint32)
			res, _ := sig.Body[1].(map[string]dbus.Variant)
			switch code {
			case 0:
				return res, nil
			case 1:
				return nil, errPortalCancelled
			default:
				return nil, fmt.Errorf("the desktop's screen-sharing service said no (%s)", method)
			}
		case <-timeout:
			bus.Object(portalDest, reqPath).Call("org.freedesktop.portal.Request.Close", 0)
			return nil, errors.New("the screen-sharing dialog wasn't answered")
		}
	}
}

var errPortalCancelled = errors.New("screen sharing was cancelled")

// portalPick shows the desktop's dialog and returns what was picked.
func portalPick() (*portalStream, error) {
	bus, err := dbus.SessionBus()
	if err != nil {
		return nil, err
	}
	res, err := portalRequest(bus, "CreateSession", 30*time.Second, map[string]dbus.Variant{
		"session_handle_token": dbus.MakeVariant(portalToken()),
	})
	if err != nil {
		return nil, err
	}
	var session dbus.ObjectPath
	switch v := res["session_handle"].Value().(type) { // a string in older portals, a path in newer
	case string:
		session = dbus.ObjectPath(v)
	case dbus.ObjectPath:
		session = v
	}
	if !session.IsValid() || session == "/" {
		return nil, errors.New("the desktop's screen-sharing service didn't start a session")
	}
	ok := false
	defer func() {
		if !ok {
			portalClose(session)
		}
	}()

	types := portalUint("AvailableSourceTypes") & 3 // monitors and windows
	if types == 0 {
		types = 1
	}
	sel := map[string]dbus.Variant{
		"types":    dbus.MakeVariant(types),
		"multiple": dbus.MakeVariant(false),
	}
	if portalUint("AvailableCursorModes")&2 != 0 {
		sel["cursor_mode"] = dbus.MakeVariant(uint32(2)) // the pointer drawn into the picture
	}
	if _, err := portalRequest(bus, "SelectSources", 30*time.Second, session, sel); err != nil {
		return nil, err
	}
	// The dialog: as long as it takes them to choose.
	res, err = portalRequest(bus, "Start", 10*time.Minute, session, "", map[string]dbus.Variant{})
	if err != nil {
		return nil, err
	}
	ps := &portalStream{session: session}
	streams, _ := res["streams"].Value().([][]interface{})
	if len(streams) == 0 || len(streams[0]) < 2 {
		return nil, errors.New("nothing was picked to share")
	}
	ps.node, _ = streams[0][0].(uint32)
	if props, ok := streams[0][1].(map[string]dbus.Variant); ok {
		if sz, ok := props["size"].Value().([]interface{}); ok && len(sz) == 2 {
			w, _ := sz[0].(int32)
			h, _ := sz[1].(int32)
			ps.w, ps.h = int(w), int(h)
		}
		if st, ok := props["source_type"].Value().(uint32); ok {
			ps.window = st == 2
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var fd dbus.UnixFD
	if err := bus.Object(portalDest, portalPath).CallWithContext(ctx, portalCast+".OpenPipeWireRemote", 0, session, map[string]dbus.Variant{}).Store(&fd); err != nil {
		return nil, fmt.Errorf("couldn't open the screen's PipeWire stream: %v", err)
	}
	ps.fd = int(fd)
	ok = true
	log.Printf("screen: portal picked node %d (%dx%d, window=%v)", ps.node, ps.w, ps.h, ps.window)
	return ps, nil
}

func portalClose(session dbus.ObjectPath) {
	bus, err := dbus.SessionBus()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bus.Object(portalDest, session).CallWithContext(ctx, "org.freedesktop.portal.Session.Close", 0)
}

// ── Keeping one picked stream across restarts ───────────────────────────

// portalCapture is a picked, open stream. It stays open while a share
// uses it, and for a short while after (portalGrace), so a share that's
// restarted picks it straight back up.
type portalCapture struct {
	ps    *portalStream
	cap   *pwStream
	users int
	timer *time.Timer
	dead  bool
}

const portalGrace = 15 * time.Second

var portalCap struct {
	mu    sync.Mutex
	cur   *portalCapture
	fresh bool // the picker was opened: the next share asks again
}

// portalNewChoice: the share picker opened — whatever's shared next is a
// new choice, so the dialog will show.
func portalNewChoice() {
	portalCap.mu.Lock()
	portalCap.fresh = true
	if c := portalCap.cur; c != nil && c.users == 0 {
		portalCap.cur = nil
		c.close()
	}
	portalCap.mu.Unlock()
}

// portalAcquire: the open stream to share — the one already open, unless
// the user is choosing anew; else the dialog.
func portalAcquire(fps int) (*portalCapture, error) {
	portalCap.mu.Lock()
	if c := portalCap.cur; c != nil && !portalCap.fresh && !c.dead && !c.cap.ended() {
		if c.timer != nil {
			c.timer.Stop()
			c.timer = nil
		}
		c.users++
		portalCap.mu.Unlock()
		log.Printf("screen: reusing the stream picked earlier")
		return c, nil
	}
	if old := portalCap.cur; old != nil {
		portalCap.cur = nil
		old.dead = true
		if old.users == 0 {
			old.close()
		} // else: closed when its last user lets go
	}
	portalCap.fresh = false
	portalCap.mu.Unlock()

	ps, err := portalPick()
	if err != nil {
		return nil, err
	}
	cap, err := openPWStream(ps.fd, ps.node, fps)
	if err != nil {
		portalClose(ps.session)
		return nil, err
	}
	c := &portalCapture{ps: ps, cap: cap, users: 1}
	portalCap.mu.Lock()
	portalCap.cur = c
	portalCap.mu.Unlock()
	go c.watchClosed()
	return c, nil
}

// release: a share stopped using it.
func (c *portalCapture) release() {
	portalCap.mu.Lock()
	defer portalCap.mu.Unlock()
	c.users--
	if c.users > 0 {
		return
	}
	if c.dead || portalCap.cur != c || c.cap.ended() {
		if portalCap.cur == c {
			portalCap.cur = nil
		}
		c.close()
		return
	}
	c.timer = time.AfterFunc(portalGrace, func() {
		portalCap.mu.Lock()
		defer portalCap.mu.Unlock()
		if c.users == 0 && portalCap.cur == c {
			portalCap.cur = nil
			c.close()
		}
	})
}

// close: with portalCap.mu held (or c unreachable).
func (c *portalCapture) close() {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.dead = true
	c.cap.close()
	go portalClose(c.ps.session)
}

// watchClosed: the session ended from the desktop's side (its "stop
// sharing" button): the stream won't be reused.
func (c *portalCapture) watchClosed() {
	bus, err := dbus.SessionBus()
	if err != nil {
		return
	}
	match := []dbus.MatchOption{dbus.WithMatchObjectPath(c.ps.session), dbus.WithMatchInterface("org.freedesktop.portal.Session"), dbus.WithMatchMember("Closed")}
	if bus.AddMatchSignal(match...) != nil {
		return
	}
	defer bus.RemoveMatchSignal(match...)
	ch := make(chan *dbus.Signal, 4)
	bus.Signal(ch)
	defer bus.RemoveSignal(ch)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case sig := <-ch:
			if sig != nil && sig.Path == c.ps.session && sig.Name == "org.freedesktop.portal.Session.Closed" {
				log.Printf("screen: sharing was stopped from the desktop")
				c.cap.markEnded()
				portalCap.mu.Lock()
				c.dead = true
				portalCap.mu.Unlock()
				return
			}
		case <-tick.C:
			portalCap.mu.Lock()
			gone := c.dead && c.users == 0
			portalCap.mu.Unlock()
			if gone {
				return
			}
		}
	}
}

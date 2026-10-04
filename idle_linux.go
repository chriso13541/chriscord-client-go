//go:build linux

package main

import (
	"errors"
	"os"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/jezek/xgb"
	"github.com/jezek/xgb/screensaver"
	"github.com/jezek/xgb/xproto"
)

var errIdleUnsupported = errors.New("idle time unavailable on this desktop")

// Linux has no single answer, so try in order:
//
//  1. GNOME (Wayland or X11): Mutter's IdleMonitor over D-Bus.
//  2. Any X11 session: the MIT-SCREEN-SAVER extension.
//
// X11 is skipped on Wayland: through XWayland it only sees input sent to
// X apps, so it would call you idle while you type in a native Wayland
// window. Other Wayland compositors (KDE, sway, Hyprland…) only offer the
// ext-idle-notify protocol, which isn't wired up yet — those fall back to
// in-window activity.

var x11Conn *xgb.Conn // kept open between calls; reset if it breaks

func systemIdle() (time.Duration, error) {
	if d, err := mutterIdle(); err == nil {
		return d, nil
	}
	if isWayland() {
		return 0, errIdleUnsupported
	}
	return x11Idle()
}

func isWayland() bool {
	return os.Getenv("WAYLAND_DISPLAY") != "" ||
		strings.EqualFold(os.Getenv("XDG_SESSION_TYPE"), "wayland")
}

func mutterIdle() (time.Duration, error) {
	conn, err := dbus.SessionBus() // shared connection: never Close it
	if err != nil {
		return 0, err
	}
	var ms uint64
	err = conn.Object("org.gnome.Mutter.IdleMonitor", "/org/gnome/Mutter/IdleMonitor/Core").
		Call("org.gnome.Mutter.IdleMonitor.GetIdletime", 0).Store(&ms)
	if err != nil {
		return 0, err
	}
	return time.Duration(ms) * time.Millisecond, nil
}

func x11Idle() (time.Duration, error) {
	if os.Getenv("DISPLAY") == "" {
		return 0, errIdleUnsupported
	}
	if x11Conn == nil {
		c, err := xgb.NewConn()
		if err != nil {
			return 0, err
		}
		if err := screensaver.Init(c); err != nil {
			c.Close()
			return 0, err
		}
		x11Conn = c
	}
	root := xproto.Setup(x11Conn).DefaultScreen(x11Conn).Root
	reply, err := screensaver.QueryInfo(x11Conn, xproto.Drawable(root)).Reply()
	if err != nil {
		x11Conn.Close()
		x11Conn = nil
		return 0, err
	}
	return time.Duration(reply.MsSinceUserInput) * time.Millisecond, nil
}

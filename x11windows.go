//go:build linux

package main

// The application windows that can be shared from an X11 session: the ones
// the window manager lists (EWMH _NET_CLIENT_LIST — every common window
// manager and desktop keeps it), front to back, leaving out Chriscord's own
// window, docks, panels, menus and anything that asks to stay off the
// taskbar. A small picture of each comes straight from the X server.
//
// Without a window manager that keeps that list (bare X, twm…), the
// top-level windows with a title are used instead.
//
// A window that's covered by others shows them in its picture unless a
// compositor is running (picom, or the desktop's own): plain X keeps no
// copy of what's hidden. Minimised windows aren't drawn at all, so they
// have no picture and can't be shared until they're brought back up.

import (
	"errors"
	"image"
	"os"
	"strings"
	"sync"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

type xWindow struct {
	id     uint32
	title  string
	class  string // WM_CLASS, e.g. "firefox"
	w, h   int
	hidden bool // minimised
	thumb  string
}

// xAtoms are looked up once per connection.
type xAtoms struct {
	clientList, clientListStacking, activeWindow, wmName, utf8, wmState,
	stateHidden, stateSkipTaskbar, wmType, typeNormal, typeDialog, wmPid xproto.Atom
}

func xAtom(c *xgb.Conn, name string) xproto.Atom {
	r, err := xproto.InternAtom(c, true, uint16(len(name)), name).Reply()
	if err != nil || r == nil {
		return xproto.AtomNone
	}
	return r.Atom
}

func loadXAtoms(c *xgb.Conn) xAtoms {
	return xAtoms{
		clientList:         xAtom(c, "_NET_CLIENT_LIST"),
		clientListStacking: xAtom(c, "_NET_CLIENT_LIST_STACKING"),
		activeWindow:       xAtom(c, "_NET_ACTIVE_WINDOW"),
		wmName:             xAtom(c, "_NET_WM_NAME"),
		utf8:               xAtom(c, "UTF8_STRING"),
		wmState:            xAtom(c, "_NET_WM_STATE"),
		stateHidden:        xAtom(c, "_NET_WM_STATE_HIDDEN"),
		stateSkipTaskbar:   xAtom(c, "_NET_WM_STATE_SKIP_TASKBAR"),
		wmType:             xAtom(c, "_NET_WM_WINDOW_TYPE"),
		typeNormal:         xAtom(c, "_NET_WM_WINDOW_TYPE_NORMAL"),
		typeDialog:         xAtom(c, "_NET_WM_WINDOW_TYPE_DIALOG"),
		wmPid:              xAtom(c, "_NET_WM_PID"),
	}
}

func xProp(c *xgb.Conn, win xproto.Window, atom xproto.Atom) *xproto.GetPropertyReply {
	if atom == xproto.AtomNone {
		return nil
	}
	r, err := xproto.GetProperty(c, false, win, atom, xproto.GetPropertyTypeAny, 0, 1<<16).Reply()
	if err != nil || r == nil || r.Format == 0 || r.ValueLen == 0 {
		return nil
	}
	return r
}

func xProp32(c *xgb.Conn, win xproto.Window, atom xproto.Atom) []uint32 {
	r := xProp(c, win, atom)
	if r == nil || r.Format != 32 {
		return nil
	}
	vals := make([]uint32, 0, r.ValueLen)
	for i := 0; i+4 <= len(r.Value) && len(vals) < int(r.ValueLen); i += 4 {
		vals = append(vals, xgb.Get32(r.Value[i:]))
	}
	return vals
}

func xPropString(c *xgb.Conn, win xproto.Window, atom xproto.Atom) string {
	r := xProp(c, win, atom)
	if r == nil || r.Format != 8 {
		return ""
	}
	return strings.TrimRight(string(r.Value), "\x00")
}

func hasAtom(list []uint32, a xproto.Atom) bool {
	for _, v := range list {
		if xproto.Atom(v) == a && a != xproto.AtomNone {
			return true
		}
	}
	return false
}

func xTitle(c *xgb.Conn, at xAtoms, win xproto.Window) string {
	if t := xPropString(c, win, at.wmName); t != "" {
		return t
	}
	return xPropString(c, win, xproto.AtomWmName)
}

// xClass: the second WM_CLASS string ("Navigator\0firefox\0" → "firefox").
func xClass(c *xgb.Conn, win xproto.Window) string {
	parts := strings.Split(xPropString(c, win, xproto.AtomWmClass), "\x00")
	for i := len(parts) - 1; i >= 0; i-- {
		if p := strings.TrimSpace(parts[i]); p != "" {
			return strings.ToLower(p)
		}
	}
	return ""
}

func xHidden(c *xgb.Conn, at xAtoms, win xproto.Window) bool {
	if hasAtom(xProp32(c, win, at.wmState), at.stateHidden) {
		return true
	}
	attr, err := xproto.GetWindowAttributes(c, win).Reply()
	return err != nil || attr.MapState != xproto.MapStateViewable
}

// xWindows lists the windows that can be shared, front to back.
func xWindows() ([]xWindow, error) {
	if xDisplay() == "" {
		return nil, errors.New("no X display")
	}
	c, err := xgb.NewConnDisplay(xDisplay())
	if err != nil {
		return nil, err
	}
	defer c.Close()
	at := loadXAtoms(c)
	root := xproto.Setup(c).DefaultScreen(c).Root

	var ids []uint32
	ewmh := true
	if ids = xProp32(c, root, at.clientListStacking); len(ids) > 0 {
		// bottom to top: turn it around so the front window comes first
		for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
			ids[i], ids[j] = ids[j], ids[i]
		}
	} else if ids = xProp32(c, root, at.clientList); len(ids) == 0 {
		ewmh = false
		tree, err := xproto.QueryTree(c, root).Reply()
		if err != nil {
			return nil, err
		}
		for i := len(tree.Children) - 1; i >= 0; i-- { // top first
			ids = append(ids, uint32(tree.Children[i]))
		}
	}

	self := uint32(os.Getpid())
	var out []xWindow
	for _, id := range ids {
		win := xproto.Window(id)
		if pid := xProp32(c, win, at.wmPid); len(pid) > 0 && pid[0] == self {
			continue // Chriscord's own window
		}
		if ewmh {
			if hasAtom(xProp32(c, win, at.wmState), at.stateSkipTaskbar) {
				continue
			}
			if types := xProp32(c, win, at.wmType); len(types) > 0 && !hasAtom(types, at.typeNormal) && !hasAtom(types, at.typeDialog) {
				continue // docks, panels, desktop, menus, tooltips…
			}
		}
		title := strings.TrimSpace(xTitle(c, at, win))
		if title == "" {
			continue
		}
		geo, err := xproto.GetGeometry(c, xproto.Drawable(win)).Reply()
		if err != nil || geo.Width < 64 || geo.Height < 48 {
			continue
		}
		hidden := xHidden(c, at, win)
		if !ewmh && hidden {
			continue // without a window manager's list, unmapped windows are mostly helpers
		}
		w := xWindow{id: id, title: title, class: xClass(c, win), w: int(geo.Width), h: int(geo.Height), hidden: hidden}
		out = append(out, w)
	}

	// Pictures: one GetImage each, in parallel on their own connections
	// would be faster, but X serialises them anyway; a few dozen windows
	// take well under a second.
	for i := range out {
		if !out[i].hidden {
			out[i].thumb = xWindowThumb(c, xproto.Window(out[i].id), out[i].w, out[i].h)
		}
	}
	return out, nil
}

// xWindowThumb: a 320-pixel-wide picture of a window, as a data: URL ("" if
// the X server won't give one — a minimised window, an unusual depth).
func xWindowThumb(c *xgb.Conn, win xproto.Window, w, h int) string {
	if w <= 0 || h <= 0 || w > 8192 || h > 8192 {
		return ""
	}
	img, err := xproto.GetImage(c, xproto.ImageFormatZPixmap, xproto.Drawable(win), 0, 0, uint16(w), uint16(h), 0xffffffff).Reply()
	if err != nil || img == nil || len(img.Data) < w*h*4 || (img.Depth != 24 && img.Depth != 32) {
		return ""
	}
	const tw = 320
	scaleW := tw
	if w < tw {
		scaleW = w
	}
	th := h * scaleW / w
	if th <= 0 {
		return ""
	}
	// Each picture pixel is the average of the block of window pixels it
	// covers. X gives 32-bit pixels, blue first (LSB-first, as every PC
	// X server is).
	dst := image.NewRGBA(image.Rect(0, 0, scaleW, th))
	src := img.Data
	stride := w * 4
	for ty := 0; ty < th; ty++ {
		y0, y1 := ty*h/th, (ty+1)*h/th
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for tx := 0; tx < scaleW; tx++ {
			x0, x1 := tx*w/scaleW, (tx+1)*w/scaleW
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, b, n int
			for y := y0; y < y1; y++ {
				row := src[y*stride:]
				for x := x0; x < x1; x++ {
					p := row[x*4:]
					b += int(p[0])
					g += int(p[1])
					r += int(p[2])
					n++
				}
			}
			o := dst.PixOffset(tx, ty)
			dst.Pix[o], dst.Pix[o+1], dst.Pix[o+2], dst.Pix[o+3] = uint8(r/n), uint8(g/n), uint8(b/n), 255
		}
	}
	return jpegDataURL(dst)
}

func xWindowSize(id uint32) (int, int, error) {
	c, err := xgb.NewConnDisplay(xDisplay())
	if err != nil {
		return 0, 0, err
	}
	defer c.Close()
	geo, err := xproto.GetGeometry(c, xproto.Drawable(id)).Reply()
	if err != nil {
		return 0, 0, errors.New("that window has closed")
	}
	return int(geo.Width), int(geo.Height), nil
}

// The window share's state is checked once a second (screen.go): one
// connection is kept for that, and reopened if it breaks.
var (
	xStateMu    sync.Mutex
	xStateConn  *xgb.Conn
	xStateAtoms xAtoms
)

// xWindowState: "focused", "background", "minimized" or "closed" (the same
// words the Windows version uses).
func xWindowState(id uint32) string {
	xStateMu.Lock()
	defer xStateMu.Unlock()
	if xStateConn == nil {
		c, err := xgb.NewConnDisplay(xDisplay())
		if err != nil {
			return "background"
		}
		xStateConn, xStateAtoms = c, loadXAtoms(c)
	}
	c, at := xStateConn, xStateAtoms
	win := xproto.Window(id)
	attr, err := xproto.GetWindowAttributes(c, win).Reply()
	if err != nil {
		if _, ok := err.(xproto.WindowError); ok {
			return "closed"
		}
		c.Close() // the connection itself broke: open a new one next time
		xStateConn = nil
		return "background"
	}
	if attr.MapState != xproto.MapStateViewable || hasAtom(xProp32(c, win, at.wmState), at.stateHidden) {
		return "minimized"
	}
	root := xproto.Setup(c).DefaultScreen(c).Root
	if act := xProp32(c, root, at.activeWindow); len(act) > 0 && act[0] != 0 {
		if act[0] == id {
			return "focused"
		}
		// A dialog of the shared app counts as the app being in front.
		if owner := xProp32(c, xproto.Window(act[0]), xproto.AtomWmTransientFor); len(owner) > 0 && owner[0] == id {
			return "focused"
		}
	}
	return "background"
}

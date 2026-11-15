//go:build linux

package main

// Pasting pictures and files on Linux. WebKitGTK's paste event only
// carries text: a copied picture (from a browser, a screenshot tool, an
// image viewer) or a copied file (from the file manager) never shows up in
// it as a file. So when a paste has no text, or only file paths, the page
// asks here instead (ClipboardAttachments, clipboard.go), and this reads
// the clipboard straight from the X server: the list of formats it's
// offered in (TARGETS), then the file list (text/uri-list) or the picture
// (PNG preferred) — following X's INCR transfer for big ones.
//
// On a Wayland session this goes through XWayland, whose clipboard the
// desktop keeps in step with Wayland's (GNOME, KDE and wlroots desktops
// all do); wl-paste is tried first there when it's installed.

import (
	"bytes"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

const clipMaxBytes = 64 << 20 // a pasted picture bigger than this is ignored

var clipImageTypes = []string{"image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp"}

func readClipboardAttachments() (ClipboardFiles, error) {
	if onWayland() {
		if out, ok := wlPasteAttachments(); ok {
			return out, nil
		}
	}
	if xDisplay() == "" {
		return ClipboardFiles{}, errors.New("no X display")
	}
	return x11ClipboardAttachments()
}

// wlPasteAttachments: the same through wl-paste (wl-clipboard), if it's
// installed. ok false: it isn't, or it failed.
func wlPasteAttachments() (ClipboardFiles, bool) {
	if _, err := exec.LookPath("wl-paste"); err != nil {
		return ClipboardFiles{}, false
	}
	types, err := runFor(2*time.Second, "wl-paste", "--no-newline", "--list-types")
	if err != nil {
		return ClipboardFiles{}, false
	}
	have := map[string]bool{}
	for _, t := range strings.Split(string(types), "\n") {
		have[strings.TrimSpace(t)] = true
	}
	var out ClipboardFiles
	if have["text/uri-list"] {
		b, err := runFor(3*time.Second, "wl-paste", "--no-newline", "--type", "text/uri-list")
		if err == nil {
			out.Paths = fileURIs(string(b))
		}
		if len(out.Paths) > 0 {
			return out, true
		}
	}
	for _, t := range clipImageTypes {
		if !have[t] {
			continue
		}
		b, err := runFor(10*time.Second, "wl-paste", "--no-newline", "--type", t)
		if err == nil && len(b) > 0 && len(b) <= clipMaxBytes {
			out.Image, out.Mime = b, t
			return out, true
		}
	}
	return out, true // nothing to attach (but wl-paste answered)
}

func runFor(d time.Duration, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	t := time.AfterFunc(d, func() { cmd.Process.Kill() })
	err := cmd.Wait()
	t.Stop()
	return buf.Bytes(), err
}

// fileURIs: the local files in a text/uri-list (or GNOME's copied-files
// list, whose first line is "copy" or "cut").
func fileURIs(list string) []string {
	var paths []string
	for _, line := range strings.Split(list, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "file://") {
			continue
		}
		u, err := url.Parse(line)
		if err != nil || (u.Host != "" && u.Host != "localhost") {
			continue
		}
		if fi, err := os.Stat(u.Path); err == nil && fi.Mode().IsRegular() {
			paths = append(paths, u.Path)
		}
	}
	return paths
}

// ── X11 ──────────────────────────────────────────────────────────────────

type x11Clip struct {
	c    *xgb.Conn
	win  xproto.Window
	prop xproto.Atom
	sel  xproto.Atom
	incr xproto.Atom
	evs  chan xgb.Event // the connection's events, until it closes
}

func x11ClipboardAttachments() (ClipboardFiles, error) {
	c, err := xgb.NewConnDisplay(xDisplay())
	if err != nil {
		return ClipboardFiles{}, err
	}
	defer c.Close()
	scr := xproto.Setup(c).DefaultScreen(c)
	win, err := xproto.NewWindowId(c)
	if err != nil {
		return ClipboardFiles{}, err
	}
	// An invisible window to receive the clipboard into.
	if err := xproto.CreateWindowChecked(c, scr.RootDepth, win, scr.Root, 0, 0, 1, 1, 0,
		xproto.WindowClassInputOutput, scr.RootVisual,
		xproto.CwEventMask, []uint32{xproto.EventMaskPropertyChange}).Check(); err != nil {
		return ClipboardFiles{}, err
	}
	defer xproto.DestroyWindow(c, win)

	x := &x11Clip{c: c, win: win, evs: make(chan xgb.Event, 64)}
	go func() {
		defer close(x.evs)
		for {
			ev, err := c.WaitForEvent()
			if ev == nil && err == nil {
				return // closed
			}
			if ev != nil {
				x.evs <- ev
			}
		}
	}()
	x.sel = x.atom("CLIPBOARD")
	x.prop = x.atom("CHRISCORD_CLIP")
	x.incr = x.atom("INCR")

	targets, _, err := x.fetch(x.atom("TARGETS"))
	if err != nil {
		return ClipboardFiles{}, err
	}
	have := map[string]bool{}
	for i := 0; i+4 <= len(targets); i += 4 {
		a := xproto.Atom(xgb.Get32(targets[i:]))
		if r, err := xproto.GetAtomName(c, a).Reply(); err == nil {
			have[r.Name] = true
		}
	}

	var out ClipboardFiles
	for _, t := range []string{"text/uri-list", "x-special/gnome-copied-files"} {
		if !have[t] {
			continue
		}
		if b, _, err := x.fetch(x.atom(t)); err == nil {
			out.Paths = fileURIs(string(b))
		}
		if len(out.Paths) > 0 {
			return out, nil
		}
	}
	for _, t := range clipImageTypes {
		if !have[t] {
			continue
		}
		if b, _, err := x.fetch(x.atom(t)); err == nil && len(b) > 0 {
			out.Image, out.Mime = b, t
			return out, nil
		}
	}
	return out, nil
}

func (x *x11Clip) atom(name string) xproto.Atom {
	r, err := xproto.InternAtom(x.c, false, uint16(len(name)), name).Reply()
	if err != nil {
		return xproto.AtomNone
	}
	return r.Atom
}

// fetch asks the clipboard's owner for it as target, and returns what it
// sent (and its type).
func (x *x11Clip) fetch(target xproto.Atom) ([]byte, xproto.Atom, error) {
	if target == xproto.AtomNone {
		return nil, 0, errors.New("unknown format")
	}
	xproto.DeleteProperty(x.c, x.win, x.prop)
	xproto.ConvertSelection(x.c, x.win, x.sel, target, x.prop, xproto.TimeCurrentTime)
	deadline := time.Now().Add(3 * time.Second)
	for {
		ev, err := x.wait(deadline)
		if err != nil {
			return nil, 0, err
		}
		sn, ok := ev.(xproto.SelectionNotifyEvent)
		if !ok {
			continue
		}
		if sn.Property == xproto.AtomNone {
			return nil, 0, errors.New("the clipboard's owner didn't give it in that format")
		}
		break
	}
	data, typ, err := x.readProp(true)
	if err != nil {
		return nil, 0, err
	}
	if typ != x.incr {
		return data, typ, nil
	}
	// INCR: it comes in pieces, each put in the property after we delete
	// the last; an empty piece ends it.
	var all []byte
	deadline = time.Now().Add(15 * time.Second)
	for {
		ev, err := x.wait(deadline)
		if err != nil {
			return nil, 0, err
		}
		pn, ok := ev.(xproto.PropertyNotifyEvent)
		if !ok || pn.Atom != x.prop || pn.State != xproto.PropertyNewValue {
			continue
		}
		piece, t, err := x.readProp(true)
		if err != nil {
			return nil, 0, err
		}
		if len(piece) == 0 {
			return all, t, nil
		}
		if len(all)+len(piece) > clipMaxBytes {
			return nil, 0, errors.New("too big")
		}
		all = append(all, piece...)
		typ = t
	}
}

// readProp reads the whole property (deleting it after, if del).
func (x *x11Clip) readProp(del bool) ([]byte, xproto.Atom, error) {
	var out []byte
	var typ xproto.Atom
	var offset uint32
	for {
		r, err := xproto.GetProperty(x.c, false, x.win, x.prop, xproto.GetPropertyTypeAny, offset, 1<<20).Reply()
		if err != nil {
			return nil, 0, err
		}
		typ = r.Type
		out = append(out, r.Value...)
		if len(out) > clipMaxBytes {
			return nil, 0, errors.New("too big")
		}
		if r.BytesAfter == 0 {
			break
		}
		offset += uint32(len(r.Value)) / 4
	}
	if del {
		xproto.DeleteProperty(x.c, x.win, x.prop)
	}
	return out, typ, nil
}

// wait: the next X event, or an error at the deadline.
func (x *x11Clip) wait(deadline time.Time) (xgb.Event, error) {
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case ev, ok := <-x.evs:
		if !ok {
			return nil, errors.New("X connection closed")
		}
		return ev, nil
	case <-t.C:
		return nil, errors.New("the clipboard didn't answer")
	}
}

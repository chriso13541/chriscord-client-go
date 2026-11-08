//go:build linux

package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Sharing from Linux goes through X11, with FFmpeg's X11 capture
// (xcbgrab, "-f x11grab"):
//
//   - A screen: each monitor on its own — found with xrandr, cut out of the
//     X screen by its position — or, without xrandr, the whole X screen.
//   - A window: any application window the window manager lists
//     (x11windows.go), captured by its X window id, so it's just that
//     window — no frame, nothing over it — wherever it moves.
//   - Its sound: everything the computer plays, from PulseAudio/PipeWire's
//     monitor of the output (sysaudio_linux.go). Linux can't leave
//     Chriscord itself out of that the way Windows can, so the call's own
//     voices are taken back out of it (echo.go). A window's share sends
//     everything you hear too, not just that app.
//
// Wayland doesn't let programs capture the screen themselves: there the
// desktop's own "share your screen" dialog picks a screen or window, and
// the picture comes over PipeWire (portal_linux.go, pwcapture_linux.go).
// Chriscord's picker then shows a single card that opens that dialog.
// CHRISCORD_X11_CAPTURE=1 shares through XWayland instead, with
// Chriscord's own picker: only programs running on XWayland can be seen
// then (most games through Wine/Proton), native Wayland windows come out
// black.

func init() {
	screenAudioNoteText = "— everything you hear (the call's own sound is taken back out)"
}

func screenWindowsSupported() bool  { return !portalMode() }
func screenGDIFallback(string) bool { return false }

// sharedWindowState: for a window share, whether it's in front, behind
// others, minimised or gone (screen.go warns the sharer and the watchers).
func sharedWindowState(id string) string {
	if win, ok := parseXWindowID(id); ok {
		return xWindowState(win)
	}
	return ""
}

// screenAudioTarget: Linux records everything the computer plays, for a
// screen and a window alike (see the top of this file).
func screenAudioTarget(id string) (uint32, bool, error) {
	return uint32(os.Getpid()), true, nil
}

// xDisplay: the X display to capture ("" if there isn't one).
func xDisplay() string { return os.Getenv("DISPLAY") }

func onWayland() bool {
	return strings.EqualFold(os.Getenv("XDG_SESSION_TYPE"), "wayland") || os.Getenv("WAYLAND_DISPLAY") != ""
}

// xCaptureOnWayland: CHRISCORD_X11_CAPTURE=1 — share through XWayland.
func xCaptureOnWayland() bool { return os.Getenv("CHRISCORD_X11_CAPTURE") == "1" }

// portalMode: sharing goes through the desktop's dialog (Wayland).
func portalMode() bool { return onWayland() && !xCaptureOnWayland() }

// portalShareID: the picker's one card on Wayland.
const portalShareID = "portal"

func screenShareUnavailable() string {
	if findFFmpeg() == "" {
		return "FFmpeg isn't available"
	}
	in := ffmpegInputs()
	if !strings.Contains(in["flv"], "E") {
		return "this FFmpeg was built without the FLV muxer — rebuild it with ffmpeg/build-ffmpeg.sh linux"
	}
	if portalMode() {
		if !strings.Contains(in["rawvideo"], "D") {
			return "this FFmpeg can't read raw video, which sharing on Wayland needs — install your distro's FFmpeg (e.g. sudo apt install ffmpeg), or rebuild the bundled one with ffmpeg/build-ffmpeg.sh linux"
		}
		if why := pwStreamAvailable(); why != "" {
			return "Sharing your screen on Wayland needs PipeWire: " + why
		}
		if why := portalAvailable(); why != "" {
			return "Sharing your screen on Wayland goes through the desktop's screen sharing, but " + why +
				" (install xdg-desktop-portal and the one for your desktop, e.g. xdg-desktop-portal-gnome)"
		}
		return ""
	}
	if xDisplay() == "" {
		return "There's no X11 display to share (DISPLAY isn't set)"
	}
	if !strings.Contains(in["x11grab"], "D") {
		return "this FFmpeg can't capture X11 screens — rebuild it with ffmpeg/build-ffmpeg.sh linux"
	}
	return ""
}

// xMonitor is one monitor: where it is on the X screen.
type xMonitor struct {
	name       string
	w, h, x, y int
	primary    bool
}

// " 0: +*DP-1 2560/597x1440/336+0+0  DP-1"
var reXrandrMon = regexp.MustCompile(`^\s*\d+:\s+\+?(\*?)(\S+)\s+(\d+)/\d+x(\d+)/\d+([+-]\d+)([+-]\d+)`)

// xMonitors: the monitors as xrandr lists them, left to right; nil without
// xrandr (then the whole X screen is shared).
func xMonitors() []xMonitor {
	out, err := exec.Command("xrandr", "--listmonitors").Output()
	if err != nil {
		return nil
	}
	var mons []xMonitor
	for _, line := range strings.Split(string(out), "\n") {
		m := reXrandrMon.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		w, _ := strconv.Atoi(m[3])
		h, _ := strconv.Atoi(m[4])
		x, _ := strconv.Atoi(m[5])
		y, _ := strconv.Atoi(m[6])
		if w > 0 && h > 0 {
			mons = append(mons, xMonitor{name: m[2], w: w, h: h, x: x, y: y, primary: m[1] == "*"})
		}
	}
	// Left to right, then top to bottom (as on Windows).
	for i := 1; i < len(mons); i++ {
		for j := i; j > 0 && (mons[j].x < mons[j-1].x || mons[j].x == mons[j-1].x && mons[j].y < mons[j-1].y); j-- {
			mons[j], mons[j-1] = mons[j-1], mons[j]
		}
	}
	return mons
}

var reXSize = regexp.MustCompile(`, (\d{2,5})x(\d{2,5})[,\s]`)

// xScreenSize: the whole X screen's size, as FFmpeg sees it.
func xScreenSize() (int, int, error) {
	// FFmpeg describes the input before complaining there's no output.
	out, _ := runFFmpegFor(5*time.Second, "-hide_banner", "-f", "x11grab", "-i", xDisplay())
	if m := reXSize.FindSubmatch(out); m != nil {
		w, _ := strconv.Atoi(string(m[1]))
		h, _ := strconv.Atoi(string(m[2]))
		return w, h, nil
	}
	return 0, 0, fmt.Errorf("FFmpeg couldn't open the X display %s: %s", xDisplay(), lastLine(out))
}

// runFFmpegFor runs FFmpeg for at most d, returning everything it printed.
func runFFmpegFor(d time.Duration, args ...string) ([]byte, error) {
	cmd := ffmpegCmd(args...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	t := time.AfterFunc(d, func() { cmd.Process.Kill() })
	err := cmd.Wait()
	t.Stop()
	return buf.Bytes(), err
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// Share ids: "x:<w>x<h>+<x>+<y>" (a part of the X screen: a monitor, or
// all of it), or "xw:<window id>" (one application window).
func xShareID(w, h, x, y int) string { return fmt.Sprintf("x:%dx%d+%d+%d", w, h, x, y) }

func parseXShareID(id string) (w, h, x, y int, err error) {
	if !strings.HasPrefix(id, "x:") {
		return 0, 0, 0, 0, errors.New("that isn't a screen this app knows")
	}
	if _, err = fmt.Sscanf(id, "x:%dx%d+%d+%d", &w, &h, &x, &y); err != nil || w <= 0 || h <= 0 {
		return 0, 0, 0, 0, errors.New("that isn't a screen this app knows")
	}
	return w, h, x, y, nil
}

func xWindowShareID(win uint32) string { return "xw:" + strconv.FormatUint(uint64(win), 10) }

func parseXWindowID(id string) (uint32, bool) {
	rest, ok := strings.CutPrefix(id, "xw:")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(rest, 10, 32)
	return uint32(n), err == nil && n != 0
}

func listShareSources() ([]ShareSource, error) {
	if portalMode() {
		// The picker opened: what's shared next is a new choice, made in
		// the desktop's own dialog.
		portalNewChoice()
		return []ShareSource{{ID: portalShareID, Kind: "screen", Name: "Screen or window",
			App: "You'll pick it in the desktop's dialog next", Primary: true}}, nil
	}
	var out []ShareSource
	mons := xMonitors()
	if len(mons) == 0 {
		w, h, err := xScreenSize()
		if err != nil {
			return nil, err
		}
		mons = []xMonitor{{name: "Screen", w: w, h: h, primary: true}}
	}
	for i, m := range mons {
		name := fmt.Sprintf("Screen %d", i+1)
		if len(mons) == 1 {
			name = "Your Screen"
		}
		out = append(out, ShareSource{ID: xShareID(m.w, m.h, m.x, m.y), Kind: "screen", Name: name, App: m.name,
			W: m.w, H: m.h, Primary: m.primary, Index: i, Thumb: xThumb(m)})
	}
	wins, err := xWindows()
	if err != nil {
		log.Printf("screen: couldn't list X windows: %v", err) // screens still work
	}
	for i, w := range wins {
		s := ShareSource{ID: xWindowShareID(w.id), Kind: "window", Name: w.title, App: w.class, W: w.w, H: w.h, Index: i, Thumb: w.thumb}
		if w.hidden {
			s.Note = "Minimized"
		}
		out = append(out, s)
	}
	return out, nil
}

// xThumb: a small picture of a monitor, as a data: URL ("" if FFmpeg
// can't make one — it needs the rawvideo writer, see build-ffmpeg.sh).
func xThumb(m xMonitor) string {
	const tw = 320
	th := (tw*m.h/m.w + 1) &^ 1
	if th <= 0 {
		return ""
	}
	cmd := ffmpegCmd("-hide_banner", "-loglevel", "error", "-f", "x11grab", "-video_size", fmt.Sprintf("%dx%d", m.w, m.h),
		"-i", fmt.Sprintf("%s+%d,%d", xDisplay(), m.x, m.y), "-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:%d:flags=bilinear", tw, th), "-pix_fmt", "rgba", "-f", "rawvideo", "pipe:1")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	if err := cmd.Start(); err != nil {
		return ""
	}
	t := time.AfterFunc(3*time.Second, func() { cmd.Process.Kill() })
	err := cmd.Wait()
	t.Stop()
	if err != nil || buf.Len() < tw*th*4 {
		return ""
	}
	img := &image.RGBA{Pix: buf.Bytes()[:tw*th*4], Stride: tw * 4, Rect: image.Rect(0, 0, tw, th)}
	return jpegDataURL(img)
}

func jpegDataURL(img image.Image) string {
	var j bytes.Buffer
	if jpeg.Encode(&j, img, &jpeg.Options{Quality: 70}) != nil {
		return ""
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(j.Bytes())
}

// screenInput: FFmpeg's X11 capture of that part of the screen (or that
// window), at the share's frame rate, scaled to the size to send.
func screenInput(o ScreenStart, tee *frameTee) ([]string, int, int, screenFeed, error) {
	if o.ID == portalShareID {
		return portalInput(o, tee)
	}
	if win, ok := parseXWindowID(o.ID); ok {
		return xWindowInput(o, win)
	}
	mw, mh, x, y, err := parseXShareID(o.ID)
	if err != nil {
		return nil, 0, 0, nil, err
	}
	w, h := shareSize(mw, mh, o.Height)
	args := []string{"-f", "x11grab", "-framerate", strconv.Itoa(o.FPS), "-video_size", fmt.Sprintf("%dx%d", mw, mh),
		"-draw_mouse", "1", "-thread_queue_size", "64", "-i", fmt.Sprintf("%s+%d,%d", xDisplay(), x, y)}
	if w != mw || h != mh {
		args = append(args, "-vf", fmt.Sprintf("scale=%d:%d:flags=bilinear", w, h))
	}
	log.Printf("screen: sharing X11 %s %dx%d at %d,%d (sent at %dx%d)", xDisplay(), mw, mh, x, y, w, h)
	return args, w, h, nil, nil
}

// xWindowInput: one window, by its X id (xcbgrab's window_id, FFmpeg 6.1
// and later). Always scaled to a fixed size, so the picture keeps going at
// that size if the window is resized.
func xWindowInput(o ScreenStart, win uint32) ([]string, int, int, screenFeed, error) {
	ww, wh, err := xWindowSize(win)
	if err != nil {
		return nil, 0, 0, nil, err
	}
	if st := xWindowState(win); st == "minimized" {
		return nil, 0, 0, nil, errors.New("that window is minimized — bring it back up, then share it")
	} else if st == "closed" {
		return nil, 0, 0, nil, errors.New("that window has closed")
	}
	w, h := shareSize(ww, wh, o.Height)
	args := []string{"-f", "x11grab", "-window_id", strconv.FormatUint(uint64(win), 10), "-framerate", strconv.Itoa(o.FPS),
		"-draw_mouse", "1", "-thread_queue_size", "64", "-i", xDisplay(),
		"-vf", fmt.Sprintf("scale=%d:%d:flags=bilinear", w, h)}
	log.Printf("screen: sharing X11 window 0x%x %dx%d (sent at %dx%d)", win, ww, wh, w, h)
	return args, w, h, nil, nil
}

// portalInput: the desktop's dialog (or the stream picked a moment ago, for
// a share that's restarting), its pictures fed to FFmpeg raw.
func portalInput(o ScreenStart, tee *frameTee) ([]string, int, int, screenFeed, error) {
	if !portalMode() {
		return nil, 0, 0, nil, errors.New("that share needs a Wayland session")
	}
	pc, err := portalAcquire(o.FPS)
	if err != nil {
		return nil, 0, 0, nil, err
	}
	sw, sh, err := pc.cap.waitFirstFrame(5 * time.Second)
	if err != nil {
		if pc.ps.w > 0 && pc.ps.h > 0 {
			sw, sh = pc.ps.w, pc.ps.h // it may still come; send black meanwhile
		} else {
			pc.release()
			return nil, 0, 0, nil, err
		}
	}
	w, h := shareSize(sw, sh, o.Height)
	log.Printf("screen: sharing through the desktop's portal, %dx%d (sent at %dx%d)", sw, sh, w, h)
	return []string{"-f", "rawvideo", "-pixel_format", "bgra", "-video_size", fmt.Sprintf("%dx%d", w, h),
		"-framerate", strconv.Itoa(o.FPS), "-i", "pipe:0"}, w, h, portalFeed(pc, w, h, o.FPS, tee), nil
}

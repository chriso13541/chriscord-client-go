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

// Sharing a screen from Linux: X11, through FFmpeg's X11 capture
// (xcbgrab, "-f x11grab"). Each monitor can be shared on its own — found
// with xrandr, cut out of the X screen by its position — or, without
// xrandr, the whole X screen at once. Single windows and the screen's
// sound aren't shared from Linux yet.
//
// Wayland doesn't let programs capture the screen this way (only through
// the desktop's screen-sharing portal and PipeWire, which FFmpeg can't do
// yet), so on a Wayland session it says so; an X11 session works.

func screenWindowsSupported() bool    { return false }
func screenGDIFallback(string) bool   { return false }
func sharedWindowState(string) string { return "" }

func screenAudioTarget(string) (uint32, bool, error) {
	return 0, false, errors.New("sharing a screen's sound from Linux isn't available yet")
}

// xDisplay: the X display to capture ("" if there isn't one).
func xDisplay() string { return os.Getenv("DISPLAY") }

func onWayland() bool {
	return strings.EqualFold(os.Getenv("XDG_SESSION_TYPE"), "wayland") || os.Getenv("WAYLAND_DISPLAY") != ""
}

func screenShareUnavailable() string {
	if onWayland() {
		return "Sharing your screen from a Wayland session isn't available yet — sign in with an X11 session instead (on the login screen, e.g. “GNOME on Xorg” or “Plasma (X11)”). You can still watch other people's."
	}
	if xDisplay() == "" {
		return "There's no X11 display to share (DISPLAY isn't set)"
	}
	if findFFmpeg() == "" {
		return "FFmpeg isn't available"
	}
	in := ffmpegInputs()
	if !strings.Contains(in["flv"], "E") {
		return "this FFmpeg was built without the FLV muxer — rebuild it with ffmpeg/build-ffmpeg.sh linux"
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
// all of it).
func xShareID(w, h, x, y int) string { return fmt.Sprintf("x:%dx%d+%d+%d", w, h, x, y) }

func parseXShareID(id string) (w, h, x, y int, err error) {
	if _, err = fmt.Sscanf(id, "x:%dx%d+%d+%d", &w, &h, &x, &y); err != nil || w <= 0 || h <= 0 {
		return 0, 0, 0, 0, errors.New("that isn't a screen this app knows")
	}
	return w, h, x, y, nil
}

func listShareSources() ([]ShareSource, error) {
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
	var j bytes.Buffer
	if jpeg.Encode(&j, img, &jpeg.Options{Quality: 70}) != nil {
		return ""
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(j.Bytes())
}

// screenInput: FFmpeg's X11 capture of that part of the screen, at the
// share's frame rate, scaled to the size to send.
func screenInput(o ScreenStart, _ *frameTee) ([]string, int, int, screenFeed, error) {
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

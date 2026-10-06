package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// The smaller version of a screen share.
//
// Someone watching can ask to receive streams at no more than a given size
// and frame rate (Settings → Screen Sharing → "Override stream native
// settings"). The server collects those requests and, when anyone watching
// this share wants less than it's being shared at, asks for a second,
// smaller version (voice_screen_low) — the largest anyone asked for. It's
// made by a second FFmpeg alongside the main one and sent on its own
// section ("screenlow"); the server passes it to just those viewers. When
// no one wants it any more the server says so (height 0) and it stops.
//
// A window copied here (Windows 10, screen_gdi_windows.go) isn't copied a
// second time: the smaller version takes the same frames (frameTee).
// Everything else gets its own capture in that second FFmpeg — scaled on
// the graphics card, which costs very little.

type screenLowSize struct{ height, fps int }

var (
	screenLowMu   sync.Mutex
	screenLowWant screenLowSize // what the server last asked for (0: none)
	activeLow     *nativeCamera
	// The most this app wants to receive of other people's screens (0:
	// as shared) — sent with every voice_watch.
	screenViewMu    sync.Mutex
	screenViewLimit screenLowSize
)

// SetScreenViewLimit sets the most this app wants to receive of other
// people's screens (height and fps; 0 for either: as they're shared). The
// page then re-sends voice_watch for the screens it's watching.
func (a *App) SetScreenViewLimit(height, fps int) {
	screenViewMu.Lock()
	if height <= 0 || fps <= 0 {
		height, fps = 0, 0
	}
	screenViewLimit = screenLowSize{height, fps}
	screenViewMu.Unlock()
}

func currentViewLimit() screenLowSize {
	screenViewMu.Lock()
	defer screenViewMu.Unlock()
	return screenViewLimit
}

// setScreenLowWanted: the server's voice_screen_low.
func (a *App) setScreenLowWanted(height, fps int) {
	if height <= 0 || fps <= 0 {
		height, fps = 0, 0
	}
	screenLowMu.Lock()
	screenLowWant = screenLowSize{height, fps}
	screenLowMu.Unlock()
	log.Printf("screen: smaller version wanted: %dp%d (0 = none)", height, fps)
	go a.restartScreenLow()
}

// restartScreenLow brings the smaller version in line with what's wanted
// and what's being shared: stops any running, starts a new one if needed.
func (a *App) restartScreenLow() {
	a.stopScreenLow()
	screenLowMu.Lock()
	want := screenLowWant
	screenLowMu.Unlock()
	screenMu.Lock()
	main := activeScreen
	screenMu.Unlock()
	if want.height == 0 || main == nil || main.stopped.Load() {
		return
	}
	if err := a.startScreenLow(main, want); err != nil {
		log.Printf("screen: smaller version couldn't start: %v", err)
	}
}

func (a *App) stopScreenLow() {
	screenLowMu.Lock()
	c := activeLow
	activeLow = nil
	screenLowMu.Unlock()
	if c == nil {
		return
	}
	c.stopped.Store(true)
	if c.cmd.Process != nil {
		c.cmd.Process.Kill()
	}
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
	}
}

// startScreenLow starts the smaller version of the running share main.
func (a *App) startScreenLow(main *nativeCamera, want screenLowSize) error {
	o := main.opts
	fps := want.fps
	if fps > o.FPS {
		fps = o.FPS
	}
	w, h := shareSize(main.w, main.h, want.height)
	if w >= main.w && h >= main.h && fps >= o.FPS {
		log.Printf("screen: smaller version not needed (%dp%d asked, sharing at %dx%d@%d)", want.height, want.fps, main.w, main.h, o.FPS)
		return nil
	}
	enc, err := resolveEncoder(o.Encoder)
	if err != nil {
		return err
	}
	var input []string
	var feed screenFeed
	if main.tee != nil {
		// Same frames as the main share, at this rate, scaled by FFmpeg.
		sink := main.tee.add(fps, main.w*main.h*4)
		input = []string{"-f", "rawvideo", "-pixel_format", "bgra", "-video_size", fmt.Sprintf("%dx%d", main.w, main.h),
			"-framerate", fmt.Sprint(fps), "-i", "pipe:0", "-vf", fmt.Sprintf("scale=%d:%d:flags=bilinear", w, h)}
		feed = func(out io.Writer, stop <-chan struct{}) error {
			defer main.tee.remove(sink)
			return sink.feed(out, stop, fps)
		}
	} else {
		lo := o
		lo.Height, lo.FPS = want.height, fps
		var lw, lh int
		input, lw, lh, feed, err = screenInput(lo, nil)
		if err != nil {
			return err
		}
		w, h = lw, lh
	}

	args := []string{"-hide_banner", "-loglevel", "error"}
	if feed == nil {
		args = append(args, "-nostdin")
	}
	if enc == "h264_nvenc" && strings.Contains(strings.Join(input, " "), "-init_hw_device") {
		args = append(args, "-init_hw_device", "cuda=enc") // see startScreen
	}
	args = append(args, input...)
	args = append(args, "-an")
	encArgs := encoderArgsLive(enc, screenBitrate(w, h, fps), fps, true)
	if enc == "h264_nvenc" {
		for i := range encArgs {
			if encArgs[i] == "-pix_fmt" && i+1 < len(encArgs) {
				encArgs[i+1] = "bgra"
			}
		}
	}
	args = append(args, encArgs...)
	args = append(args, "-f", "flv", "-flvflags", "no_duration_filesize", "-flush_packets", "1", "pipe:1")

	cmd := exec.Command(findFFmpeg(), args...)
	captureProcAttr(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stdin io.WriteCloser
	if feed != nil {
		if stdin, err = cmd.StdinPipe(); err != nil {
			return err
		}
	}
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, n: 8192}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("couldn't start FFmpeg: %w", err)
	}
	// Its own clock anchoring, on the main share's clock: both versions'
	// pictures carry capture times the sound lines up with.
	c := &nativeCamera{kind: "screenlow", cmd: cmd, done: make(chan struct{}), clock: &shareClock{epoch: main.clock.epoch}}
	c.send.Store(true)
	screenLowMu.Lock()
	activeLow = c
	screenLowMu.Unlock()
	log.Printf("screen: sending a smaller version at %dx%d@%d with %s", w, h, fps, enc)
	if feed != nil {
		go func() {
			err := feed(stdin, c.done)
			stdin.Close()
			if err != nil {
				log.Printf("screen: smaller version's frames stopped: %v", err)
			}
		}()
	}
	first := make(chan struct{}, 1)
	go func() {
		defer close(c.done)
		readErr := a.pumpFLV(c, stdout, fps, first)
		waitErr := cmd.Wait()
		if !c.stopped.Load() {
			log.Printf("screen: smaller version ended: %s", screenReason(stderr.String(), readErr, waitErr))
		}
		screenLowMu.Lock()
		if activeLow == c {
			activeLow = nil
		}
		screenLowMu.Unlock()
	}()
	select {
	case <-first:
		return nil
	case <-c.done:
		return errors.New(screenReason(stderr.String(), nil, nil))
	case <-time.After(10 * time.Second):
		a.stopScreenLow()
		return errors.New("didn't start within 10 seconds")
	}
}

// screenLowKeyframeRequested: a viewer of the smaller version needs a
// picture to start from.
func (a *App) screenLowKeyframeRequested(out *rtpOut) {
	screenLowMu.Lock()
	c := activeLow
	screenLowMu.Unlock()
	if c != nil {
		c.resendIfIdle(out)
	}
}

// ── Sharing one copied window's frames with the smaller version ─────────

// frameTee passes the window copier's fresh frames to whoever's listening
// (the smaller version), without ever making the copier wait.
type frameTee struct {
	mu    sync.Mutex
	sinks map[*teeSink]struct{}
}

func newFrameTee() *frameTee { return &frameTee{sinks: map[*teeSink]struct{}{}} }

type teeSink struct {
	mu       sync.Mutex
	buf      []byte
	have     bool
	every    time.Duration // copies taken at most this often (its frame rate)
	lastCopy time.Time
}

func (t *frameTee) add(fps, size int) *teeSink {
	s := &teeSink{buf: make([]byte, size), every: time.Second / time.Duration(fps) * 9 / 10}
	t.mu.Lock()
	t.sinks[s] = struct{}{}
	t.mu.Unlock()
	return s
}

func (t *frameTee) remove(s *teeSink) {
	t.mu.Lock()
	delete(t.sinks, s)
	t.mu.Unlock()
}

// publish offers one fresh frame (called by the copier; nil-safe).
func (t *frameTee) publish(frame []byte) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for s := range t.sinks {
		s.mu.Lock()
		if len(s.buf) == len(frame) && now.Sub(s.lastCopy) >= s.every {
			copy(s.buf, frame)
			s.have, s.lastCopy = true, now
		}
		s.mu.Unlock()
	}
}

// feed writes the latest frame to out at exactly fps (repeating it when no
// newer one has come), like the copier's own feed.
func (s *teeSink) feed(out io.Writer, stop <-chan struct{}, fps int) error {
	interval := time.Second / time.Duration(fps)
	frame := make([]byte, len(s.buf))
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	start := time.Now()
	for i := 0; ; i++ {
		due := start.Add(time.Duration(i) * interval)
		if d := time.Until(due); d > 0 {
			timer.Reset(d)
			select {
			case <-stop:
				return nil
			case <-timer.C:
			}
		} else {
			select {
			case <-stop:
				return nil
			default:
			}
		}
		s.mu.Lock()
		if s.have {
			copy(frame, s.buf)
			s.have = false
		}
		s.mu.Unlock()
		if _, err := out.Write(frame); err != nil {
			return nil // FFmpeg has stopped
		}
	}
}

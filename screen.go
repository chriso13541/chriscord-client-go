package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hraban/opus"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// Screen sharing.
//
// Like the native camera (capture.go), the bundled FFmpeg does the work
// outside the web view: on Windows, Windows Graphics Capture (FFmpeg's
// gfxcapture source) grabs a whole monitor or one application's window,
// scaled to the chosen size on the graphics card, and it's encoded to
// H.264 with the same encoder choice as the camera. The frames go into
// the call's second video track ("screen", see addVideoSender) through
// the same FLV pump as the camera.
//
// Nobody receives a screen until they click to watch it (WatchScreen):
// the server only forwards it to viewers who asked.

// ShareSource is one thing that can be shared: a monitor or a window.
type ShareSource struct {
	ID      string `json:"id"`   // "m:<monitor handle>" or "w:<window handle>"
	Kind    string `json:"kind"` // "screen" or "window"
	Name    string `json:"name"` // "Screen 1", or the window's title
	App     string `json:"app"`  // windows: the program, e.g. "chrome.exe"
	W       int    `json:"w"`    // its size in pixels right now
	H       int    `json:"h"`
	Primary bool   `json:"primary"`
	Index   int    `json:"index"` // screens: 0-based, left to right
	Thumb   string `json:"thumb"` // data: URL (JPEG), "" if it couldn't be captured
	Note    string `json:"note"`  // e.g. "Minimised"
}

// screenFeed writes raw frames into FFmpeg's stdin until stop is closed or
// FFmpeg stops reading — for captures made in this app rather than by
// FFmpeg (a window on Windows 10, see screen_gdi_windows.go). Returning an
// error ends the share with that reason.
type screenFeed func(out io.Writer, stop <-chan struct{}) error

// ScreenStart is what the page asks for.
type ScreenStart struct {
	ID         string `json:"id"`
	Height     int    `json:"height"` // 0 = full size
	FPS        int    `json:"fps"`
	Encoder    string `json:"encoder"`    // as for the camera
	Preview    bool   `json:"preview"`    // frames back to the page as screen:preview
	Audio      bool   `json:"audio"`      // share its sound too
	HideBorder bool   `json:"hideBorder"` // no yellow capture border — always set by StartScreenShare; false only for its fallback
}

var (
	screenMu     sync.Mutex
	activeScreen *nativeCamera
	screenGen    int // bumped whenever a share is stopped or replaced

	screenRetryMu sync.Mutex
	screenRetries []time.Time
)

// screenRetryAllowed: a share that keeps losing its capture is picked back
// up at most 5 times a minute; past that something's wrong, and it ends.
func screenRetryAllowed() bool {
	screenRetryMu.Lock()
	defer screenRetryMu.Unlock()
	now := time.Now()
	kept := screenRetries[:0]
	for _, t := range screenRetries {
		if now.Sub(t) < time.Minute {
			kept = append(kept, t)
		}
	}
	screenRetries = kept
	if len(screenRetries) >= 5 {
		return false
	}
	screenRetries = append(screenRetries, now)
	return true
}

var (
	ffFiltersOnce sync.Once
	ffFilters     map[string]bool
)

// ffmpegFilters: the filters (and filter sources) this FFmpeg has.
func ffmpegFilters() map[string]bool {
	ffFiltersOnce.Do(func() {
		ffFilters = map[string]bool{}
		out, err := ffmpegCmd("-hide_banner", "-filters").Output()
		if err != nil {
			return
		}
		// Lines look like " ... gfxcapture        |->V       Capture …".
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Fields(line)
			if len(f) >= 3 && strings.Contains(f[2], "->") {
				ffFilters[f[1]] = true
			}
		}
	})
	return ffFilters
}

// ScreenShareInfo says whether screens can be shared from here.
func (a *App) ScreenShareInfo() map[string]interface{} {
	if reason := screenShareUnavailable(); reason != "" {
		return map[string]interface{}{"available": false, "reason": reason}
	}
	return map[string]interface{}{"available": true, "windows": screenWindowsSupported(), "encoderLabel": encoderLabel(captureEncoder())}
}

// ListShareSources lists the screens and windows that can be shared, each
// with a small picture of what's on it.
func (a *App) ListShareSources() ([]ShareSource, error) {
	if reason := screenShareUnavailable(); reason != "" {
		return nil, errors.New(reason)
	}
	return listShareSources()
}

// screenBitrate: screens need more than a camera at the same size (sharp
// text, lots of fine detail), less per extra frame.
func screenBitrate(w, h, fps int) int {
	px := w * h
	base := 10_000_000
	switch {
	case px <= 1280*720:
		base = 2_500_000
	case px <= 1920*1080:
		base = 4_500_000
	case px <= 2560*1440:
		base = 7_000_000
	}
	scale := float64(fps) / 30
	if scale < .5 {
		scale = .5
	}
	if scale > 1.7 {
		scale = 1.7
	}
	b := int(float64(base) * scale)
	if b > 12_000_000 {
		b = 12_000_000
	}
	return b
}

// shareSize: the size to send — the source scaled down to the chosen
// height (never up), kept even (H.264 needs that).
func shareSize(w, h, height int) (int, int) {
	if w <= 0 || h <= 0 {
		w, h = 1920, 1080
	}
	if height > 0 && height < h {
		w = w * height / h
		h = height
	}
	return w &^ 1, h &^ 1
}

// StartScreenShare starts sharing a screen or window (replacing any share
// already running). It returns the label of the encoder in use once the
// first frame is out, or why it couldn't start.
func (a *App) StartScreenShare(opts ScreenStart) (string, error) {
	// Always without the yellow capture border where Windows allows it
	// (Desktop Duplication for screens, a PrintWindow copy for windows on
	// Windows 10, the borderless setting on Windows 11).
	opts.HideBorder = true
	label, err := a.startScreen(opts)
	if err != nil && opts.HideBorder {
		// The border-free way (Desktop Duplication for a screen, GDI for a
		// window on Windows 10) didn't work on this computer: share with
		// Windows Graphics Capture instead, and say so.
		log.Printf("screen: border-free capture failed (%v); trying Windows Graphics Capture", err)
		opts.HideBorder = false
		if label, err2 := a.startScreen(opts); err2 == nil {
			wailsruntime.EventsEmit(a.ctx, "screen:notice", "Couldn’t hide the yellow border on this computer ("+err.Error()+"), so it’s showing.")
			return label, nil
		}
	}
	return label, err
}

func (a *App) startScreen(opts ScreenStart) (string, error) {
	a.StopScreenShare()
	if reason := screenShareUnavailable(); reason != "" {
		return "", errors.New(reason)
	}
	if opts.FPS <= 0 || opts.FPS > 120 {
		opts.FPS = 30
	}
	enc, err := resolveEncoder(opts.Encoder)
	if err != nil {
		return "", err
	}
	tee := newFrameTee()
	input, w, h, feed, err := screenInput(opts, tee)
	if err != nil {
		return "", err
	}
	args := []string{"-hide_banner", "-loglevel", "error"}
	if feed == nil {
		args = append(args, "-nostdin")
	}
	if enc == "h264_nvenc" && strings.Contains(strings.Join(input, " "), "-init_hw_device") {
		// With a D3D11 device set up for Desktop Duplication, FFmpeg would
		// hand that to NVENC too — and on a laptop it's the Intel chip's,
		// which NVENC can't use. A CUDA device (NVENC's first choice) keeps
		// it on the NVIDIA card.
		args = append(args, "-init_hw_device", "cuda=enc")
	}
	args = append(args, input...)
	args = append(args, "-an")
	encArgs := encoderArgsLive(enc, screenBitrate(w, h, opts.FPS), opts.FPS, true)
	if enc == "h264_nvenc" {
		// NVENC takes the captured BGRA as it is and converts it on the card.
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
		return "", err
	}
	var stdin io.WriteCloser
	if feed != nil {
		if stdin, err = cmd.StdinPipe(); err != nil {
			return "", err
		}
	}
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, n: 8192}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("couldn't start FFmpeg: %w", err)
	}
	var running atomic.Bool // got going (a share that never started isn't picked back up)
	c := &nativeCamera{kind: "screen", cmd: cmd, done: make(chan struct{}), clock: newShareClock(), opts: opts, w: w, h: h}
	if feed != nil {
		c.tee = tee // a window copied here: the smaller version shares its frames
	}
	c.send.Store(true)
	c.preview.Store(opts.Preview)
	first := make(chan struct{}, 1)
	screenMu.Lock()
	activeScreen = c
	screenMu.Unlock()
	log.Printf("screen: sharing %s at %dx%d@%d with %s", opts.ID, w, h, opts.FPS, enc)

	// Frames made here go into FFmpeg; when that stops (the window closed,
	// say), FFmpeg's input ends and so does the share.
	feedDone := make(chan struct{})
	var feedErr error // set before feedDone closes
	feedReason := func(wait time.Duration) error {
		select {
		case <-feedDone:
			return feedErr
		case <-time.After(wait):
			return nil
		}
	}
	if feed != nil {
		go func() {
			feedErr = feed(stdin, c.done)
			stdin.Close()
			if feedErr != nil {
				log.Printf("screen: window copy stopped: %v", feedErr)
			}
			close(feedDone)
		}()
	}

	go func() {
		defer close(c.done)
		readErr := a.pumpFLV(c, stdout, opts.FPS, first)
		waitErr := cmd.Wait()
		c.stopAudio()
		if c.stopped.Load() {
			return
		}
		a.stopScreenLow()
		reason := screenReason(stderr.String(), readErr, waitErr)
		if feed != nil {
			if err := feedReason(time.Second); err != nil {
				reason = "Screen sharing stopped: " + err.Error()
			}
		}
		log.Printf("screen: sharing ended: %s (%s)", reason, strings.TrimSpace(stderr.String()))
		screenMu.Lock()
		if activeScreen == c {
			activeScreen = nil
		}
		gen := screenGen
		screenMu.Unlock()
		// Desktop Duplication gives up when the screen changes under it — a
		// game going full screen, the resolution changing, a UAC prompt —
		// with "access lost" (887a0026), and FFmpeg's ddagrab stops there.
		// Pick the share straight back up instead of ending it.
		if running.Load() && strings.Contains(strings.ToLower(stderr.String()), "887a0026") && screenRetryAllowed() {
			go func() {
				time.Sleep(500 * time.Millisecond)
				screenMu.Lock()
				still := screenGen == gen && activeScreen == nil // not stopped or replaced meanwhile
				screenMu.Unlock()
				if !still {
					return
				}
				log.Printf("screen: screen capture lost access (a full-screen game, a display change…); starting it again")
				if _, err := a.startScreen(opts); err != nil {
					log.Printf("screen: couldn't pick the share back up: %v", err)
					wailsruntime.EventsEmit(a.ctx, "screen:stopped", reason)
				}
			}()
			return
		}
		wailsruntime.EventsEmit(a.ctx, "screen:stopped", reason)
	}()

	select {
	case <-first:
		running.Store(true)
	case <-c.done:
		if feed != nil {
			if err := feedReason(0); err != nil {
				return "", err
			}
		}
		return "", errors.New(screenReason(stderr.String(), nil, nil))
	case <-time.After(10 * time.Second):
		a.StopScreenShare()
		return "", errors.New("screen sharing didn't start within 10 seconds")
	}
	// What it's being shared at, so the server knows who wants less (and a
	// smaller version, if one's already wanted).
	a.voiceMu.Lock()
	session := a.voice
	a.voiceMu.Unlock()
	if session != nil {
		_ = a.sendVoiceJSON(map[string]interface{}{"type": "voice_screen_native", "board_id": session.boardID, "height": h, "fps": opts.FPS})
	}
	go a.restartScreenLow()
	if opts.Audio {
		select {
		case <-c.done: // FFmpeg already gave up after its first frame
		default:
			a.startScreenAudio(c, opts.ID)
		}
	}
	return encoderLabel(enc), nil
}

// startScreenAudio starts sending the shared screen's sound: everything
// playing except this app for a whole screen, just that program for a
// window. If it can't, the picture carries on and the page is told why
// ("screen:audio").
func (a *App) startScreenAudio(c *nativeCamera, id string) {
	pid, exclude, err := screenAudioTarget(id)
	if err == nil {
		err = startSystemAudio(pid, exclude)
	}
	if err != nil {
		log.Printf("screen: no sound: %v", err)
		wailsruntime.EventsEmit(a.ctx, "screen:audio", map[string]interface{}{"ok": false, "reason": err.Error()})
		return
	}
	stop, done := make(chan struct{}), make(chan struct{})
	c.audioMu.Lock()
	if c.audioOff {
		// The share ended (or failed) while the sound was starting: don't
		// leave it capturing.
		c.audioMu.Unlock()
		stopSystemAudio()
		log.Printf("screen: share ended before its sound started; sound not shared")
		return
	}
	c.audioStopCh, c.audioDone = stop, done
	c.audioMu.Unlock()
	log.Printf("screen: sharing sound (pid %d, exclude=%v)", pid, exclude)
	go func() {
		defer close(done)
		a.runScreenAudio(c, stop)
	}()
	wailsruntime.EventsEmit(a.ctx, "screen:audio", map[string]interface{}{"ok": true})
}

// runScreenAudio encodes the captured sound (Opus, stereo, 128 kb/s, 20 ms
// frames) into the call's screen-sound track until stop is closed.
func (a *App) runScreenAudio(c *nativeCamera, stop <-chan struct{}) {
	defer stopSystemAudio()
	// This app's own sound (the call) is kept out of the recording by
	// Windows; if some of it gets in anyway, it's taken back out (echo.go).
	echoRef.enable()
	defer echoRef.disable()
	echo := newEchoCanceller(echoRef)
	enc, err := opus.NewEncoder(48000, 2, opus.AppAudio)
	if err != nil {
		log.Printf("screen: sound encoder: %v", err)
		return
	}
	_ = enc.SetBitrate(128000)
	const frame = 960 // 20 ms
	f := make([]float32, frame*2)
	pcm := make([]int16, frame*2)
	out := make([]byte, 4000)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	var captured, sent, skipped int
	lastLog := time.Now()
	// Capture time of the next frame, on the share's clock (avsync.go):
	// counted on 20 ms at a time from an anchor, re-anchored if it drifts
	// from what the buffer level says.
	capMs, anchored := 0.0, false
	for {
		select {
		case <-stop:
			log.Printf("screen: sound stopped (%d frames sent)", sent)
			return
		case <-tick.C:
		}
		if time.Since(lastLog) >= 5*time.Second {
			log.Printf("screen: sound — last 5 s: %d ms captured, %d ms sent, %d ms skipped", captured*20, sent*20, skipped*20)
			captured, sent, skipped, lastLog = 0, 0, 0, time.Now()
		}
		// Fallen far behind (e.g. a moment outside a call): skip to now.
		for systemAudioAvailable() > frame*10 {
			readSystemAudio(f)
			echo.skip(frame)
			skipped++
		}
		if systemAudioAvailable() == 0 {
			echo.catchUp() // nothing recorded for a while: keep its count on time
		}
		for systemAudioAvailable() >= frame {
			if readSystemAudio(f) < frame {
				break
			}
			captured++
			// This frame ended about as long ago as what's still queued behind
			// it (plus Windows' own ~10 ms), so it started 20 ms before that.
			est := c.clock.nowMs() - 10 - float64(systemAudioAvailable())/48 - 20
			if !anchored || math.Abs(est-capMs) > 60 {
				capMs, anchored = est, true
			}
			frameCap := capMs
			capMs += 20
			echo.process(f)
			for i, v := range f {
				pcm[i] = int16(clampF(float64(v), -1, 1) * 32767)
			}
			n, err := enc.Encode(pcm, out)
			if err != nil {
				continue
			}
			a.voiceMu.Lock()
			session := a.voice
			a.voiceMu.Unlock()
			if session == nil || session.soundOut == nil || !c.send.Load() {
				continue
			}
			if err := session.soundOut.write(append([]byte(nil), out[:n]...), rtpTS(frameCap, 48000)); err != nil {
				log.Printf("screen: sending sound failed: %v", err)
			} else {
				sent++
			}
		}
	}
}

// StopScreenShare stops sharing (no-op if not sharing).
func (a *App) StopScreenShare() {
	a.stopScreenLow()
	screenMu.Lock()
	screenGen++
	c := activeScreen
	activeScreen = nil
	screenMu.Unlock()
	if c == nil {
		return
	}
	c.stopped.Store(true)
	c.stopAudio()
	if c.cmd.Process != nil {
		c.cmd.Process.Kill()
	}
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
	}
}

// SetScreenPreview turns the sharer's own preview frames on or off.
func (a *App) SetScreenPreview(on bool) {
	screenMu.Lock()
	c := activeScreen
	screenMu.Unlock()
	if c != nil {
		c.preview.Store(on)
	}
}

// screenKeyframeRequested: a viewer needs a picture to start from.
func (a *App) screenKeyframeRequested(out *rtpOut) {
	screenMu.Lock()
	c := activeScreen
	screenMu.Unlock()
	if c != nil {
		c.resendIfIdle(out)
	}
}

// screenReason turns FFmpeg's complaint into a sentence for the page.
func screenReason(stderr string, errs ...error) string {
	low := strings.ToLower(stderr)
	switch {
	case strings.Contains(low, "closed") || strings.Contains(low, "invalid window") || strings.Contains(low, "no longer"):
		return "The window you were sharing was closed"
	case strings.Contains(low, "no matching") || strings.Contains(low, "not found"):
		return "That screen or window isn't available any more"
	}
	// The first line that says what actually went wrong — FFmpeg follows it
	// with knock-on lines ("Task finished…", "Nothing was written…").
	var pick string
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		line = strings.TrimSpace(line)
		low := strings.ToLower(line)
		if line == "" || strings.Contains(low, "nothing was written") || strings.Contains(low, "terminating thread") ||
			strings.Contains(low, "task finished") || strings.Contains(low, "could not open encoder before eof") ||
			strings.Contains(low, "error sending frames to consumers") {
			if pick == "" && line != "" {
				pick = line // only if there's nothing better
			}
			continue
		}
		pick = line
		break
	}
	if pick != "" {
		if i := strings.Index(pick, "] "); strings.HasPrefix(pick, "[") && i > 0 {
			pick = pick[i+2:] // drop FFmpeg's "[component @ address]" prefix
		}
		if len(pick) > 160 {
			pick = pick[:160] + "…"
		}
		return "Screen sharing stopped: " + pick
	}
	for _, e := range errs {
		if e != nil && e.Error() != "EOF" {
			return "Screen sharing stopped: " + e.Error()
		}
	}
	return "Screen sharing stopped"
}

// parseShareID splits "m:123" / "w:456".
func parseShareID(id string) (kind string, handle uint64, err error) {
	k, v, ok := strings.Cut(id, ":")
	if !ok || (k != "m" && k != "w") {
		return "", 0, errors.New("unknown screen or window")
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil || n == 0 {
		return "", 0, errors.New("unknown screen or window")
	}
	if k == "m" {
		return "screen", n, nil
	}
	return "window", n, nil
}

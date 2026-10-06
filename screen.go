package main

import (
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hraban/opus"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
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
	Index   int    `json:"index"`  // screens: 0-based, left to right
	Thumb   string `json:"thumb"`  // data: URL (JPEG), "" if it couldn't be captured
	Note    string `json:"note"`   // e.g. "Minimised"
}

// ScreenStart is what the page asks for.
type ScreenStart struct {
	ID      string `json:"id"`
	Height  int    `json:"height"` // 0 = full size
	FPS     int    `json:"fps"`
	Encoder string `json:"encoder"` // as for the camera
	Preview bool   `json:"preview"` // frames back to the page as screen:preview
	Audio   bool   `json:"audio"`   // share its sound too
}

var (
	screenMu     sync.Mutex
	activeScreen *nativeCamera
)

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
	input, w, h, err := screenInputArgs(opts)
	if err != nil {
		return "", err
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
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
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, n: 8192}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("couldn't start FFmpeg: %w", err)
	}
	c := &nativeCamera{kind: "screen", cmd: cmd, done: make(chan struct{})}
	c.send.Store(true)
	c.preview.Store(opts.Preview)
	first := make(chan struct{}, 1)
	screenMu.Lock()
	activeScreen = c
	screenMu.Unlock()
	log.Printf("screen: sharing %s at %dx%d@%d with %s", opts.ID, w, h, opts.FPS, enc)

	go func() {
		defer close(c.done)
		readErr := a.pumpFLV(c, stdout, opts.FPS, first)
		waitErr := cmd.Wait()
		c.stopAudio()
		if c.stopped.Load() {
			return
		}
		reason := screenReason(stderr.String(), readErr, waitErr)
		log.Printf("screen: sharing ended: %s (%s)", reason, strings.TrimSpace(stderr.String()))
		screenMu.Lock()
		if activeScreen == c {
			activeScreen = nil
		}
		screenMu.Unlock()
		wailsruntime.EventsEmit(a.ctx, "screen:stopped", reason)
	}()

	select {
	case <-first:
	case <-c.done:
		return "", errors.New(screenReason(stderr.String(), nil, nil))
	case <-time.After(10 * time.Second):
		a.StopScreenShare()
		return "", errors.New("screen sharing didn't start within 10 seconds")
	}
	if opts.Audio {
		a.startScreenAudio(c, opts.ID)
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
	log.Printf("screen: sharing sound (pid %d, exclude=%v)", pid, exclude)
	stop, done := make(chan struct{}), make(chan struct{})
	c.audioMu.Lock()
	c.audioStopCh, c.audioDone = stop, done
	c.audioMu.Unlock()
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
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		// Fallen far behind (e.g. a moment outside a call): skip to now.
		for systemAudioAvailable() > frame*10 {
			readSystemAudio(f)
		}
		for systemAudioAvailable() >= frame {
			if readSystemAudio(f) < frame {
				break
			}
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
			if session == nil || session.screenAudioTrack == nil || !c.send.Load() {
				continue
			}
			_ = session.screenAudioTrack.WriteSample(media.Sample{Data: append([]byte(nil), out[:n]...), Duration: 20 * time.Millisecond})
		}
	}
}

// StopScreenShare stops sharing (no-op if not sharing).
func (a *App) StopScreenShare() {
	screenMu.Lock()
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
func (a *App) screenKeyframeRequested(trk *webrtc.TrackLocalStaticSample) {
	screenMu.Lock()
	c := activeScreen
	screenMu.Unlock()
	if c != nil {
		c.resendIfIdle(trk)
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
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		if len(last) > 160 {
			last = last[:160] + "…"
		}
		return "Screen sharing stopped: " + last
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

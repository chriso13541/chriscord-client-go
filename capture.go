package main

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// Native camera capture.
//
// Instead of the page capturing and encoding the camera (getUserMedia +
// WebCodecs, see video.go), the bundled FFmpeg does it outside the web
// view: DirectShow (Windows) / V4L2 (Linux) in, the graphics card's H.264
// encoder (NVENC, or Media Foundation — Quick Sync / AMD / NVIDIA) out,
// tuned for live video (no B-frames, constant bitrate, keyframe every 2 s).
// FFmpeg writes FLV to a pipe — FLV because every frame arrives as one
// tagged, sized packet with its keyframe flag, so frames are passed on
// the moment they're encoded with no guessing where one ends. Each frame
// is turned back into Annex B H.264 and written to the call's video track
// (the same track the page path uses; the server and viewers can't tell
// the difference). Your own preview is your encoded stream, sent to the
// page ("camera:preview") and decoded there on the graphics card, like
// anyone else's.
//
// Only modes the camera reports natively (FFmpeg asks the driver) are
// offered, and the camera is opened in exactly that mode; a lower frame
// rate than the mode's is made by dropping frames after capture.

// CameraDevice is one camera as the system names it.
type CameraDevice struct {
	ID   string `json:"id"`   // what FFmpeg opens it by
	Name string `json:"name"` // what to show
}

// CameraMode is one size the camera produces natively, at its best frame
// rate, and the pixel format that gets that rate.
type CameraMode struct {
	W      int    `json:"w"`
	H      int    `json:"h"`
	FPS    int    `json:"fps"`
	Format string `json:"format"` // e.g. "mjpeg", "yuyv422", "nv12"
}

// CameraStart is what the page asks for.
type CameraStart struct {
	ID      string `json:"id"`
	W       int    `json:"w"`
	H       int    `json:"h"`
	FPS     int    `json:"fps"`     // frame rate to send
	ModeFPS int    `json:"modeFps"` // frame rate the camera runs at (≥ FPS)
	Format  string `json:"format"`
	Send    bool   `json:"send"`    // into the call (when in one)
	Preview bool   `json:"preview"` // also to the page as camera:preview
	Encoder string `json:"encoder"` // "auto" (default), "hardware", "nvenc" or "software"
}

type nativeCamera struct {
	cmd     *exec.Cmd
	send    atomic.Bool
	preview atomic.Bool
	stopped atomic.Bool
	done    chan struct{}
}

var (
	camMu     sync.Mutex
	activeCam *nativeCamera
)

// NativeCaptureInfo says whether native capture can be used here, and
// with which encoder.
func (a *App) NativeCaptureInfo() map[string]interface{} {
	if findFFmpeg() == "" {
		return map[string]interface{}{"available": false, "reason": "FFmpeg isn't available"}
	}
	have := ffmpegInputs()
	if !strings.Contains(have[cameraInputFormat()], "D") {
		return map[string]interface{}{"available": false, "reason": "this FFmpeg was built without camera capture — rebuild it with ffmpeg/build-ffmpeg.sh"}
	}
	if !strings.Contains(have["flv"], "E") {
		return map[string]interface{}{"available": false, "reason": "this FFmpeg was built without the FLV muxer — rebuild it with ffmpeg/build-ffmpeg.sh"}
	}
	enc := captureEncoder()
	return map[string]interface{}{"available": true, "encoder": enc, "encoderLabel": encoderLabel(enc)}
}

var (
	ffInputsOnce sync.Once
	ffInputs     map[string]string // format name → its flags ("D" read, "E" write)
)

// ffmpegInputs: which formats/devices this FFmpeg can read ("D") or write
// ("E").
func ffmpegInputs() map[string]string {
	ffInputsOnce.Do(func() {
		ffInputs = map[string]string{}
		out, err := ffmpegCmd("-hide_banner", "-formats").Output()
		if err != nil {
			return
		}
		// Lines look like " DE flv  FLV (Flash Video)" / " D  dshow  …":
		// flag columns (D, E, d, .) and then the name(s).
		for _, line := range strings.Split(string(out), "\n") {
			flags := ""
			for _, f := range strings.Fields(line) {
				if strings.Trim(f, "DEd.") == "" {
					flags += f
					continue
				}
				for _, n := range strings.Split(f, ",") {
					ffInputs[n] += flags
				}
				break
			}
		}
	})
	return ffInputs
}

// captureEncoder: the graphics card's H.264 encoder if one works here,
// else x264 on the CPU (fast settings).
func captureEncoder() string {
	if hw, _ := hardwareEncoder(); hw != "" {
		return hw
	}
	return "libx264"
}

func encoderLabel(enc string) string {
	switch enc {
	case "h264_nvenc":
		return "H.264 on your NVIDIA graphics card (native)"
	case "h264_mf":
		return "H.264 on your graphics card (native)"
	}
	return "H.264 on the CPU (native)"
}

// EncoderChoice is one option in Settings → Voice & Video → Encoder.
type EncoderChoice struct {
	ID        string `json:"id"`    // "auto", "hardware", "nvenc", "software"
	Label     string `json:"label"` // what the menu shows
	Available bool   `json:"available"`
	Reason    string `json:"reason"` // why not, when it isn't
}

// The camera encoder choices and the FFmpeg encoder each one means.
var encoderChoices = []struct{ id, enc, label string }{
	{"hardware", "h264_mf", "Hardware H.264 (Media Foundation \u2014 Intel, AMD or NVIDIA)"},
	{"nvenc", "h264_nvenc", "NVIDIA NVENC"},
	{"software", "libx264", "Software H.264 (CPU)"},
}

var (
	encTestMu  sync.Mutex
	encTestRes = map[string]string{} // FFmpeg encoder → "" (works) or why not
)

// encoderWorks tries a short live-settings encode once per encoder (the
// answer is remembered until "Check again" in Settings → Performance).
func encoderWorks(enc string) string {
	encTestMu.Lock()
	defer encTestMu.Unlock()
	if r, ok := encTestRes[enc]; ok {
		return r
	}
	var why string
	if have := ffmpegEncoders(); have != nil && !have[enc] {
		why = "this FFmpeg wasn't built with it"
	} else if enc == "h264_mf" && cameraInputFormat() != "dshow" {
		why = "only on Windows"
	} else if msg := testEncode(enc, encoderArgsLive(enc, 1_000_000, 30, false)[2:]); msg != "" {
		why = explainEncoderError(enc, msg)
	}
	encTestRes[enc] = why
	return why
}

// CaptureEncoders lists the encoder choices for the camera, with which
// ones work on this computer. Auto picks NVENC, then hardware H.264, then
// the CPU.
func (a *App) CaptureEncoders() []EncoderChoice {
	out := []EncoderChoice{{ID: "auto", Label: "Auto \u2014 " + strings.TrimSuffix(encoderLabel(captureEncoder()), " (native)"), Available: true}}
	for _, c := range encoderChoices {
		if c.id == "hardware" && cameraInputFormat() != "dshow" {
			continue // Media Foundation is Windows-only
		}
		why := encoderWorks(c.enc)
		out = append(out, EncoderChoice{ID: c.id, Label: c.label, Available: why == "", Reason: why})
	}
	return out
}

// resolveEncoder turns a choice into the FFmpeg encoder to use.
func resolveEncoder(choice string) (string, error) {
	for _, c := range encoderChoices {
		if c.id == choice {
			if why := encoderWorks(c.enc); why != "" {
				return "", fmt.Errorf("%s isn't available: %s", c.label, why)
			}
			return c.enc, nil
		}
	}
	return captureEncoder(), nil // "auto" or anything unknown
}

// ListCameras names the cameras FFmpeg can open.
func (a *App) ListCameras() ([]CameraDevice, error) {
	return listCameraDevices()
}

// CameraModes lists the sizes a camera produces natively, biggest first.
func (a *App) CameraModes(id string) ([]CameraMode, error) {
	modes, err := listCameraModes(id)
	if err != nil {
		return nil, err
	}
	sort.Slice(modes, func(i, j int) bool { return modes[i].W*modes[i].H > modes[j].W*modes[j].H })
	return modes, nil
}

// bestModes keeps one entry per size: the format with the highest frame
// rate, preferring uncompressed formats on a tie (no JPEG to decode).
func bestModes(all []CameraMode) []CameraMode {
	best := map[[2]int]CameraMode{}
	for _, m := range all {
		k := [2]int{m.W, m.H}
		cur, ok := best[k]
		better := !ok || m.FPS > cur.FPS || (m.FPS == cur.FPS && cur.Format == "mjpeg" && m.Format != "mjpeg")
		if better {
			best[k] = m
		}
	}
	out := make([]CameraMode, 0, len(best))
	for _, m := range best {
		out = append(out, m)
	}
	return out
}

func cameraBitrate(w, h, fps int) int {
	px := w * h
	base := 8_000_000
	switch {
	case px <= 320*240:
		base = 300_000
	case px <= 640*480:
		base = 800_000
	case px <= 1280*720:
		base = 1_800_000
	case px <= 1920*1080:
		base = 3_500_000
	}
	scale := float64(fps) / 30
	if scale < .5 {
		scale = .5
	}
	if scale > 2 {
		scale = 2
	}
	return int(float64(base) * scale)
}

// encoderArgsLive: low-latency settings — no B-frames (they add delay),
// constant bitrate, a keyframe every 2 s so anyone joining or recovering
// gets a picture quickly, Constrained Baseline to match the call's codec.
func encoderArgsLive(enc string, bitrate, fps int, screen bool) []string {
	b := strconv.Itoa(bitrate)
	half := strconv.Itoa(bitrate / 2)
	gop := strconv.Itoa(fps * 2)
	switch enc {
	case "h264_nvenc":
		return []string{"-c:v", "h264_nvenc", "-preset", "p2", "-tune", "ull", "-zerolatency", "1", "-delay", "0",
			"-rc", "cbr", "-b:v", b, "-maxrate", b, "-bufsize", half, "-profile:v", "baseline",
			"-g", gop, "-bf", "0", "-forced-idr", "1", "-pix_fmt", "nv12"}
	case "h264_mf":
		scenario := "video_conference"
		if screen {
			scenario = "display_remoting"
		}
		return []string{"-c:v", "h264_mf", "-hw_encoding", "1", "-scenario", scenario, "-rate_control", "cbr",
			"-b:v", b, "-g", gop, "-bf", "0", "-pix_fmt", "nv12"} // (no profile option for MF: it uses its own default)
	}
	return []string{"-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency", "-profile:v", "baseline",
		"-b:v", b, "-maxrate", b, "-bufsize", half, "-g", gop, "-bf", "0", "-pix_fmt", "yuv420p"}
}

// StartCamera opens the camera in FFmpeg and starts sending (replacing any
// camera already running). It returns the label of the encoder in use once
// FFmpeg has produced its first frame, or FFmpeg's reason if it couldn't
// open the camera.
func (a *App) StartCamera(opts CameraStart) (string, error) {
	a.StopCamera()
	if opts.W <= 0 || opts.H <= 0 || opts.ModeFPS <= 0 {
		return "", errors.New("no camera mode chosen")
	}
	if opts.FPS <= 0 || opts.FPS > opts.ModeFPS {
		opts.FPS = opts.ModeFPS
	}
	enc, err := resolveEncoder(opts.Encoder)
	if err != nil {
		return "", err
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-fflags", "nobuffer"}
	args = append(args, cameraInputArgs(opts)...)
	if opts.FPS < opts.ModeFPS {
		args = append(args, "-vf", "fps="+strconv.Itoa(opts.FPS))
	}
	args = append(args, "-an")
	args = append(args, encoderArgsLive(enc, cameraBitrate(opts.W, opts.H, opts.FPS), opts.FPS, false)...)
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
	c := &nativeCamera{cmd: cmd, done: make(chan struct{})}
	c.send.Store(opts.Send)
	c.preview.Store(opts.Preview)
	first := make(chan struct{}, 1)
	camMu.Lock()
	activeCam = c
	camMu.Unlock()
	log.Printf("camera: native capture %dx%d@%d (camera %d fps, %s) with %s", opts.W, opts.H, opts.FPS, opts.ModeFPS, opts.Format, enc)

	go func() {
		defer close(c.done)
		readErr := a.pumpFLV(c, stdout, opts.FPS, first)
		waitErr := cmd.Wait()
		if c.stopped.Load() {
			return // stopped on purpose
		}
		reason := ffmpegReason(stderr.String(), readErr, waitErr)
		log.Printf("camera: native capture ended: %s", reason)
		camMu.Lock()
		if activeCam == c {
			activeCam = nil
		}
		camMu.Unlock()
		wailsruntime.EventsEmit(a.ctx, "camera:stopped", reason)
	}()

	select {
	case <-first:
		return encoderLabel(enc), nil
	case <-c.done:
		return "", errors.New(ffmpegReason(stderr.String(), nil, nil))
	case <-time.After(10 * time.Second):
		a.StopCamera()
		return "", errors.New("the camera didn't start within 10 seconds")
	}
}

// StopCamera stops native capture (no-op if it isn't running).
func (a *App) StopCamera() {
	camMu.Lock()
	c := activeCam
	activeCam = nil
	camMu.Unlock()
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

// SetCameraOutputs changes where the running camera's frames go.
func (a *App) SetCameraOutputs(send, preview bool) {
	camMu.Lock()
	c := activeCam
	camMu.Unlock()
	if c != nil {
		c.send.Store(send)
		c.preview.Store(preview)
	}
}

type previewFrame struct {
	Key  bool   `json:"key"`
	Data string `json:"data"`
}

// pumpFLV reads FFmpeg's FLV output: each video tag is one encoded frame.
func (a *App) pumpFLV(c *nativeCamera, r io.Reader, fps int, first chan<- struct{}) error {
	br := bufio.NewReaderSize(r, 1<<20)
	head := make([]byte, 13) // FLV header (9) + first PreviousTagSize (4)
	if _, err := io.ReadFull(br, head); err != nil {
		return err
	}
	if string(head[:3]) != "FLV" {
		return errors.New("FFmpeg didn't produce FLV")
	}
	var sps, pps [][]byte
	nalLen := 4
	lastTS, frames, bytes := uint32(0), 0, 0
	statT := time.Now()
	sentFirst := false
	tag := make([]byte, 11)
	for {
		if _, err := io.ReadFull(br, tag); err != nil {
			return err
		}
		size := int(tag[1])<<16 | int(tag[2])<<8 | int(tag[3])
		ts := uint32(tag[4])<<16 | uint32(tag[5])<<8 | uint32(tag[6]) | uint32(tag[7])<<24
		body := make([]byte, size+4) // + PreviousTagSize
		if _, err := io.ReadFull(br, body); err != nil {
			return err
		}
		body = body[:size]
		if tag[0] != 9 || len(body) < 5 || body[0]&0x0f != 7 { // video, AVC
			continue
		}
		key := body[0]>>4 == 1
		switch body[1] {
		case 0: // AVCDecoderConfigurationRecord: the SPS/PPS
			sps, pps, nalLen = parseAVCConfig(body[5:])
			continue
		case 1:
		default:
			continue
		}
		au := avccToAnnexB(body[5:], nalLen, key, sps, pps)
		if len(au) == 0 {
			continue
		}
		dur := time.Second / time.Duration(fps)
		if lastTS != 0 && ts > lastTS && ts-lastTS < 1000 {
			dur = time.Duration(ts-lastTS) * time.Millisecond
		}
		lastTS = ts
		if !sentFirst {
			sentFirst = true
			select {
			case first <- struct{}{}:
			default:
			}
		}
		if c.send.Load() {
			a.voiceMu.Lock()
			session := a.voice
			a.voiceMu.Unlock()
			if session != nil && session.videoTrack != nil &&
				strings.EqualFold(session.videoTrack.Codec().MimeType, webrtc.MimeTypeH264) {
				session.videoTrack.WriteSample(media.Sample{Data: au, Duration: dur})
			}
		}
		if c.preview.Load() {
			wailsruntime.EventsEmit(a.ctx, "camera:preview", previewFrame{Key: key, Data: base64.StdEncoding.EncodeToString(au)})
		}
		frames++
		bytes += len(au)
		if el := time.Since(statT); el >= time.Second {
			wailsruntime.EventsEmit(a.ctx, "camera:stats", map[string]interface{}{
				"fps": float64(frames) / el.Seconds(), "kbps": float64(bytes*8) / 1000 / el.Seconds(),
			})
			frames, bytes, statT = 0, 0, time.Now()
		}
	}
}

// parseAVCConfig pulls the SPS and PPS out of an AVCDecoderConfigurationRecord.
func parseAVCConfig(b []byte) (sps, pps [][]byte, nalLen int) {
	nalLen = 4
	if len(b) < 7 {
		return
	}
	nalLen = int(b[4]&3) + 1
	p := 5
	n := int(b[p] & 0x1f)
	p++
	for i := 0; i < n && p+2 <= len(b); i++ {
		l := int(binary.BigEndian.Uint16(b[p:]))
		p += 2
		if p+l > len(b) {
			return
		}
		sps = append(sps, append([]byte(nil), b[p:p+l]...))
		p += l
	}
	if p >= len(b) {
		return
	}
	n = int(b[p])
	p++
	for i := 0; i < n && p+2 <= len(b); i++ {
		l := int(binary.BigEndian.Uint16(b[p:]))
		p += 2
		if p+l > len(b) {
			return
		}
		pps = append(pps, append([]byte(nil), b[p:p+l]...))
		p += l
	}
	return
}

// avccToAnnexB turns length-prefixed NAL units into start-code ones, and
// puts the SPS/PPS in front of keyframes that don't carry their own (a
// viewer can only start decoding from a keyframe that has them).
func avccToAnnexB(b []byte, nalLen int, key bool, sps, pps [][]byte) []byte {
	start := []byte{0, 0, 0, 1}
	var out []byte
	hasSPS := false
	var nals [][]byte
	for p := 0; p+nalLen <= len(b); {
		l := 0
		for i := 0; i < nalLen; i++ {
			l = l<<8 | int(b[p+i])
		}
		p += nalLen
		if l <= 0 || p+l > len(b) {
			break
		}
		nal := b[p : p+l]
		if nal[0]&0x1f == 7 {
			hasSPS = true
		}
		nals = append(nals, nal)
		p += l
	}
	if key && !hasSPS {
		for _, s := range sps {
			out = append(append(out, start...), s...)
		}
		for _, s := range pps {
			out = append(append(out, start...), s...)
		}
	}
	for _, nal := range nals {
		out = append(append(out, start...), nal...)
	}
	return out
}

// ffmpegReason turns FFmpeg's complaint into a sentence for the page.
func ffmpegReason(stderr string, errs ...error) string {
	low := strings.ToLower(stderr)
	switch {
	case strings.Contains(low, "could not run graph") || strings.Contains(low, "device or resource busy") ||
		strings.Contains(low, "being used by another"):
		return "Your camera is busy — another app may be using it"
	case strings.Contains(low, "could not find video device") || strings.Contains(low, "no such file or directory"):
		return "That camera isn't connected any more"
	case strings.Contains(low, "could not set video options") || strings.Contains(low, "invalid argument"):
		return "The camera refused that mode — try Re-check camera in Settings"
	}
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		if len(last) > 160 {
			last = last[:160] + "…"
		}
		return "The camera stopped: " + last
	}
	for _, e := range errs {
		if e != nil && !errors.Is(e, io.EOF) {
			return "The camera stopped: " + e.Error()
		}
	}
	return "The camera stopped"
}

// limitedWriter keeps the last part of FFmpeg's error output.
type limitedWriter struct {
	w *strings.Builder
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.w.Len()+len(p) > l.n {
		s := l.w.String() + string(p)
		if len(s) > l.n {
			s = s[len(s)-l.n:]
		}
		l.w.Reset()
		l.w.WriteString(s)
		return len(p), nil
	}
	return l.w.Write(p)
}

var (
	reModeDshow = regexp.MustCompile(`(vcodec|pixel_format)=(\S+)\s+min s=(\d+)x(\d+) fps=([\d.]+)\s+max s=(\d+)x(\d+) fps=([\d.]+)`)
	reDevDshow  = regexp.MustCompile(`"([^"]+)" \(video\)`)
	reAltDshow  = regexp.MustCompile(`Alternative name "([^"]+)"`)
)

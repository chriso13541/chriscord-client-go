package main

// transcode.go — making videos playable in the app before they're sent.
//
// When you attach a video, the app checks what's inside it and, if other
// people's apps might not be able to play it inline, converts it on your
// computer first, doing as little work as possible:
//
//   already MP4 with H.264 + AAC/MP3          → sent as it is
//   H.264 video in another container (MKV…)   → repackaged to MP4 (seconds; video untouched)
//   …with AC-3/DTS/FLAC/other sound           → video untouched, sound converted to AAC
//   HEVC, MPEG-2, VP9, 10-bit H.264, …        → video converted to H.264 (uses the graphics
//                                               card's encoder when there is one)
//
// The FFmpeg that does it ships inside the app (ffmpeg/build-ffmpeg.sh), so
// nobody has to install anything; failing that, one on the PATH is used;
// failing that, videos are simply sent as they are. Conversion happens on
// the sender's computer, so it keeps working once attachments are
// end-to-end encrypted (the server never needs to read them).

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// ── Finding FFmpeg ───────────────────────────────────────────────────────────

var (
	ffmpegOnce sync.Once
	ffmpegPath string
	ffmpegFrom string // "bundled" or "system"
)

// findFFmpeg unpacks the bundled FFmpeg into the user's cache folder the
// first time (named by its hash, so an app update brings a fresh copy), or
// falls back to one on the PATH. "" if there's none.
func findFFmpeg() string {
	ffmpegOnce.Do(func() {
		if p, err := unpackBundledFFmpeg(); err == nil && p != "" {
			ffmpegPath, ffmpegFrom = p, "bundled"
			return
		} else if err != nil {
			fmt.Println("transcode: bundled ffmpeg unusable:", err)
		}
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			ffmpegPath, ffmpegFrom = p, "system"
		}
	})
	return ffmpegPath
}

func unpackBundledFFmpeg() (string, error) {
	if bundledFFmpegFile == "" {
		return "", nil
	}
	gz, err := bundledFFmpeg.ReadFile(bundledFFmpegDir + "/" + bundledFFmpegFile)
	if err != nil {
		return "", nil // not bundled in this build
	}
	sum := sha256.Sum256(gz)
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cache, "chriscord", "ffmpeg", hex.EncodeToString(sum[:])[:16])
	dest := filepath.Join(dir, ffmpegExeName)
	if st, err := os.Stat(dest); err == nil && st.Size() > 0 {
		return dest, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return "", err
	}
	tmp := dest + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, zr); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	f.Close()
	return dest, os.Rename(tmp, dest)
}

// hideWindow keeps a console window from flashing up on Windows while
// FFmpeg runs, and starts it at below-normal priority when asked (see
// transcode_windows.go). lowerPriority does the latter elsewhere
// (transcode_unix.go).
var hideWindow = func(cmd *exec.Cmd) {}

func ffmpegCmd(args ...string) *exec.Cmd {
	cmd := exec.Command(findFFmpeg(), args...)
	hideWindow(cmd)
	return cmd
}

// ── Looking inside a video ───────────────────────────────────────────────────

type mediaInfo struct {
	Container  string  // ffmpeg's demuxer names, e.g. "matroska,webm" / "mov,mp4,m4a,3gp,3g2,mj2"
	Duration   float64 // seconds
	VideoCodec string  // "h264", "hevc", … ("" = none)
	PixFmt     string  // "yuv420p", "yuv420p10le", …
	Width      int
	Height     int
	AudioCodec string // "aac", "ac3", "dts", … ("" = none)
}

var (
	reInput    = regexp.MustCompile(`(?m)^Input #0, ([^,]+(?:,[^,\s]+)*), from`)
	reDuration = regexp.MustCompile(`Duration: (\d+):(\d+):(\d+(?:\.\d+)?)`)
	reVideo    = regexp.MustCompile(`(?m)Stream #0:\d+.*?: Video: (\w+)[^,]*,\s*(\w+)[^,]*?(?:,|$)(?:.*?(\d{2,5})x(\d{2,5}))?`)
	reAudio    = regexp.MustCompile(`(?m)Stream #0:\d+.*?: Audio: (\w+)`)
)

// probeMedia reads a file's container and first video/audio streams from
// `ffmpeg -i` (which describes the input, then exits with "no output").
func probeMedia(path string) (mediaInfo, error) {
	var info mediaInfo
	out, _ := ffmpegCmd("-hide_banner", "-nostdin", "-i", path).CombinedOutput()
	text := string(out)
	m := reInput.FindStringSubmatch(text)
	if m == nil {
		return info, fmt.Errorf("not a video FFmpeg can read")
	}
	info.Container = m[1]
	if d := reDuration.FindStringSubmatch(text); d != nil {
		h, _ := strconv.Atoi(d[1])
		mi, _ := strconv.Atoi(d[2])
		s, _ := strconv.ParseFloat(d[3], 64)
		info.Duration = float64(h*3600+mi*60) + s
	}
	for _, line := range strings.Split(text, "\n") {
		// Skip attached cover art ("(attached pic)") — it shows up as a video stream.
		if info.VideoCodec == "" && strings.Contains(line, ": Video: ") && !strings.Contains(line, "attached pic") {
			if v := reVideo.FindStringSubmatch(line); v != nil {
				info.VideoCodec, info.PixFmt = v[1], v[2]
				info.Width, _ = strconv.Atoi(v[3])
				info.Height, _ = strconv.Atoi(v[4])
			}
		}
		if info.AudioCodec == "" && strings.Contains(line, ": Audio: ") {
			if a := reAudio.FindStringSubmatch(line); a != nil {
				info.AudioCodec = a[1]
			}
		}
	}
	if info.VideoCodec == "" {
		return info, fmt.Errorf("no video stream")
	}
	return info, nil
}

// ── Deciding what to do ──────────────────────────────────────────────────────

type convertPlan struct {
	Needed      bool
	CopyVideo   bool
	CopyAudio   bool
	Description string // what will happen, for the app ("Repackaging…")
}

// planConversion: what (if anything) has to change for the video to play
// in every copy of the app — an MP4 with 8-bit H.264 and AAC/MP3 sound.
func planConversion(info mediaInfo) convertPlan {
	isMP4 := strings.Contains(info.Container, "mp4")
	h264OK := info.VideoCodec == "h264" && (info.PixFmt == "yuv420p" || info.PixFmt == "yuvj420p")
	audioOK := info.AudioCodec == "" || info.AudioCodec == "aac" || info.AudioCodec == "mp3"
	p := convertPlan{CopyVideo: h264OK, CopyAudio: audioOK}
	p.Needed = !(isMP4 && h264OK && audioOK)
	switch {
	case !p.Needed:
		p.Description = ""
	case h264OK && audioOK:
		p.Description = "Repackaging"
	case h264OK:
		p.Description = "Converting sound"
	default:
		p.Description = "Converting"
	}
	return p
}

// ── Performance settings (Settings → Performance) ────────────────────────────

var (
	settingsMu       sync.Mutex
	transcodeThreads int // 0 = let FFmpeg use every core
	transcodeLowPrio = true
	transcodeUseGPU  = true
)

// SetTranscodeSettings applies the Performance settings: how much of the
// CPU conversions may use (as a percentage of its threads; 100 = all), whether
// they run at low priority so games and other apps stay smooth, and whether
// the graphics card's encoder may be used.
func (a *App) SetTranscodeSettings(cpuPercent int, lowPriority, useGPU bool) {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	transcodeThreads = 0
	if cpuPercent > 0 && cpuPercent < 100 {
		n := (goruntime.NumCPU()*cpuPercent + 50) / 100
		if n < 1 {
			n = 1
		}
		transcodeThreads = n
	}
	transcodeLowPrio = lowPriority
	transcodeUseGPU = useGPU
}

func transcodeSettings() (threads int, lowPrio, useGPU bool) {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	return transcodeThreads, transcodeLowPrio, transcodeUseGPU
}

// ── H.264 encoders ───────────────────────────────────────────────────────────

var (
	encoderOnce sync.Once
	encoderName string
)

// pickEncoder tries the graphics card's H.264 encoder first (NVIDIA NVENC;
// on Windows also Media Foundation, which reaches Intel Quick Sync and AMD
// too), with a tiny test encode, and falls back to x264 on the CPU — or
// goes straight to x264 if the graphics card is turned off in settings.
func pickEncoder() string {
	if _, _, useGPU := transcodeSettings(); !useGPU {
		return "libx264"
	}
	encoderOnce.Do(func() {
		candidates := []string{"h264_nvenc"}
		if goruntime.GOOS == "windows" {
			candidates = append(candidates, "h264_mf")
		}
		for _, enc := range candidates {
			args := append([]string{"-hide_banner", "-nostdin", "-loglevel", "error",
				"-f", "lavfi", "-i", "color=c=black:s=320x240:d=0.3", "-c:v", enc}, encoderArgs(enc, 320, 240)...)
			args = append(args, "-f", "null", "-")
			cmd := ffmpegCmd(args...)
			done := make(chan error, 1)
			if cmd.Start() == nil {
				go func() { done <- cmd.Wait() }()
				select {
				case err := <-done:
					if err == nil {
						encoderName = enc
						return
					}
				case <-time.After(15 * time.Second):
					cmd.Process.Kill()
				}
			}
		}
		encoderName = "libx264"
	})
	return encoderName
}

// encoderArgs: good-looking settings per encoder. Hardware encoders work
// to a bitrate picked from the picture size; x264 to a quality level.
func encoderArgs(enc string, w, h int) []string {
	pixels := w * h
	rate := "4M"
	switch {
	case pixels >= 3840*2160*9/10:
		rate = "20M"
	case pixels >= 2560*1440*9/10:
		rate = "12M"
	case pixels >= 1920*1080*9/10:
		rate = "8M"
	case pixels >= 1280*720*9/10:
		rate = "5M"
	}
	switch enc {
	case "h264_nvenc":
		return []string{"-preset", "p4", "-rc", "vbr", "-cq", "23", "-b:v", rate, "-maxrate", rate}
	case "h264_mf":
		return []string{"-hw_encoding", "1", "-rate_control", "u_vbr", "-b:v", rate}
	default:
		return []string{"-preset", "veryfast", "-crf", "21"}
	}
}

// ── Converting ───────────────────────────────────────────────────────────────

var (
	convertMu   sync.Mutex
	convertJobs = map[string]*exec.Cmd{}
)

// ConvertVideoForPlayback is called by the app for each video attached. It
// returns the path to upload: a converted copy (in a temp folder — pass it
// to DiscardConverted once uploaded), or "" to send the original (already
// playable, or no FFmpeg available). Progress goes out as
// "convert:progress" {id, percent, stage}.
func (a *App) ConvertVideoForPlayback(path, jobID string) (string, error) {
	if findFFmpeg() == "" {
		return "", nil
	}
	info, err := probeMedia(path)
	if err != nil {
		return "", nil // not something we understand — send as is
	}
	plan := planConversion(info)
	if !plan.Needed {
		return "", nil
	}
	enc := "copy"
	if !plan.CopyVideo {
		enc = pickEncoder()
	}
	runtime.EventsEmit(a.ctx, "convert:progress", map[string]interface{}{"id": jobID, "percent": 0, "stage": plan.Description, "encoder": enc})

	dir := filepath.Join(os.TempDir(), "chriscord-convert", jobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	out := filepath.Join(dir, base+".mp4")

	threads, lowPrio, _ := transcodeSettings()
	args := []string{"-hide_banner", "-nostdin", "-y"}
	if threads > 0 { // decoding (often the heaviest part, e.g. 10-bit HEVC) obeys the limit too
		args = append(args, "-threads", strconv.Itoa(threads))
	}
	args = append(args, "-i", path,
		"-map", "0:v:0", "-map", "0:a:0?", "-sn", "-dn", "-map_metadata", "0")
	if threads > 0 {
		args = append(args, "-threads", strconv.Itoa(threads), "-filter_threads", strconv.Itoa(threads))
	}
	if plan.CopyVideo {
		args = append(args, "-c:v", "copy")
	} else {
		// Even dimensions and 8-bit colour (HEVC is often 10-bit, which H.264 players can't show).
		args = append(args, "-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2,format=yuv420p", "-c:v", enc)
		args = append(args, encoderArgs(enc, info.Width, info.Height)...)
	}
	if plan.CopyAudio {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-b:a", "192k", "-ac", "2")
	}
	args = append(args, "-movflags", "+faststart", "-progress", "pipe:1", "-nostats", "-loglevel", "error", out)

	cmd := ffmpegCmd(args...)
	stdout, _ := cmd.StdoutPipe()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", err
	}
	if lowPrio {
		lowerPriority(cmd) // (on Windows it's set when the process is created)
	}
	convertMu.Lock()
	convertJobs[jobID] = cmd
	convertMu.Unlock()
	defer func() { convertMu.Lock(); delete(convertJobs, jobID); convertMu.Unlock() }()

	// -progress writes "out_time_us=…" lines; turn them into a percentage.
	sc := bufio.NewScanner(stdout)
	last := time.Time{}
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "out_time_us=") && info.Duration > 0 {
			us, _ := strconv.ParseFloat(strings.TrimPrefix(line, "out_time_us="), 64)
			pct := int(us / 1e6 / info.Duration * 100)
			if pct > 99 {
				pct = 99
			}
			if time.Since(last) > 250*time.Millisecond {
				last = time.Now()
				runtime.EventsEmit(a.ctx, "convert:progress", map[string]interface{}{"id": jobID, "percent": pct, "stage": plan.Description, "encoder": enc})
			}
		}
	}
	if err := cmd.Wait(); err != nil {
		os.RemoveAll(dir)
		if cmd.ProcessState != nil && !cmd.ProcessState.Success() && wasCancelled(jobID) {
			return "", fmt.Errorf("cancelled")
		}
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		return "", fmt.Errorf("conversion failed: %s", msg)
	}
	runtime.EventsEmit(a.ctx, "convert:progress", map[string]interface{}{"id": jobID, "percent": 100, "stage": plan.Description, "encoder": enc})
	return out, nil
}

var cancelled sync.Map

func wasCancelled(jobID string) bool { _, ok := cancelled.LoadAndDelete(jobID); return ok }

// CancelConversion stops a conversion (the attachment was removed).
func (a *App) CancelConversion(jobID string) {
	convertMu.Lock()
	cmd := convertJobs[jobID]
	convertMu.Unlock()
	if cmd != nil && cmd.Process != nil {
		cancelled.Store(jobID, true)
		cmd.Process.Kill()
	}
}

// DiscardConverted deletes a converted copy once it has been uploaded.
func (a *App) DiscardConverted(path string) {
	dir := filepath.Dir(path)
	if strings.HasPrefix(dir, filepath.Join(os.TempDir(), "chriscord-convert")) {
		os.RemoveAll(dir)
	}
}

// VideoConversionInfo tells the settings page whether conversion is
// available and how: {"available", "ffmpeg": "bundled"|"system", "encoder"}.
func (a *App) VideoConversionInfo() map[string]interface{} {
	cores := goruntime.NumCPU()
	if findFFmpeg() == "" {
		return map[string]interface{}{"available": false, "cores": cores}
	}
	// What the graphics card can do, regardless of the current setting.
	settingsMu.Lock()
	saved := transcodeUseGPU
	transcodeUseGPU = true
	settingsMu.Unlock()
	hw := pickEncoder()
	settingsMu.Lock()
	transcodeUseGPU = saved
	settingsMu.Unlock()
	return map[string]interface{}{"available": true, "ffmpeg": ffmpegFrom, "encoder": pickEncoder(), "gpuEncoder": hw, "cores": cores}
}

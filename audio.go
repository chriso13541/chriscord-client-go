package main

// audio.go — voice audio controls: input gain, output volume, input
// sensitivity (manual threshold or automatic), and per-user volume/mute.
//
// Everything here is plain in-memory state read by the capture, playback
// and mic-test loops in voice.go on every 20 ms frame. The frontend owns
// persistence (localStorage) and pushes the saved values in on startup and
// whenever they change, so nothing here touches disk.

import (
	"math"
	"sync"
	"sync/atomic"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// Sensitivity is in dBFS: 0 is the loudest a mic can report, around -100
// is digital silence. Normal speech into a typical mic sits roughly
// between -45 and -20; a quiet room's background noise between -70 and -50.
const (
	defaultThresholdDB = -50.0
	minThresholdDB     = -100.0
	maxThresholdDB     = 0.0
	// Auto sensitivity sits this far above the measured background noise,
	// kept within a sane range so a silent or very noisy room can't push it
	// somewhere useless.
	autoMarginDB     = 12.0
	autoThresholdMin = -65.0
	autoThresholdMax = -30.0
	// Stays "open" through short pauses between words (~300 ms) instead of
	// cutting out on every syllable break.
	vadHangoverFrames = 15
	maxGain           = 2.0 // sliders go to 200%
)

type userAudio struct {
	Volume float64 // 1 = 100%
	Muted  bool    // muted locally, for you only
}

type audioSettings struct {
	mu              sync.RWMutex
	inputGain       float64
	outputVolume    float64
	autoSensitivity bool
	thresholdDB     float64
	users           map[string]userAudio
}

var audioCfg = &audioSettings{
	inputGain: 1, outputVolume: 1, autoSensitivity: true, thresholdDB: defaultThresholdDB,
	users: map[string]userAudio{},
}

// levelMeterOn is set while the voice settings page is open, so mic level
// events are only emitted when something is actually drawing them.
var levelMeterOn atomic.Bool

func clampF(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

// SetAudioSettings applies the voice settings page's values.
func (a *App) SetAudioSettings(inputGain, outputVolume float64, autoSensitivity bool, thresholdDB float64) {
	audioCfg.mu.Lock()
	audioCfg.inputGain = clampF(inputGain, 0, maxGain)
	audioCfg.outputVolume = clampF(outputVolume, 0, maxGain)
	audioCfg.autoSensitivity = autoSensitivity
	audioCfg.thresholdDB = clampF(thresholdDB, minThresholdDB, maxThresholdDB)
	audioCfg.mu.Unlock()
}

// SetUserAudio sets how loud another participant plays for you, and
// whether you've muted them — local only, nobody else is affected.
func (a *App) SetUserAudio(username string, volume float64, muted bool) {
	audioCfg.mu.Lock()
	if volume == 1 && !muted {
		delete(audioCfg.users, username)
	} else {
		audioCfg.users[username] = userAudio{Volume: clampF(volume, 0, maxGain), Muted: muted}
	}
	audioCfg.mu.Unlock()
}

// SetStreamVideoLag is reported by the page while you watch someone's
// screen: how long after capture its picture reaches your screen, as
// (this computer's wall-clock ms) − (capture time on the sharer's clock).
// The stream's sound is held back to the same lag (avsync.go).
func (a *App) SetStreamVideoLag(user string, lagMs float64) {
	streamLagMu.Lock()
	if lagMs <= 0 {
		delete(streamLag, user)
	} else {
		streamLag[user] = lagMs
	}
	streamLagMu.Unlock()
}

var (
	streamLagMu sync.Mutex
	streamLag   = map[string]float64{} // sharer → picture lag (see SetStreamVideoLag)
)

func streamVideoLag(user string) (float64, bool) {
	streamLagMu.Lock()
	defer streamLagMu.Unlock()
	v, ok := streamLag[user]
	return v, ok
}

// SetLevelMeter turns live mic level events (voice:level) on or off.
func (a *App) SetLevelMeter(on bool) { levelMeterOn.Store(on) }

func (s *audioSettings) input() (gain float64, auto bool, threshold float64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inputGain, s.autoSensitivity, s.thresholdDB
}

// playbackGain is the multiplier for one participant's audio: their own
// volume times the overall output volume, or 0 if you've muted them.
func (s *audioSettings) playbackGain(username string) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[username]
	if !ok {
		return s.outputVolume
	}
	if u.Muted {
		return 0
	}
	return u.Volume * s.outputVolume
}

// levelDB is a frame's loudness in dBFS.
func levelDB(samples []int16) float64 {
	rms := rmsLevel(samples)
	if rms < 1 {
		return -100
	}
	return math.Max(-100, 20*math.Log10(rms/32768))
}

// applyGain scales samples in place, clipping instead of wrapping around.
func applyGain(buf []int16, gain float64) {
	if gain == 1 {
		return
	}
	for i, v := range buf {
		buf[i] = int16(clampF(float64(v)*gain, -32768, 32767))
	}
}

// voiceActivity is one capture loop's voice-activity detector: it decides
// frame by frame whether the mic is "open" (you're talking — transmit it
// and light the speaking ring) or below your input sensitivity (send
// silence instead). One per capture loop; only touched from that loop.
type voiceActivity struct {
	open       bool
	quiet      int
	noiseFloor float64 // dBFS, tracked for automatic sensitivity
	frames     int
}

func newVoiceActivity() *voiceActivity { return &voiceActivity{noiseFloor: -60} }

// process applies input gain to buf in place and runs detection on the
// result. changed reports whether open flipped on this frame.
func (v *voiceActivity) process(buf []int16) (open, changed bool, level, threshold float64) {
	gain, auto, manual := audioCfg.input()
	applyGain(buf, gain)
	level = levelDB(buf)
	threshold = manual
	if auto {
		// Background-noise estimate: follows quieter levels quickly and
		// louder ones slowly, and never learns from frames it has already
		// decided are speech — so talking doesn't drag the threshold up.
		if level < v.noiseFloor {
			v.noiseFloor += (level - v.noiseFloor) * 0.1
		} else if !v.open {
			v.noiseFloor += (level - v.noiseFloor) * 0.01
		}
		threshold = clampF(v.noiseFloor+autoMarginDB, autoThresholdMin, autoThresholdMax)
	}
	was := v.open
	if level >= threshold {
		v.open, v.quiet = true, 0
	} else if v.open {
		v.quiet++
		if v.quiet > vadHangoverFrames {
			v.open = false
		}
	}
	return v.open, v.open != was, level, threshold
}

// reset closes the gate (used when muting), reporting whether it was open.
func (v *voiceActivity) reset() (wasOpen bool) {
	wasOpen = v.open
	v.open, v.quiet = false, 0
	return wasOpen
}

// emitLevel sends the mic level to the settings page's meter, about 16
// times a second, only while that page is open.
func (v *voiceActivity) emitLevel(a *App, level, threshold float64) {
	v.frames++
	if !levelMeterOn.Load() || v.frames%3 != 0 || a.ctx == nil {
		return
	}
	wailsruntime.EventsEmit(a.ctx, "voice:level", map[string]interface{}{
		"level": math.Round(level*10) / 10, "threshold": math.Round(threshold*10) / 10, "open": v.open,
	})
}

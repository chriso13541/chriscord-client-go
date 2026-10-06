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

	// Automatic gain: brings speech to a steady loudness for everyone else,
	// the way Discord's voice processing does — most microphones deliver
	// speech well below it (around −30 dBFS), which is why people had to be
	// turned up to 160–200%.
	agcTargetDB  = -23.0 // speech level aimed for (frame RMS, dBFS)
	agcMaxBoost  = 12.0  // at most this much louder (4×)…
	agcMaxCut    = -10.0 // … or quieter
	agcUpPerSec  = 6.0   // how fast the gain may rise (dB per second)…
	agcDownPerFr = 1.0   // … and fall (dB per 20 ms frame: loud speech is pulled in quickly)
	// Between words (the gate held open, nothing above the threshold) the
	// boost is eased off, so background noise isn't turned up with you;
	// it comes back as soon as you speak again.
	agcGapEasePerFr = 1.5 // dB per frame towards no boost in a gap…
	agcSpeechPerFr  = 4.0 // … and back to your gain when speech resumes
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
	autoGain        bool
	users           map[string]userAudio
}

var audioCfg = &audioSettings{
	inputGain: 1, outputVolume: 1, autoSensitivity: true, thresholdDB: defaultThresholdDB, autoGain: true,
	users: map[string]userAudio{},
}

// levelMeterOn is set while the voice settings page is open, so mic level
// events are only emitted when something is actually drawing them.
var levelMeterOn atomic.Bool

func clampF(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

// SetAudioSettings applies the voice settings page's values.
func (a *App) SetAudioSettings(inputGain, outputVolume float64, autoSensitivity bool, thresholdDB float64, autoGain bool) {
	audioCfg.mu.Lock()
	audioCfg.inputGain = clampF(inputGain, 0, maxGain)
	audioCfg.outputVolume = clampF(outputVolume, 0, maxGain)
	audioCfg.autoSensitivity = autoSensitivity
	audioCfg.thresholdDB = clampF(thresholdDB, minThresholdDB, maxThresholdDB)
	audioCfg.autoGain = autoGain
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
	old, had := streamLag[user]
	switch {
	case lagMs <= 0:
		delete(streamLag, user)
	case !had || math.Abs(lagMs-old) > 500:
		streamLag[user] = lagMs // first figure, or a real jump: take it
	default:
		// Smoothed, so one slow second doesn't yank the sound around.
		streamLag[user] = old*0.75 + lagMs*0.25
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

func (s *audioSettings) input() (gain float64, auto bool, threshold float64, autoGain bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inputGain, s.autoSensitivity, s.thresholdDB, s.autoGain
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
	agc        autoGain
}

func newVoiceActivity() *voiceActivity { return &voiceActivity{noiseFloor: -60} }

// process applies input gain to buf in place and runs detection on the
// result; frames that will be sent then get automatic gain (if on). The
// level reported (for the meter and the sensitivity) is before automatic
// gain, so the threshold keeps meaning the same thing. changed reports
// whether open flipped on this frame.
func (v *voiceActivity) process(buf []int16) (open, changed bool, level, threshold float64) {
	gain, auto, manual, autoGain := audioCfg.input()
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
	if autoGain && v.open {
		v.agc.process(buf, level, level >= threshold)
	}
	return v.open, v.open != was, level, threshold
}

// autoGain follows how loud your speech is and turns it up (or down) to
// agcTargetDB, slowly enough that it's never heard pumping, with a soft
// limiter so a shout or a laugh doesn't clip.
type autoGain struct {
	have      bool
	speechDB  float64 // running estimate of your speech level, before this gain
	gainDB    float64 // the gain your speech gets
	appliedDB float64 // what's applied right now (eased off between words)
}

// process applies the gain to one frame. level is its loudness before
// this gain; speech says the frame is above the sensitivity threshold
// (frames in the gap between words are let through but don't teach it).
func (g *autoGain) process(buf []int16, level float64, speech bool) {
	if speech && level > -70 {
		if !g.have {
			g.have, g.speechDB = true, level
			g.gainDB = clampF(agcTargetDB-level, agcMaxCut, agcMaxBoost) * 0.5 // start halfway
		} else if level > g.speechDB {
			g.speechDB += (level - g.speechDB) * 0.08
		} else {
			g.speechDB += (level - g.speechDB) * 0.02
		}
	}
	if g.have {
		want := clampF(agcTargetDB-g.speechDB, agcMaxCut, agcMaxBoost)
		up := agcUpPerSec * voiceFrameMs / 1000
		switch {
		case want > g.gainDB+up:
			g.gainDB += up
		case want < g.gainDB-agcDownPerFr:
			g.gainDB -= agcDownPerFr
		default:
			g.gainDB = want
		}
	}
	// Speech gets the full gain; a gap between words gets no boost (a cut
	// still applies), eased towards over a few frames either way.
	target := g.gainDB
	step := agcSpeechPerFr
	if !speech {
		target = math.Min(g.gainDB, 0)
		step = agcGapEasePerFr
	}
	from := g.appliedDB
	switch {
	case target > from+step:
		g.appliedDB = from + step
	case target < from-step:
		g.appliedDB = from - step
	default:
		g.appliedDB = target
	}
	// Ramped across the frame, so a change is never a click.
	m0, m1 := math.Pow(10, from/20), math.Pow(10, g.appliedDB/20)
	n := float64(len(buf))
	for i, v := range buf {
		mul := m0 + (m1-m0)*float64(i)/n
		buf[i] = int16(softLimit(float64(v)*mul/32768) * 32767)
	}
}

// softLimit: unchanged up to −3 dBFS, then rounded off smoothly towards
// full scale instead of clipping.
func softLimit(x float64) float64 {
	const knee = 0.7
	ax := math.Abs(x)
	if ax <= knee {
		return x
	}
	y := knee + (1-knee)*math.Tanh((ax-knee)/(1-knee))
	return math.Copysign(y, x)
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

package main

import (
	"log"
	"math"
	"sync/atomic"
	"time"
)

// Smooth voice playback over network hiccups (an adaptive jitter buffer,
// in the spirit of WebRTC's NetEQ, scaled down to what a call here needs).
//
// Each person's decoded voice waits in a queue until the speaker plays it,
// 20 ms at a time. Packets normally arrive about as fast as they're played,
// but not evenly — and when the network stalls, everything held up lands
// in one burst. A queue that only ever plays at normal speed keeps such a
// backlog as delay for the rest of the call, and a sound card that runs a
// little slow (common in virtual machines) builds one up bit by bit.
//
// So, each 20 ms:
//
//   - Measure how much of the queue is pure delay: the least that was
//     waiting at any moment over the last second or two (beyond the frame
//     being played). Audio that was always sitting there unused could have
//     been played sooner — except for a safety margin kept for packets
//     arriving unevenly. That margin adapts to the connection: 20 ms to
//     start with, 20 ms more each time the queue runs dry (up to 200 ms),
//     and 20 ms less after every 15 s without running dry.
//   - Remove that excess where nobody can hear it: from pauses. The sender
//     sends silence whenever its voice gate is closed, so between words
//     and sentences there are whole silent frames, and one is dropped only
//     in the middle of a pause (the frame before it was silent too).
//   - Only when the excess is large (150 ms or more) and the person isn't
//     pausing, take one pitch cycle out of their speech (pitchTrim), at
//     most once every 80 ms. That shortens it by a few milliseconds without
//     changing its pitch — never by playing it faster.
//   - After running dry, wait for a frame plus the margin to queue before
//     playing again, so unevenly arriving packets don't make it choppy.
//   - As a last resort, a backlog of more than 1.5 s is skipped outright.
//
// Nothing here follows any other clock: it only ever looks at its own
// queue, which changes slowly, so it never swings back and forth.
//
// The setting can be turned off (Voice & Video → "Smooth out network
// hiccups"); then trimVoiceQueue's plain skip-ahead is used instead.

// voiceSmoothing: whether playback is smoothed as above. On by default.
var voiceSmoothing atomic.Bool

func init() { voiceSmoothing.Store(true) }

// SetVoiceSmoothing turns smoothed voice playback on or off.
func (a *App) SetVoiceSmoothing(on bool) {
	if voiceSmoothing.Swap(on) != on {
		log.Printf("voice: smooth playback %s", map[bool]string{true: "on", false: "off"}[on])
	}
}

const (
	poFrame      = voiceFrameSize      // one frame: 20 ms at 48 kHz (mono)
	poSafetyMin  = voiceFrameSize      // spare kept for jitter, at least: 20 ms
	poSafetyMax  = 10 * voiceFrameSize // … and at most: 200 ms
	poCalm       = 15 * time.Second    // without running dry, before the margin shrinks a step
	poWindow     = 50                  // frames per measuring window: 1 s
	poMaxPauses  = 10                  // pause frames dropped per frame played, at most
	poTrimAt     = 48 * 150            // excess at which speech is trimmed too: 150 ms
	poTrimEvery  = 4                   // frames between speech trims, at least: 80 ms
	poHardMax    = 48 * 1500           // backlog skipped outright: over 1.5 s
	poSilentAbs  = 40.0                // RMS below which a frame is silent (about -58 dBFS)
	poSilentRel  = 0.03                // … or below this fraction of their recent speech level (-30 dB)
	poStatsEvery = 30 * time.Second
)

const poNone = math.MaxInt

// poNow is the clock (replaceable, so the timing can be tested).
var poNow = time.Now

// playout is one person's smoothing state (guarded by their source's mu).
type playout struct {
	who        string
	started    bool // playing (false: waiting for the prebuffer)
	safety     int  // spare kept for jitter, samples (adapts — see above)
	calmSince  time.Time
	curMin     int // least spare seen in this window, samples
	prevMin    int // … and in the last one
	ticks      int // frames into this window
	speechLvl  float64
	lastSilent bool // the frame played last was silent
	sinceTrim  int  // frames since the last speech trim

	stAt                         time.Time
	stPauses, stTrims, stTrimSmp int
	stDry, stSkips, stSkipSmp    int
	stMaxExcess                  int
}

func newPlayout(who string) playout {
	return playout{who: who, curMin: poNone, prevMin: poNone, sinceTrim: poTrimEvery, safety: poSafetyMin}
}

// next prepares the queue for the frame about to be played (call with the
// source's mu held). play false means play nothing this time (waiting for
// the prebuffer); fadeIn means what's played next follows a cut, and
// should be faded in so the join can't click.
func (p *playout) next(buf *[]int16) (play, fadeIn bool) {
	p.logStats()
	q := len(*buf)

	// The jitter margin: back down a step after a calm spell.
	if p.calmSince.IsZero() {
		p.calmSince = poNow()
	} else if p.safety > poSafetyMin && poNow().Sub(p.calmSince) >= poCalm {
		p.safety -= poFrame
		p.calmSince = poNow()
	}
	prebuffer := poFrame + p.safety

	if q > poHardMax { // a huge backlog (a long freeze): skip it
		drop := q - prebuffer
		*buf = (*buf)[drop:]
		p.stSkips++
		p.stSkipSmp += drop
		p.resetWindow()
		log.Printf("voice: %s's sound was %d ms behind (a long network freeze) — skipped ahead", p.who, q/48)
		q, fadeIn = len(*buf), true
	}
	if !p.started {
		if q < prebuffer {
			return false, fadeIn
		}
		p.started, fadeIn = true, true
		p.resetWindow()
	}
	if q == 0 {
		// Ran dry: wait for the prebuffer again — with a bigger margin, if
		// they were talking (packets came in too unevenly for it). Not
		// when they've just stopped sending (muted, left).
		if p.started && !p.lastSilent {
			p.safety = min(p.safety+poFrame, poSafetyMax)
			p.calmSince = poNow()
			p.stDry++
		}
		p.started = false
		return false, fadeIn
	}

	// How much is pure delay.
	spare := q - poFrame
	if spare < 0 {
		spare = 0
	}
	if spare < p.curMin {
		p.curMin = spare
	}
	if p.ticks++; p.ticks >= poWindow {
		p.prevMin, p.curMin, p.ticks = p.curMin, poNone, 0
	}
	floor := min(p.curMin, p.prevMin)
	excess := 0
	if floor != poNone {
		excess = floor - p.safety
	}
	if excess > p.stMaxExcess {
		p.stMaxExcess = excess
	}

	// Their recent speech level, for telling pauses from quiet speech.
	head := (*buf)[:min(poFrame, len(*buf))]
	if lvl := frameRMS(head); lvl > p.speechLvl {
		p.speechLvl = lvl
	} else {
		p.speechLvl *= 0.998 // halves in about 7 s
	}

	removed := 0
	// In a pause: drop whole silent frames.
	for p.lastSilent && excess-removed >= poFrame && removed < poMaxPauses*poFrame &&
		len(*buf) >= 2*poFrame && p.silent((*buf)[:poFrame]) {
		*buf = (*buf)[poFrame:]
		removed += poFrame
		p.stPauses++
	}
	// Far behind and they're talking: take out one pitch cycle.
	if removed == 0 && excess >= poTrimAt && p.sinceTrim >= poTrimEvery && len(*buf) >= poFrame+poFrame &&
		!p.silent((*buf)[:poFrame]) {
		if n := pitchTrim((*buf)[:poFrame]); n > 0 {
			*buf = (*buf)[n:]
			removed += n
			p.sinceTrim = 0
			p.stTrims++
			p.stTrimSmp += n
		}
	}
	p.sinceTrim++
	if removed > 0 {
		p.lowerWindow(removed)
		if removed >= poFrame {
			fadeIn = true // pitchTrim joins its own cut smoothly
		}
	}
	p.lastSilent = p.silent((*buf)[:min(poFrame, len(*buf))])
	return true, fadeIn
}

// silent: a frame with (next to) no sound in it, by absolute level or
// relative to how loud this person has been talking.
func (p *playout) silent(f []int16) bool {
	return frameRMS(f) < max(poSilentAbs, p.speechLvl*poSilentRel)
}

func (p *playout) resetWindow() { p.curMin, p.prevMin, p.ticks = poNone, poNone, 0 }

// lowerWindow: the queue just got n samples shorter, and so did every
// level measured in the windows (otherwise they'd still claim the removed
// audio as excess, and more would be taken out than was there).
func (p *playout) lowerWindow(n int) {
	if p.curMin != poNone {
		p.curMin = max(0, p.curMin-n)
	}
	if p.prevMin != poNone {
		p.prevMin = max(0, p.prevMin-n)
	}
}

// logStats: what smoothing did for this person, every 30 s if anything.
func (p *playout) logStats() {
	if p.stAt.IsZero() {
		p.stAt = poNow()
		return
	}
	if poNow().Sub(p.stAt) < poStatsEvery {
		return
	}
	if p.stPauses+p.stTrims+p.stSkips > 0 || p.stMaxExcess > 48*100 {
		log.Printf("voice: %s over %.0fs: %d ms of pauses shortened, %d ms of speech trimmed (%d cycles), %d ms skipped; most excess delay %d ms; ran dry %d times; jitter margin %d ms",
			p.who, poNow().Sub(p.stAt).Seconds(), p.stPauses*poFrame/48, p.stTrimSmp/48, p.stTrims, p.stSkipSmp/48, p.stMaxExcess/48, p.stDry, p.safety/48)
	}
	p.stPauses, p.stTrims, p.stTrimSmp, p.stDry, p.stSkips, p.stSkipSmp, p.stMaxExcess = 0, 0, 0, 0, 0, 0, 0
	p.stAt = poNow()
}

func frameRMS(f []int16) float64 {
	if len(f) == 0 {
		return 0
	}
	var s float64
	for _, v := range f {
		s += float64(v) * float64(v)
	}
	return math.Sqrt(s / float64(len(f)))
}

// pitchTrim removes one pitch cycle from the start of x (a frame of voiced
// speech) without changing its pitch, the way NetEQ's "accelerate" does.
// A voice repeats its waveform once per pitch period; the period P is
// found by comparing the start of the frame with itself shifted (the shift
// that matches best, between 2.5 and 10 ms — voices from 100 to 400 Hz).
// The first two periods are then blended into one: x[P:2P] is rewritten
// as a crossfade from x[0:P] to x[P:2P], so the caller drops x[:P] and
// the sound runs on seamlessly — from exactly where it was (x[0]) into
// exactly where it continues (x[2P]). Returns P, or 0 (x untouched) if no
// clear period was found — unvoiced sounds, noise — in which case nothing
// should be removed, since cutting those would be audible.
func pitchTrim(x []int16) int {
	const minLag, maxLag, minMatch = 120, 480, 0.9
	best, bestC := 0, minMatch
	for lag := minLag; lag <= maxLag && 2*lag <= len(x); lag++ {
		var xy, xx, yy float64
		for i := 0; i < lag; i++ {
			a, b := float64(x[i]), float64(x[i+lag])
			xy += a * b
			xx += a * a
			yy += b * b
		}
		if xx < 1 || yy < 1 {
			continue
		}
		// A close match only counts if the two periods are about as loud
		// (no blending the end of a word into silence).
		if r := xx / yy; r < 0.5 || r > 2 {
			continue
		}
		if c := xy / math.Sqrt(xx*yy); c > bestC {
			best, bestC = lag, c
		}
	}
	if best == 0 {
		return 0
	}
	for i := 0; i < best; i++ {
		w := (float64(i) + 0.5) / float64(best)
		x[best+i] = clampInt16(float64(x[i])*(1-w) + float64(x[best+i])*w)
	}
	return best
}

func clampInt16(v float64) int16 {
	return int16(math.Max(-32768, math.Min(32767, math.Round(v))))
}

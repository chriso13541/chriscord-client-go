package main

import (
	"log"
	"math"
	"math/cmplx"
	"sync"
	"sync/atomic"
	"time"
)

// Keeping this app's own sound out of a shared screen's sound.
//
// A whole-screen share records everything playing on the computer except
// this app (sysaudio_windows.c asks Windows to leave this process out).
// When that doesn't hold — a Windows build where the exclusion misbehaves,
// or the call's sound passing through another program on its way out
// (Voicemeeter, SteelSeries Sonar, a virtual cable…) — the call's voices
// end up in the stream and everyone hears themselves.
//
// So, as a safety net, what this app plays is kept for a few seconds
// (echoRef, filled by the playback loop in voice.go) and compared with what
// the share records. If the call's sound turns up in the recording, it's
// found (how much later), and taken back out with a short filter fitted
// to the last second of sound (least squares, refitted every second) —
// it's a digital copy, not something picked up by a microphone, so it
// cancels almost completely. If it never turns up, nothing is changed.

const (
	echoRate     = 48000
	echoRefCap   = 4 * echoRate     // reference kept: 4 s
	echoDecim    = 4                // the search for the delay runs at 12 kHz
	echoWin      = echoRate         // compared over the last 1 s
	echoLagMin   = -echoRate / 5    // −200 ms …
	echoLagMax   = echoRate * 3 / 5 // … +600 ms
	echoTaps     = 64               // filter length per channel (1.3 ms)
	echoPre      = 16               // of which before the main delay
	echoMu       = 0.25             // adaptation speed
	echoFoundRho = 0.35             // correlation that counts as "it's in there"
	echoWeakRho  = 0.04             // … or this much, if it stands out
	echoFoundZ   = 10               // this many standard deviations above the rest
	echoGapFill  = echoRate / 20    // a 50 ms gap in either stream is filled with silence
)

// ── What this app played ────────────────────────────────────────────────

type echoRefBuf struct {
	active atomic.Bool // collected only while a share's sound is running
	mu     sync.Mutex
	buf    []float32 // ring, stereo interleaved
	total  int64     // frames ever pushed (gaps filled), on the wall clock
	start  time.Time
}

var echoRef = &echoRefBuf{}

func (r *echoRefBuf) enable() {
	r.mu.Lock()
	if r.buf == nil {
		r.buf = make([]float32, echoRefCap*2)
	}
	for i := range r.buf {
		r.buf[i] = 0
	}
	r.total, r.start = 0, time.Now()
	r.mu.Unlock()
	r.active.Store(true)
}

func (r *echoRefBuf) disable() { r.active.Store(false) }

// fill keeps total in step with the wall clock: a stretch with nothing
// played (between calls, a device restart) becomes silence. Called locked.
func (r *echoRefBuf) fill(upcoming int) {
	want := int64(time.Since(r.start).Seconds()*echoRate) - int64(upcoming)
	gap := want - r.total
	if gap <= echoGapFill {
		return
	}
	if gap > echoRefCap {
		r.total += gap - echoRefCap
		gap = echoRefCap
	}
	for i := int64(0); i < gap; i++ {
		p := int((r.total+i)%echoRefCap) * 2
		r.buf[p], r.buf[p+1] = 0, 0
	}
	r.total += gap
}

// push: one block the playback loop has just handed to the speakers
// (int16, ch = 1 or 2 interleaved).
func (r *echoRefBuf) push(pcm []int16, ch int) {
	if !r.active.Load() || ch < 1 {
		return
	}
	n := len(pcm) / ch
	r.mu.Lock()
	r.fill(n)
	for i := 0; i < n; i++ {
		p := int((r.total+int64(i))%echoRefCap) * 2
		l := float32(pcm[i*ch]) / 32768
		rr := l
		if ch > 1 {
			rr = float32(pcm[i*ch+1]) / 32768
		}
		r.buf[p], r.buf[p+1] = l, rr
	}
	r.total += int64(n)
	r.mu.Unlock()
}

// now: the index the next pushed frame will get.
func (r *echoRefBuf) now() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fill(0)
	return r.total
}

// read copies frames [from, from+n) (stereo) into out; anything not (or no
// longer) there reads as silence.
func (r *echoRefBuf) read(from int64, out []float32) {
	n := len(out) / 2
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := 0; i < n; i++ {
		j := from + int64(i)
		if j < 0 || j >= r.total || j < r.total-echoRefCap {
			out[2*i], out[2*i+1] = 0, 0
			continue
		}
		p := int(j%echoRefCap) * 2
		out[2*i], out[2*i+1] = r.buf[p], r.buf[p+1]
	}
}

// ── Taking it back out of the share's sound ─────────────────────────────

type echoCanceller struct {
	ref   *echoRefBuf
	base  int64 // reference index that capture frame 0 lines up with (before the delay)
	start time.Time
	cap   int64 // capture frames seen

	hist    []float32 // last echoWin capture frames, mono at 12 kHz (ring)
	histPos int
	raw     []float32 // last echoWin capture frames as recorded, stereo (ring)
	rawPos  int
	dec     float32 // decimation accumulator
	decN    int

	frames    int  // processed since the last search
	locked    bool // the app's sound has been found in the recording
	lag       int  // … this many frames after its place on the reference
	candLag   int
	candSeen  bool
	w         [2][]float64
	accR      [2][echoTaps]float64 // the fit's sums, carried over (fading) from second to second
	accP      [2][echoTaps]float64
	refBlk    []float32
	inE, outE float64 // energy in/out since the last log line
	logged    time.Time
}

func newEchoCanceller(ref *echoRefBuf) *echoCanceller {
	e := &echoCanceller{ref: ref, base: ref.now(), start: time.Now(),
		hist: make([]float32, echoWin/echoDecim), raw: make([]float32, echoWin*2)}
	for c := range e.w {
		e.w[c] = make([]float64, echoTaps)
	}
	return e
}

// catchUp: the recording delivered nothing for a while (Windows may pause
// it during silence) — move its count on to the wall clock, so it keeps
// lining up with the reference.
func (e *echoCanceller) catchUp() {
	want := int64(time.Since(e.start).Seconds() * echoRate)
	if want-e.cap > 4*echoGapFill {
		e.skip(int(want - e.cap - echoGapFill))
	}
}

// skip: frames that went by without being processed.
func (e *echoCanceller) skip(n int) {
	e.cap += int64(n)
	for i := 0; i < n/echoDecim; i++ {
		e.hist[e.histPos] = 0
		e.histPos = (e.histPos + 1) % len(e.hist)
	}
	for i := 0; i < n && i < echoWin; i++ {
		e.raw[e.rawPos*2], e.raw[e.rawPos*2+1] = 0, 0
		e.rawPos = (e.rawPos + 1) % echoWin
	}
}

// process takes this app's sound out of one block of recorded sound
// (stereo float32, in place).
func (e *echoCanceller) process(f []float32) {
	n := len(f) / 2
	k0 := e.cap
	for i := 0; i < n; i++ { // keep the history for the search and the fit
		e.raw[e.rawPos*2], e.raw[e.rawPos*2+1] = f[2*i], f[2*i+1]
		e.rawPos = (e.rawPos + 1) % echoWin
		e.dec += (f[2*i] + f[2*i+1]) / 2
		if e.decN++; e.decN == echoDecim {
			e.hist[e.histPos] = e.dec / echoDecim
			e.histPos = (e.histPos + 1) % len(e.hist)
			e.dec, e.decN = 0, 0
		}
	}
	e.cap += int64(n)
	e.frames += n
	every := echoRate / 2
	if e.locked {
		every = echoRate
	}
	if e.frames >= every && e.cap >= echoWin {
		e.frames = 0
		e.search()
		if e.locked {
			e.fit()
		}
	}
	if !e.locked {
		return
	}
	// The reference this block's echo comes from, with room for the taps.
	first := e.base + k0 - int64(e.lag) - (echoTaps - 1 - echoPre)
	need := (n + echoTaps - 1) * 2
	if cap(e.refBlk) < need {
		e.refBlk = make([]float32, need)
	}
	x := e.refBlk[:need]
	e.ref.read(first, x)
	var inE, outE float64
	for c := 0; c < 2; c++ {
		w := e.w[c]
		for i := 0; i < n; i++ {
			// x[i .. i+taps-1] is this sample's window, newest last.
			var y float64
			for t := 0; t < echoTaps; t++ {
				y += w[t] * float64(x[2*(i+echoTaps-1-t)+c])
			}
			d := float64(f[2*i+c])
			out := d - y
			inE += d * d
			outE += out * out
			f[2*i+c] = float32(out)
		}
	}
	// Gone wrong (made it louder): stop until the next fit.
	if outE > 2*inE+1e-4 {
		for c := range e.w {
			for t := range e.w[c] {
				e.w[c][t] = 0
			}
		}
	}
	e.inE += inE
	e.outE += outE
	if time.Since(e.logged) >= 30*time.Second {
		if e.outE > 0 && e.inE > 0 {
			log.Printf("screen: sound — removing this app's own sound from the share (%.0f ms behind, level change %.1f dB)",
				float64(e.lag)*1000/echoRate, 10*math.Log10(e.outE/e.inE))
		}
		e.inE, e.outE, e.logged = 0, 0, time.Now()
	}
}

// search looks for the reference in the last second of the recording:
// where (if anywhere) it is, and how loud.
func (e *echoCanceller) search() {
	nc := len(e.hist)
	c := make([]float64, nc)
	for i := 0; i < nc; i++ {
		c[i] = float64(e.hist[(e.histPos+i)%nc])
	}
	// Capture frames [k0, k0+echoWin) ↔ reference base+k−lag.
	k0 := e.cap - echoWin
	refFirst := e.base + k0 - echoLagMax
	refLen := echoWin + echoLagMax - echoLagMin
	full := make([]float32, refLen*2)
	e.ref.read(refFirst, full)
	nr := refLen / echoDecim
	r := make([]float64, nr)
	var rE float64
	for i := 0; i < nr; i++ {
		var s float64
		for j := 0; j < echoDecim; j++ {
			p := (i*echoDecim + j) * 2
			s += float64(full[p]+full[p+1]) / 2
		}
		r[i] = s / echoDecim
		rE += r[i] * r[i]
	}
	var cE float64
	for _, v := range c {
		cE += v * v
	}
	if rE/float64(nr) < 1e-7 || cE/float64(nc) < 1e-8 {
		return // nothing played (or recorded) to compare
	}
	corr := xcorr(c, r)
	pre := make([]float64, nr+1) // running energy of r
	for i, v := range r {
		pre[i+1] = pre[i] + v*v
	}
	// The best match, and how far it stands out from all the others (with
	// loud game or music sound recorded too, the match is weak in absolute
	// terms but still a sharp spike at one delay).
	best, bestM := 0.0, -1
	var sum, sum2 float64
	cnt := 0
	for m := 0; m+nc <= nr; m++ {
		er := pre[m+nc] - pre[m]
		if er < 1e-9 {
			continue
		}
		rho := corr[m] / math.Sqrt(cE*er)
		sum += rho
		sum2 += rho * rho
		cnt++
		if rho > best {
			best, bestM = rho, m
		}
	}
	if bestM < 0 || cnt < 2 {
		e.candSeen = false
		return
	}
	mean := sum / float64(cnt)
	sd := math.Sqrt(math.Max(sum2/float64(cnt)-mean*mean, 1e-12))
	z := (best - mean) / sd
	if best < echoFoundRho && !(best >= echoWeakRho && z >= echoFoundZ) {
		e.candSeen = false
		return
	}
	// (Good to within the 12 kHz grid; the taps either side cover the rest.)
	lag := echoLagMax - bestM*echoDecim
	// Found twice in the same place (or very clearly once): that's it.
	if !((best > 0.6 && z >= echoFoundZ) || (e.candSeen && abs(lag-e.candLag) <= echoRate/1000)) {
		e.candLag, e.candSeen = lag, true
		return
	}
	e.candLag, e.candSeen = lag, true
	if e.locked && abs(lag-e.lag) <= 2 {
		return
	}
	if !e.locked {
		log.Printf("screen: sound — this app's own sound is getting into the share (Windows isn't leaving it out here); removing it (%.0f ms behind, match %.2f, %.0f× above chance)",
			float64(lag)*1000/echoRate, best, z)
	}
	e.locked, e.lag = true, lag
	for ch := range e.w {
		for t := range e.w[ch] {
			e.w[ch][t] = 0
		}
	}
	e.accR, e.accP = [2][echoTaps]float64{}, [2][echoTaps]float64{}
	e.logged = time.Now()
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// fit works out the filter that best turns the reference into what was
// recorded over the last second (per channel), so subtracting it leaves
// everything else. Least squares with the reference's autocorrelation
// (a Toeplitz system) — a 32×32 solve.
func (e *echoCanceller) fit() {
	const T = echoTaps
	k0 := e.cap - echoWin
	first := e.base + k0 - int64(e.lag) - (T - 1 - echoPre)
	x := make([]float32, (echoWin+T-1)*2)
	e.ref.read(first, x)
	for c := 0; c < 2; c++ {
		// Autocorrelation of the reference, and its correlation with the
		// recording at each tap.
		var r [T]float64
		var p [T]float64
		for i := 0; i < echoWin; i++ {
			d := float64(e.raw[((e.rawPos+i)%echoWin)*2+c])
			base := i + T - 1
			xi := float64(x[2*base+c])
			for t := 0; t < T; t++ {
				xt := float64(x[2*(base-t)+c])
				r[t] += xi * xt
				p[t] += d * xt
			}
		}
		if r[0] < 1e-4 {
			continue // too little played to fit to; keep the last filter
		}
		// Earlier seconds count too (fading out over a few seconds): with
		// loud game or music sound in the recording, one second alone gives
		// a noisy fit.
		for t := 0; t < T; t++ {
			e.accR[c][t] = 0.75*e.accR[c][t] + r[t]
			e.accP[c][t] = 0.75*e.accP[c][t] + p[t]
		}
		r, p = e.accR[c], e.accP[c]
		var m [T][T + 1]float64
		for a := 0; a < T; a++ {
			for b := 0; b < T; b++ {
				d := a - b
				if d < 0 {
					d = -d
				}
				m[a][b] = r[d]
			}
			m[a][a] += 1e-4*r[0] + 1e-9
			m[a][T] = p[a]
		}
		w, ok := solve(&m)
		if !ok {
			continue
		}
		copy(e.w[c], w[:])
	}
}

// solve: Gaussian elimination with partial pivoting on an augmented matrix.
func solve(m *[echoTaps][echoTaps + 1]float64) ([echoTaps]float64, bool) {
	const n = echoTaps
	var out [n]float64
	for col := 0; col < n; col++ {
		piv := col
		for r := col + 1; r < n; r++ {
			if math.Abs(m[r][col]) > math.Abs(m[piv][col]) {
				piv = r
			}
		}
		if math.Abs(m[piv][col]) < 1e-15 {
			return out, false
		}
		m[col], m[piv] = m[piv], m[col]
		for r := col + 1; r < n; r++ {
			f := m[r][col] / m[col][col]
			for k := col; k <= n; k++ {
				m[r][k] -= f * m[col][k]
			}
		}
	}
	for r := n - 1; r >= 0; r-- {
		v := m[r][n]
		for k := r + 1; k < n; k++ {
			v -= m[r][k] * out[k]
		}
		out[r] = v / m[r][r]
	}
	return out, true
}

// xcorr: out[m] = Σ c[n]·r[n+m] for m = 0 … len(r)−len(c), via FFT.
func xcorr(c, r []float64) []float64 {
	size := 1
	for size < len(r) {
		size <<= 1
	}
	a := make([]complex128, size)
	b := make([]complex128, size)
	for i, v := range c {
		a[i] = complex(v, 0)
	}
	for i, v := range r {
		b[i] = complex(v, 0)
	}
	fft(a, false)
	fft(b, false)
	for i := range a {
		a[i] = cmplx.Conj(a[i]) * b[i]
	}
	fft(a, true)
	out := make([]float64, len(r)-len(c)+1)
	for m := range out {
		out[m] = real(a[m]) / float64(size)
	}
	return out
}

// fft: in-place radix-2 FFT (len(a) a power of two); inverse unscaled.
func fft(a []complex128, inverse bool) {
	n := len(a)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		ang := 2 * math.Pi / float64(size)
		if !inverse {
			ang = -ang
		}
		wn := complex(math.Cos(ang), math.Sin(ang))
		for start := 0; start < n; start += size {
			w := complex(1, 0)
			for k := 0; k < size/2; k++ {
				u, v := a[start+k], a[start+k+size/2]*w
				a[start+k], a[start+k+size/2] = u+v, u-v
				w *= wn
			}
		}
	}
}

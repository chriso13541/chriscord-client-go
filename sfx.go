package main

// The app's own sound effects (join, leave, mute, mention…) played from
// here rather than by the page — used on Linux, where the page can't play
// them well: WebKitGTK's <audio> can't load the app's own files at all,
// and its Web Audio output crackles and stops after a sound or two. Here
// they go out the same way the call's voices do (PortAudio), so they work
// wherever the call's sound does, on the speaker chosen in Settings.
//
//   - In a call, they're mixed into the call's own output (VoiceSession.sfx,
//     in the playback loop in voice.go): same device, nothing else opened.
//   - Otherwise a small output stream is opened on the chosen speaker while
//     sounds are playing, and closed again a second after the last one.
//
// The .wav files are read from the page's own files (frontend/sounds,
// embedded in the app), decoded once and kept, resampled to 48 kHz.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math"
	"regexp"
	"runtime"
	"sync"
	"time"

	"github.com/gordonklaus/portaudio"
)

// sfxQueue mixes any number of sounds, 48 kHz mono, as they're asked for.
type sfxQueue struct {
	mu      sync.Mutex
	playing []sfxVoice
}

type sfxVoice struct {
	pcm  []float32
	pos  int
	gain float64
}

func (q *sfxQueue) add(pcm []float32, gain float64) {
	q.mu.Lock()
	q.playing = append(q.playing, sfxVoice{pcm: pcm, gain: gain})
	q.mu.Unlock()
}

func (q *sfxQueue) busy() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.playing) > 0
}

// mixInto adds the next len(buf)/ch samples of everything playing to buf
// (interleaved, ch channels — the same sound on each).
func (q *sfxQueue) mixInto(buf []int16, ch int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.playing) == 0 {
		return
	}
	frames := len(buf) / ch
	kept := q.playing[:0]
	for _, v := range q.playing {
		n := min(frames, len(v.pcm)-v.pos)
		for i := 0; i < n; i++ {
			s := float64(v.pcm[v.pos+i]) * v.gain * 32767
			for c := 0; c < ch; c++ {
				mixInto(&buf[i*ch+c], s)
			}
		}
		v.pos += n
		if v.pos < len(v.pcm) {
			kept = append(kept, v)
		}
	}
	q.playing = kept
}

// ── The sounds ──────────────────────────────────────────────────────────

var (
	sfxNameOK = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)
	sfxMu     sync.Mutex
	sfxCache  = map[string][]float32{}
)

func sfxSound(name string) ([]float32, error) {
	if !sfxNameOK.MatchString(name) {
		return nil, errors.New("not a sound name")
	}
	sfxMu.Lock()
	defer sfxMu.Unlock()
	if pcm, ok := sfxCache[name]; ok {
		return pcm, nil
	}
	data, err := assets.ReadFile("frontend/sounds/" + name + ".wav")
	if err != nil {
		return nil, err
	}
	pcm, err := decodeWav48k(data)
	if err != nil {
		return nil, fmt.Errorf("%s.wav: %w", name, err)
	}
	sfxCache[name] = pcm
	return pcm, nil
}

// decodeWav48k: a PCM .wav (8/16/24/32-bit integer or 32-bit float, any
// channels) as 48 kHz mono float samples.
func decodeWav48k(b []byte) ([]float32, error) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, errors.New("not a WAV file")
	}
	var format, ch, bits int
	var rate int
	var data []byte
	for o := 12; o+8 <= len(b); {
		id, n := string(b[o:o+4]), int(binary.LittleEndian.Uint32(b[o+4:]))
		body := o + 8
		end := min(body+n, len(b))
		switch id {
		case "fmt ":
			if end-body >= 16 {
				format = int(binary.LittleEndian.Uint16(b[body:]))
				ch = int(binary.LittleEndian.Uint16(b[body+2:]))
				rate = int(binary.LittleEndian.Uint32(b[body+4:]))
				bits = int(binary.LittleEndian.Uint16(b[body+14:]))
			}
		case "data":
			data = b[body:end]
		}
		if data != nil {
			break
		}
		o = body + n + n&1
	}
	if format == 0xFFFE {
		format = 1
	}
	bytes := bits / 8
	if ch < 1 || rate < 1000 || data == nil || bytes < 1 || bytes > 4 || (format != 1 && !(format == 3 && bytes == 4)) {
		return nil, errors.New("not a PCM WAV file")
	}
	frames := len(data) / (bytes * ch)
	mono := make([]float64, frames)
	for i := 0; i < frames; i++ {
		sum := 0.0
		for c := 0; c < ch; c++ {
			p := data[(i*ch+c)*bytes:]
			var v float64
			switch {
			case format == 3:
				v = float64(math.Float32frombits(binary.LittleEndian.Uint32(p)))
			case bytes == 1:
				v = (float64(p[0]) - 128) / 128
			case bytes == 2:
				v = float64(int16(binary.LittleEndian.Uint16(p))) / 32768
			case bytes == 3:
				v = float64(int32(uint32(p[0])|uint32(p[1])<<8|uint32(p[2])<<16)<<8>>8) / 8388608
			default:
				v = float64(int32(binary.LittleEndian.Uint32(p))) / 2147483648
			}
			sum += v
		}
		mono[i] = sum / float64(ch)
	}
	// To 48 kHz, by straight-line interpolation (fine for short effects).
	out := make([]float32, int(int64(frames)*voiceSampleRate/int64(rate)))
	step := float64(rate) / voiceSampleRate
	for i := range out {
		x := float64(i) * step
		j := int(x)
		f := x - float64(j)
		a := mono[min(j, frames-1)]
		b := mono[min(j+1, frames-1)]
		out[i] = float32(a + (b-a)*f)
	}
	return out, nil
}

// ── Playing one ─────────────────────────────────────────────────────────

// PlaySound plays one of the app's sounds (frontend/sounds/<name>.wav) at
// volume (0–1) on speaker (the device label from Settings > Voice; "" for
// the default).
func (a *App) PlaySound(name string, volume float64, speaker string) error {
	pcm, err := sfxSound(name)
	if err != nil {
		return err
	}
	volume = math.Max(0, math.Min(1, volume))
	if volume == 0 {
		return nil
	}
	// In a call: into the call's own output.
	a.voiceMu.Lock()
	s := a.voice
	a.voiceMu.Unlock()
	if s != nil && s.playStream != nil {
		select {
		case <-s.stopped:
		default:
			s.sfx.add(pcm, volume)
			return nil
		}
	}
	sfxOut.queue.add(pcm, volume)
	return sfxOut.ensure(speaker)
}

// sfxPlayer: the stream sounds play on outside a call.
type sfxPlayer struct {
	queue   sfxQueue
	mu      sync.Mutex // guards running
	running bool
}

var sfxOut sfxPlayer

// ensure: the stream is open (opening it if not), playing what's queued.
func (o *sfxPlayer) ensure(speaker string) error {
	o.mu.Lock()
	if o.running {
		o.mu.Unlock()
		return nil
	}
	o.running = true
	o.mu.Unlock()

	started := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		finished := false // running was cleared by the idle check below
		defer func() {
			if !finished {
				o.mu.Lock()
				o.running = false
				o.mu.Unlock()
			}
		}()
		device, err := findDeviceByName(speaker, false)
		if err != nil {
			started <- err
			return
		}
		ch := 2
		if device.MaxOutputChannels < 2 {
			ch = 1
		}
		buf := make([]int16, voiceFrameSize*ch)
		stream, err := portaudio.OpenStream(portaudio.StreamParameters{
			Output:          portaudio.StreamDeviceParameters{Device: device, Channels: ch, Latency: device.DefaultLowOutputLatency},
			SampleRate:      voiceSampleRate,
			FramesPerBuffer: voiceFrameSize,
		}, buf)
		if err != nil {
			started <- err
			return
		}
		defer stream.Close()
		if err := stream.Start(); err != nil {
			started <- err
			return
		}
		defer stream.Stop()
		started <- nil
		idle := 0
		for {
			if idle >= 50 { // a second of nothing: close (unless one just came)
				o.mu.Lock()
				if !o.queue.busy() {
					o.running, finished = false, true
					o.mu.Unlock()
					return
				}
				o.mu.Unlock()
				idle = 0
			}
			for i := range buf {
				buf[i] = 0
			}
			if o.queue.busy() {
				idle = 0
				o.queue.mixInto(buf, ch)
			} else {
				idle++
			}
			if err := stream.Write(); err != nil {
				log.Printf("sfx: speaker stopped: %v", err)
				return
			}
		}
	}()
	select {
	case err := <-started:
		if err != nil {
			o.queue.mu.Lock()
			o.queue.playing = nil
			o.queue.mu.Unlock()
			log.Printf("sfx: couldn't open the speaker: %v", err)
		}
		return err
	case <-time.After(3 * time.Second):
		return nil
	}
}

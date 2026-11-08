//go:build linux

package main

// The computer's sound for a screen share on Linux: the monitor of the
// default output — everything that's playing — recorded by PulseAudio's
// parec, or PipeWire's pw-record where there's no parec. Both work on a
// PipeWire desktop (pipewire-pulse answers parec), so nothing needs adding
// on the usual distros; the first one found is used.
//
// Unlike Windows, this can't leave Chriscord's own sound out (or record
// just one app), so the call's voices are in the recording too:
// runScreenAudio's echo canceller (echo.go) finds them and takes them back
// out. pid and exclude are ignored.
//
// The recording arrives as 48 kHz stereo float32 on the program's stdout,
// and is kept here (up to 2 s) until runScreenAudio reads it.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const saMaxFrames = 48000 * 2 // keep at most 2 s

var sa struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	buf  []float32 // interleaved stereo, oldest first
	gen  int       // bumped by stop, so an old reader can't add to a new recording
	what string    // which program, for the log
}

// saRecorders: the ways to record the output's monitor, in order.
func saRecorders() [][]string {
	return [][]string{
		{"parec", "--device=@DEFAULT_MONITOR@", "--format=float32le", "--rate=48000", "--channels=2", "--raw", "--latency-msec=20",
			"--client-name=Chriscord", "--stream-name=Screen share sound"},
		{"pw-record", "-P", "{ stream.capture.sink = true node.name = chriscord-screen-sound }", "--format=f32", "--rate=48000", "--channels=2", "--latency=20ms", "-"},
	}
}

func startSystemAudio(pid uint32, exclude bool) error {
	stopSystemAudio()
	var tried []string
	for _, argv := range saRecorders() {
		path, err := exec.LookPath(argv[0])
		if err != nil {
			continue
		}
		err = saStart(path, argv)
		if err == nil {
			return nil
		}
		tried = append(tried, fmt.Sprintf("%s: %v", argv[0], err))
	}
	if len(tried) == 0 {
		return errors.New("sharing sound needs parec (pulseaudio-utils) or pw-record (pipewire) — neither is installed")
	}
	return errors.New("couldn't record the computer's sound (" + strings.Join(tried, "; ") + ")")
}

func saStart(path string, argv []string) error {
	cmd := exec.Command(path, argv[1:]...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, n: 2048}
	if err := cmd.Start(); err != nil {
		return err
	}
	sa.mu.Lock()
	sa.gen++
	gen := sa.gen
	sa.cmd, sa.buf, sa.what = cmd, sa.buf[:0], argv[0]
	sa.mu.Unlock()

	started := make(chan struct{}, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		saRead(stdout, gen, started)
		cmd.Wait()
	}()
	// It's working once sound arrives — or it's still running a moment
	// later (a monitor of a sink that's asleep sends nothing until something
	// plays).
	select {
	case <-started:
	case <-time.After(1500 * time.Millisecond):
	case <-exited:
		sa.mu.Lock()
		if sa.gen == gen {
			sa.cmd = nil
		}
		sa.mu.Unlock()
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = "it stopped straight away"
		}
		return errors.New(lastLine([]byte(msg)))
	}
	log.Printf("sysaudio: recording the computer's sound with %s", argv[0])
	return nil
}

// saRead turns the bytes into samples and queues them, dropping the oldest
// past saMaxFrames.
func saRead(r io.Reader, gen int, started chan<- struct{}) {
	raw := make([]byte, 8192)
	var carry []byte
	first := true
	for {
		n, err := r.Read(raw)
		if n > 0 {
			data := append(carry, raw[:n]...)
			whole := len(data) / 8 * 8 // whole stereo frames
			samples := make([]float32, whole/4)
			for i := range samples {
				samples[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[i*4:]))
			}
			carry = append([]byte(nil), data[whole:]...)
			sa.mu.Lock()
			if sa.gen != gen {
				sa.mu.Unlock()
				return
			}
			sa.buf = append(sa.buf, samples...)
			if over := len(sa.buf) - saMaxFrames*2; over > 0 {
				over += over & 1
				sa.buf = append(sa.buf[:0], sa.buf[over:]...)
			}
			sa.mu.Unlock()
			if first {
				first = false
				select {
				case started <- struct{}{}:
				default:
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// readSystemAudio fills out (interleaved stereo float32) with the oldest
// sound waiting and says how many frames it got.
func readSystemAudio(out []float32) int {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	n := len(out) / 2 * 2
	if n > len(sa.buf) {
		n = len(sa.buf) / 2 * 2
	}
	copy(out, sa.buf[:n])
	sa.buf = append(sa.buf[:0], sa.buf[n:]...)
	return n / 2
}

// systemAudioAvailable: frames waiting.
func systemAudioAvailable() int {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	return len(sa.buf) / 2
}

func stopSystemAudio() {
	sa.mu.Lock()
	cmd := sa.cmd
	sa.cmd = nil
	sa.gen++
	sa.buf = sa.buf[:0]
	what := sa.what
	sa.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill() // its reader goroutine Waits for it
		log.Printf("sysaudio: stopped %s", what)
	}
}

package main

// H.264 video decoded by the app's own FFmpeg instead of the web view —
// used on Linux. There the web view (WebKitGTK) decodes through whatever
// GStreamer plugins the distro has: Fedora's only H.264 decoder by default
// is OpenH264, which locks the whole window up, and other distros may have
// none at all. The bundled FFmpeg is the same everywhere, so it decodes
// each H.264 stream (people's screens and cameras, and your own previews)
// and hands the page JPEG pictures, which the web view draws with its
// ordinary image decoder — no GStreamer involved.
//
// One FFmpeg per stream, started at the stream's first keyframe and
// stopped after a few seconds without frames. Frames reach it through a
// short queue: if it falls behind (a slow computer), frames are dropped up
// to the next keyframe rather than piling up. Each picture keeps the
// capture time of the frame it came from (the page syncs sound to it).
//
// VP8 (some cameras) still goes to the web view: every distro decodes it
// fine. CHRISCORD_VIDEO_DECODE=webview turns this off (webview_linux.go).

import (
	"bufio"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// nativeH264 is set on Linux (webview_linux.go).
var nativeH264 bool

const (
	nativeDecQueue = 8               // frames waiting for FFmpeg at most
	nativeDecIdle  = 5 * time.Second // no frames this long: stop its FFmpeg
)

type nativeDec struct {
	kind  byte
	user  string
	in    chan nativeAU
	caps  chan float64 // capture times, in the order the pictures will come out
	last  time.Time
	needK bool // dropping until a keyframe (fell behind)
	dead  bool
}

type nativeAU struct {
	data  []byte
	capMs float64
}

var nativeDecs struct {
	sync.Mutex
	m       map[string]*nativeDec
	checked bool
	ok      bool
	janitor bool
}

// nativeDecodeAvailable: FFmpeg is there and can turn H.264 into JPEG.
func nativeDecodeAvailable() bool {
	if nativeDecs.checked {
		return nativeDecs.ok
	}
	nativeDecs.checked = true
	if findFFmpeg() == "" {
		log.Printf("videodec: no FFmpeg — H.264 goes to the web view")
		return false
	}
	out, err := ffmpegCmd("-hide_banner", "-encoders").Output()
	if err != nil || !strings.Contains(string(out), " mjpeg ") {
		log.Printf("videodec: this FFmpeg has no JPEG encoder (rebuild it: ffmpeg/build-ffmpeg.sh linux) — H.264 goes to the web view")
		return false
	}
	nativeDecs.ok = true
	log.Printf("videodec: H.264 is decoded by FFmpeg, not the web view")
	return true
}

// nativeDecode takes an H.264 frame meant for the page. false: not handled
// here (the caller passes it to the page as it is).
func nativeDecode(kind byte, user string, key bool, capMs float64, au []byte) bool {
	nativeDecs.Lock()
	defer nativeDecs.Unlock()
	if !nativeDecodeAvailable() {
		return false
	}
	if nativeDecs.m == nil {
		nativeDecs.m = map[string]*nativeDec{}
	}
	if !nativeDecs.janitor {
		nativeDecs.janitor = true
		go nativeJanitor()
	}
	id := string([]byte{kind}) + user
	d := nativeDecs.m[id]
	if d != nil && d.dead {
		delete(nativeDecs.m, id)
		d = nil
	}
	if d == nil {
		if !key {
			return true // nothing to start from yet: wait for a keyframe
		}
		var err error
		if d, err = startNativeDec(kind, user); err != nil {
			log.Printf("videodec: couldn't start FFmpeg: %v", err)
			return true
		}
		nativeDecs.m[id] = d
	}
	d.last = time.Now()
	if d.needK {
		if !key {
			return true
		}
		d.needK = false
	}
	select {
	case d.in <- nativeAU{data: au, capMs: capMs}:
	default:
		d.needK = true // FFmpeg is behind: skip to the next keyframe
	}
	return true
}

func startNativeDec(kind byte, user string) (*nativeDec, error) {
	// low_delay + one thread: a picture comes out for each frame as soon as
	// the next one starts, rather than a few frames later. (Not
	// -fflags nobuffer: with a raw H.264 input it produces nothing.)
	cmd := ffmpegCmd("-hide_banner", "-nostdin", "-loglevel", "error",
		"-probesize", "32", "-analyzeduration", "0", "-flags", "low_delay", "-threads", "1",
		"-f", "h264", "-i", "pipe:0",
		"-an", "-vf", "scale=out_range=full", "-color_range", "pc",
		"-c:v", "mjpeg", "-q:v", "4", "-strict", "unofficial", "-flush_packets", "1",
		"-f", "mjpeg", "pipe:1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, n: 2048}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	d := &nativeDec{kind: kind, user: user, in: make(chan nativeAU, nativeDecQueue), caps: make(chan float64, 256), last: time.Now()}

	go func() { // frames in
		defer stdin.Close()
		for au := range d.in {
			select {
			case d.caps <- au.capMs:
			default:
			}
			if _, err := stdin.Write(au.data); err != nil {
				return
			}
		}
	}()
	go func() { // pictures out
		r := bufio.NewReaderSize(stdout, 256<<10)
		for {
			jpg, err := readJPEG(r)
			if err != nil {
				break
			}
			capMs := 0.0
			select {
			case capMs = <-d.caps:
			default:
			}
			videoOut.pushJPEG(kind, user, capMs, jpg)
		}
		cmd.Wait()
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			log.Printf("videodec: FFmpeg for %q stopped: %s", user, lastLine([]byte(msg)))
		}
		nativeDecs.Lock()
		if !d.dead {
			d.dead = true
			close(d.in) // lets the frames-in goroutine finish
		}
		nativeDecs.Unlock()
	}()
	log.Printf("videodec: decoding %q's video with FFmpeg", user)
	return d, nil
}

// nativeJanitor stops the FFmpeg of a stream that's gone quiet.
func nativeJanitor() {
	for range time.Tick(time.Second) {
		nativeDecs.Lock()
		for id, d := range nativeDecs.m {
			if d.dead || time.Since(d.last) > nativeDecIdle {
				if !d.dead {
					d.dead = true
					close(d.in) // its FFmpeg finishes and exits
				}
				delete(nativeDecs.m, id)
			}
		}
		nativeDecs.Unlock()
	}
}

// readJPEG reads one JPEG (SOI … EOI) from FFmpeg's output, following its
// segments so an FF D9 inside a table can't end it early.
func readJPEG(r *bufio.Reader) ([]byte, error) {
	// Find the start: FF D8.
	var prev byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if prev == 0xFF && b == 0xD8 {
			break
		}
		prev = b
	}
	out := make([]byte, 0, 128<<10)
	out = append(out, 0xFF, 0xD8)
	pending := byte(0) // a marker code already read (after picture data)
	for {
		code := pending
		pending = 0
		if code == 0 {
			// A marker: FF (possibly repeated), then its code.
			b, err := r.ReadByte()
			if err != nil {
				return nil, err
			}
			if b != 0xFF {
				return nil, io.ErrUnexpectedEOF // not where a marker should be
			}
			for code = 0xFF; code == 0xFF; {
				if code, err = r.ReadByte(); err != nil {
					return nil, err
				}
			}
		}
		out = append(out, 0xFF, code)
		switch {
		case code == 0xD9: // EOI
			return out, nil
		case code == 0x01 || (code >= 0xD0 && code <= 0xD7): // no length
			continue
		}
		var lenb [2]byte
		if _, err := io.ReadFull(r, lenb[:]); err != nil {
			return nil, err
		}
		n := int(lenb[0])<<8 | int(lenb[1])
		if n < 2 {
			return nil, io.ErrUnexpectedEOF
		}
		out = append(out, lenb[:]...)
		seg := make([]byte, n-2)
		if _, err := io.ReadFull(r, seg); err != nil {
			return nil, err
		}
		out = append(out, seg...)
		if code != 0xDA { // not SOS: the next marker follows
			continue
		}
		// The picture data: up to the next marker that isn't a stuffed
		// FF 00 or a restart (FF D0–D7).
		for pending == 0 {
			b, err := r.ReadByte()
			if err != nil {
				return nil, err
			}
			if b != 0xFF {
				out = append(out, b)
				continue
			}
			nx, err := r.ReadByte()
			if err != nil {
				return nil, err
			}
			switch {
			case nx == 0x00 || (nx >= 0xD0 && nx <= 0xD7):
				out = append(out, b, nx)
			case nx == 0xFF: // fill byte before a marker
				r.UnreadByte() // (straight after a ReadByte: allowed)
			default:
				pending = nx
			}
		}
	}
}

var _ = exec.Command // (ffmpegCmd builds the command)

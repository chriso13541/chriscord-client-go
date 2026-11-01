package main

// media.go — video frames to the page as raw bytes.
//
// Every video frame the page shows (people's cameras and screens in the
// call, and your own camera and screen previews) used to go through Wails
// events: base64, inside JSON, escaped into a script that ran on the
// window's main thread, then parsed and decoded back to bytes on the page.
// Now they wait here and the page asks for them over the app's own asset
// handler (main.go): GET /cc-media/frames?after=N returns every frame after
// number N as raw bytes, at once if there are any, or as soon as the next
// one comes in (up to mediaWait). The page asks again straight away, so
// there's always one request waiting.
//
// Wails v2 can't stream a response on Windows (it hands WebView2 the whole
// body when the handler returns), hence a request per batch rather than one
// long response. The asset handler is only reachable by the app's own window:
// no network port is opened, so nothing else on the computer can read the
// (decrypted) frames.
//
// The response body is the frames one after another, each:
//
//	u8  kind    0 camera, 1 screen, 2 your camera's preview, 3 your screen's preview
//	u8  flags   1 keyframe, 2 H.264 (else VP8)
//	u16 user    length of the user name, in bytes
//	f64 cap     screens: capture time on the sharer's clock, ms (0: none)
//	u32 size    the frame's length
//	user name, then the frame
//
// all little-endian. Headers: X-Media-Last is the number to ask after next;
// X-Media-Gap: 1 means frames were dropped since (the page fell behind), so
// every picture waits for its next keyframe.

import (
	"context"
	"encoding/binary"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	mediaPath      = "/cc-media/frames"
	mediaWait      = 5 * time.Second // an empty answer after this long (the page asks again)
	mediaMaxFrames = 600             // waiting frames kept at most (≈ 3 s of 3 streams at 60 fps)
	mediaMaxBytes  = 24 << 20
	mediaMaxReply  = 8 << 20 // bytes per answer at most (the rest in the next)
)

// Frame kinds, as the page reads them.
const (
	mediaCamera        byte = 0
	mediaScreen        byte = 1
	mediaCameraPreview byte = 2
	mediaScreenPreview byte = 3
)

type mediaFrame struct {
	seq  uint64
	kind byte
	key  bool
	h264 bool
	cap  float64
	user string
	data []byte
}

type mediaHub struct {
	mu     sync.Mutex
	seq    uint64       // the last frame's number
	frames []mediaFrame // waiting, oldest first
	bytes  int
	wake   chan struct{} // closed when a frame comes in
}

var videoOut = &mediaHub{wake: make(chan struct{})}

// push adds a frame for the page. data is kept as it is (not copied): the
// caller mustn't change it afterwards.
func (h *mediaHub) push(kind byte, user, codec string, key bool, capMs float64, data []byte) {
	h.mu.Lock()
	h.seq++
	h.frames = append(h.frames, mediaFrame{seq: h.seq, kind: kind, key: key, h264: codec == "h264", cap: capMs, user: user, data: data})
	h.bytes += len(data)
	// Nobody's reading fast enough (or at all: the page is reloading):
	// drop the oldest. The page notices the gap and waits for keyframes.
	drop := 0
	for len(h.frames)-drop > mediaMaxFrames || (h.bytes > mediaMaxBytes && len(h.frames)-drop > 1) {
		h.bytes -= len(h.frames[drop].data)
		drop++
	}
	if drop > 0 {
		clear(h.frames[:drop])
		h.frames = h.frames[drop:]
	}
	close(h.wake)
	h.wake = make(chan struct{})
	h.mu.Unlock()
}

// take: the frames after `after` (up to mediaMaxReply bytes), waiting for
// one up to `wait`. after 0 means "from now on" (the page just started).
// last is what to ask after next; gap, that frames after `after` were
// dropped before they could be taken.
func (h *mediaHub) take(ctx context.Context, after uint64, wait time.Duration) (out []mediaFrame, last uint64, gap bool) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	h.mu.Lock()
	if after == 0 || after > h.seq { // starting, or a number from before a restart: from now on
		after = h.seq
	}
	h.mu.Unlock()
	for {
		h.mu.Lock()
		size := 0
		for _, f := range h.frames {
			if f.seq <= after {
				continue
			}
			if len(out) == 0 && f.seq > after+1 {
				gap = true
			}
			if len(out) > 0 && size+len(f.data) > mediaMaxReply {
				break
			}
			out = append(out, f)
			size += len(f.data)
		}
		if len(out) == 0 && h.seq > after {
			gap = true // everything after it was dropped
			after = h.seq
		}
		last = after
		if len(out) > 0 {
			last = out[len(out)-1].seq
		}
		wake := h.wake
		h.mu.Unlock()
		if len(out) > 0 || gap {
			return out, last, gap
		}
		select {
		case <-wake:
		case <-timer.C:
			return nil, last, false
		case <-ctx.Done():
			return nil, last, false
		}
	}
}

// encodeMediaFrames lays frames out as the page reads them.
func encodeMediaFrames(frames []mediaFrame) []byte {
	n := 0
	for _, f := range frames {
		n += 16 + len(f.user) + len(f.data)
	}
	buf := make([]byte, 0, n)
	for _, f := range frames {
		var flags byte
		if f.key {
			flags |= 1
		}
		if f.h264 {
			flags |= 2
		}
		user := f.user
		if len(user) > math.MaxUint16 {
			user = user[:math.MaxUint16]
		}
		buf = append(buf, f.kind, flags)
		buf = binary.LittleEndian.AppendUint16(buf, uint16(len(user)))
		buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(f.cap))
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(f.data)))
		buf = append(buf, user...)
		buf = append(buf, f.data...)
	}
	return buf
}

// serveMedia answers the page's GET /cc-media/frames?after=N.
func serveMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	out, last, gap := videoOut.take(r.Context(), after, mediaWait)
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Media-Last", strconv.FormatUint(last, 10))
	if gap {
		h.Set("X-Media-Gap", "1")
	}
	body := encodeMediaFrames(out)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

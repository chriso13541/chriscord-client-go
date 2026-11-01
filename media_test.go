package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func newTestHub() *mediaHub { return &mediaHub{wake: make(chan struct{})} }

// A waiting request comes back as soon as a frame comes in, with it.
func TestMediaWaitsForFrame(t *testing.T) {
	h := newTestHub()
	h.push(mediaCamera, "old", "vp8", true, 0, []byte{9}) // before the page started: not sent
	got := make(chan []mediaFrame, 1)
	var last uint64
	go func() {
		f, l, _ := h.take(context.Background(), 0, 5*time.Second)
		last = l
		got <- f
	}()
	time.Sleep(50 * time.Millisecond)
	h.push(mediaScreen, "alice", "h264", true, 12.5, []byte{1, 2, 3})
	select {
	case f := <-got:
		if len(f) != 1 || f[0].user != "alice" || f[0].cap != 12.5 || last != 2 {
			t.Fatalf("got %+v last %d", f, last)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the waiting request didn't come back when a frame came in")
	}
	// Nothing new: an empty answer after the wait.
	start := time.Now()
	f, l, gap := h.take(context.Background(), 2, 100*time.Millisecond)
	if len(f) != 0 || l != 2 || gap || time.Since(start) < 90*time.Millisecond {
		t.Fatalf("empty wait: %v %d %v", f, l, gap)
	}
}

// The page's first request, before any frame was ever sent: it gets the
// first frames, not just the ones after them.
func TestMediaFirstFrames(t *testing.T) {
	h := newTestHub()
	got := make(chan []mediaFrame, 1)
	go func() { f, _, _ := h.take(context.Background(), 0, 5*time.Second); got <- f }()
	time.Sleep(50 * time.Millisecond)
	h.push(mediaCamera, "alice", "vp8", true, 0, []byte{1})
	if f := <-got; len(f) != 1 || f[0].user != "alice" {
		t.Fatalf("the first frame was skipped: %+v", f)
	}
}

// Falling behind: the oldest frames go, the page is told (gap), and the
// memory kept is bounded.
func TestMediaGap(t *testing.T) {
	h := newTestHub()
	for i := 0; i < mediaMaxFrames+50; i++ {
		h.push(mediaCamera, "bob", "vp8", i%30 == 0, 0, []byte{byte(i)})
	}
	if len(h.frames) != mediaMaxFrames {
		t.Fatalf("kept %d frames", len(h.frames))
	}
	f, last, gap := h.take(context.Background(), 10, time.Second)
	if !gap || len(f) != mediaMaxFrames || f[0].seq != 51 || last != uint64(mediaMaxFrames+50) {
		t.Fatalf("gap %v, %d frames from %d, last %d", gap, len(f), f[0].seq, last)
	}
	// Caught up: no gap.
	h.push(mediaCamera, "bob", "vp8", false, 0, []byte{1})
	if _, _, gap := h.take(context.Background(), last, time.Second); gap {
		t.Fatal("a gap reported with nothing dropped")
	}
	// Big frames: bounded by bytes too.
	big := newTestHub()
	for i := 0; i < 10; i++ {
		big.push(mediaScreen, "c", "h264", true, 0, make([]byte, 5<<20))
	}
	if big.bytes > mediaMaxBytes {
		t.Fatalf("kept %d bytes", big.bytes)
	}
	// One answer is at most mediaMaxReply (plus one frame).
	f, _, _ = big.take(context.Background(), 1, time.Second)
	if len(f) != 1 {
		t.Fatalf("an answer carried %d big frames", len(f))
	}
}

// The bytes, as the page reads them, through the asset handler.
func TestMediaServe(t *testing.T) {
	old := videoOut
	videoOut = newTestHub()
	defer func() { videoOut = old }()
	videoOut.push(mediaCamera, "x", "vp8", false, 0, []byte{0}) // seq 1
	go func() {
		time.Sleep(30 * time.Millisecond)
		videoOut.push(mediaScreen, "zoë", "h264", true, 3.25, []byte("frame-one"))
		videoOut.push(mediaCameraPreview, "", "h264", false, 0, []byte("p"))
	}()
	w := httptest.NewRecorder()
	serveMedia(w, httptest.NewRequest("GET", mediaPath+"?after=1", nil))
	body := w.Body.Bytes()
	if w.Code != 200 || w.Header().Get("X-Media-Gap") != "" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	if n, _ := strconv.Atoi(w.Header().Get("X-Media-Last")); n < 2 {
		t.Fatalf("last %q", w.Header().Get("X-Media-Last"))
	}
	// First record.
	if body[0] != mediaScreen || body[1] != 3 {
		t.Fatalf("kind/flags %d %d", body[0], body[1])
	}
	ulen := int(binary.LittleEndian.Uint16(body[2:]))
	capMs := math.Float64frombits(binary.LittleEndian.Uint64(body[4:]))
	size := int(binary.LittleEndian.Uint32(body[12:]))
	if string(body[16:16+ulen]) != "zoë" || capMs != 3.25 || !bytes.Equal(body[16+ulen:16+ulen+size], []byte("frame-one")) {
		t.Fatalf("record: %q %v %q", body[16:16+ulen], capMs, body[16+ulen:16+ulen+size])
	}
	if w := httptest.NewRecorder(); true {
		serveMedia(w, httptest.NewRequest("POST", mediaPath, nil))
		if w.Code != 405 {
			t.Fatal("POST answered")
		}
	}
}

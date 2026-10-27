package main

import "testing"

// A stereo voice is kept as middle and difference, and whatever smoothing
// takes off the front of one comes off the other.
func TestStereoVoiceQueue(t *testing.T) {
	r := &voiceRemoteSource{}
	r.add([]int16{100, 100}, false) // mono to start with
	r.side = make([]int16, len(r.buf))
	r.add([]int16{1000, -1000, 400, 200}, true) // L/R pairs
	if len(r.buf) != 4 || len(r.side) != 4 {
		t.Fatalf("buf %v side %v", r.buf, r.side)
	}
	if r.buf[2] != 0 || r.side[2] != 1000 || r.buf[3] != 300 || r.side[3] != 100 {
		t.Fatalf("buf %v side %v", r.buf, r.side)
	}
	before := len(r.buf)
	r.buf = r.buf[3:] // smoothing skips three
	r.keepSide(before)
	if len(r.side) != 1 || r.side[0] != 100 {
		t.Fatalf("side not kept in step: %v", r.side)
	}
	// Left and right come back out: 300+100, 300-100.
	if l, rr := r.buf[0]+r.side[0], r.buf[0]-r.side[0]; l != 400 || rr != 200 {
		t.Fatalf("got %d/%d", l, rr)
	}
	// Mono packets on a stereo voice: no difference.
	r.add([]int16{50}, false)
	if len(r.side) != len(r.buf) || r.side[len(r.side)-1] != 0 {
		t.Fatalf("buf %v side %v", r.buf, r.side)
	}
}

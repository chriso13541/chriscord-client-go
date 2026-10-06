package main

import (
	"math"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
)

// Keeping a screen share's sound in step with its picture.
//
// The picture takes longer to get to a viewer's screen than the sound does
// (capture → graphics-card encode → decode in the page, against a plain
// Opus round trip), so played as soon as it arrives, the sound runs ahead.
// To line them up:
//
//   - The sharer stamps both with the moment they were captured, on one
//     clock: the RTP timestamps of the screen (90 kHz) and its sound
//     (48 kHz) both count from the start of the share. (Ordinary tracks
//     start at a random timestamp; these are packetised here instead so
//     the timestamps mean something.) The server passes timestamps through
//     untouched.
//   - A viewer's page reports how long after capture each frame of the
//     picture actually appears (SetStreamVideoLag).
//   - The viewer's mixer holds the sound back until it's that old too
//     (see the stereo branch of startPlayback's mixer).
//
// Everything is measured against each side's own wall clock, so the two
// computers' clocks never need to agree: the unknown difference between
// them is the same for the picture and the sound, and cancels out.

const (
	// From capture to the moment a frame reaches this app: grab, scale,
	// encode, hand over. Not measurable from here, so a typical figure.
	screenVideoPipelineMs = 45.0
	rtpMTU                = 1200
)

// rtpOut packetises one outgoing stream with timestamps chosen by the
// caller (pion's TrackLocalStaticSample picks its own).
type rtpOut struct {
	mu        sync.Mutex
	trk       *webrtc.TrackLocalStaticRTP
	payloader rtp.Payloader
	seq       uint16
	video     bool
}

func newRTPOut(trk *webrtc.TrackLocalStaticRTP, video bool) *rtpOut {
	o := &rtpOut{trk: trk, video: video, seq: uint16(time.Now().UnixNano())}
	if video {
		o.payloader = &codecs.H264Payloader{}
	} else {
		o.payloader = &codecs.OpusPayloader{}
	}
	return o
}

// write sends one frame (an Annex B access unit, or one Opus packet).
func (o *rtpOut) write(frame []byte, ts uint32) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	payloads := o.payloader.Payload(rtpMTU, frame)
	for i, p := range payloads {
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				Marker:         o.video && i == len(payloads)-1,
				SequenceNumber: o.seq,
				Timestamp:      ts,
			},
			Payload: p,
		}
		o.seq++
		if err := o.trk.WriteRTP(pkt); err != nil {
			return err
		}
	}
	return nil
}

// shareClock is the sharer's "ms since the share started" clock, for both
// the picture and the sound.
type shareClock struct {
	epoch time.Time

	mu        sync.Mutex
	vAnchored bool
	vFLVBase  uint32  // FFmpeg's timestamp (ms) of the anchor frame
	vCapBase  float64 // its capture time on this clock
}

func newShareClock() *shareClock { return &shareClock{epoch: time.Now()} }

func (c *shareClock) nowMs() float64 { return float64(time.Since(c.epoch)) / float64(time.Millisecond) }

// videoCapture: when a frame FFmpeg stamped flvMs was captured. FFmpeg's
// own timestamps give the exact spacing between frames; they're anchored
// to this clock by the first frame's arrival (less the pipeline), and
// re-anchored if they ever wander off (a long still screen, a hiccup).
func (c *shareClock) videoCapture(flvMs uint32) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	est := c.nowMs() - screenVideoPipelineMs
	if c.vAnchored {
		cap := c.vCapBase + float64(int64(flvMs)-int64(c.vFLVBase))
		if d := est - cap; d > -50 && d < 400 {
			return cap
		}
	}
	c.vAnchored, c.vFLVBase, c.vCapBase = true, flvMs, est
	return est
}

func rtpTS(ms float64, clockRate float64) uint32 {
	return uint32(int64(math.Round(ms * clockRate / 1000)))
}

// tsUnwrap turns a stream of wrapping 32-bit RTP timestamps into
// milliseconds that keep counting up.
type tsUnwrap struct {
	have   bool
	last   uint32
	cycles int64
}

func (u *tsUnwrap) ms(ts uint32, clockRate float64) float64 {
	if u.have {
		if ts < u.last && u.last-ts > 1<<31 {
			u.cycles++
		} else if ts > u.last && ts-u.last > 1<<31 && u.cycles > 0 {
			u.cycles-- // a late packet from before the wrap
		}
	}
	u.have, u.last = true, ts
	return float64(u.cycles<<32+int64(ts)) * 1000 / clockRate
}

// nowWallMs: this computer's wall clock in ms (the page uses Date.now(),
// the same clock).
func nowWallMs() float64 { return float64(time.Now().UnixNano()) / 1e6 }

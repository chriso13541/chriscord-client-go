package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/samplebuilder"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// Webcam video in calls.
//
// The camera itself is captured and encoded in the page (getUserMedia +
// WebCodecs — the webview already knows every camera and what it
// supports), and each encoded frame is handed to PushVideoFrame, which
// writes it to a video track on the call's existing WebRTC connection.
//
// The codec is H.264 wherever the page can encode it — Windows hands that
// to the graphics card (Quick Sync / NVENC / AMD) through Media
// Foundation, so a camera costs next to no CPU — falling back to software
// H.264, and to VP8 only where H.264 isn't available at all. The page
// says which at startup (SetVideoCodec); each person's choice travels
// with their stream, so senders don't have to agree.
// Everyone else's video arrives here as RTP, is put back together into
// whole frames (samplebuilder), and goes to the page as "video:frame"
// events for it to decode and draw. So the server only ever forwards
// packets, and nothing here decodes or encodes video.
//
// The offer carries one send-only video section after all the audio ones
// (see startVoiceSession); packets only flow while the camera is on.
// Whether it's on is signalled separately (SetVideoEnabled), which is
// what everyone's app goes by.

// videoFrameEvent is one whole encoded frame from someone else's camera.
type videoFrameEvent struct {
	User  string `json:"user"`
	Kind  string `json:"kind"` // "camera" or "screen"
	Codec string `json:"codec"` // "h264" (Annex B) or "vp8"
	Key   bool   `json:"key"`
	Data  string `json:"data"` // base64 frame
}

// The codec this app sends its camera in, as chosen by the page.
var (
	videoCodecMu sync.Mutex
	videoCodec   = "h264"
)

// SetVideoCodec is called by the page at startup with what it can encode:
// "h264" or "vp8". Takes effect for the next call joined.
func (a *App) SetVideoCodec(codec string) {
	if codec != "h264" && codec != "vp8" {
		return
	}
	videoCodecMu.Lock()
	videoCodec = codec
	videoCodecMu.Unlock()
}

func videoCapability() webrtc.RTPCodecCapability {
	videoCodecMu.Lock()
	c := videoCodec
	videoCodecMu.Unlock()
	// Native capture (capture.go) always sends H.264, whatever the page
	// could encode itself: never give the call a track it can't fill.
	if c == "vp8" && nativeCaptureUsable() {
		c = "h264"
	}
	if c == "vp8" {
		return webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}
	}
	return h264Capability()
}

// h264Capability: Constrained Baseline (no B-frames — right for live
// video), the one every WebRTC stack and hardware encoder agrees on.
func h264Capability() webrtc.RTPCodecCapability {
	return webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeH264,
		ClockRate:   90000,
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
	}
}

// addVideoSender adds this session's outgoing camera track — called
// after the audio placeholders so it's the last section of the offer.
func (s *VoiceSession) addVideoSender() {
	track, err := webrtc.NewTrackLocalStaticSample(videoCapability(), "video", "chriscord-video")
	if err != nil {
		log.Printf("video: local track: %v", err)
		return
	}
	tr, err := s.pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
	if err != nil {
		log.Printf("video: add transceiver: %v", err)
		return
	}
	s.videoTrack = track
	// Keyframe requests from the server (a viewer starting or recovering)
	// arrive as RTCP on this sender; the page's encoder makes one.
	go readKeyframeRequests(tr.Sender(), func() { wailsruntime.EventsEmit(s.ctx, "video:keyframe") })

	// The screen share's own send-only section, right after the camera's
	// (always H.264: only native capture shares screens). The server tells
	// it apart from the camera by its track id, "screen".
	screen, err := webrtc.NewTrackLocalStaticSample(h264Capability(), "screen", "chriscord-screen")
	if err != nil {
		log.Printf("screen: local track: %v", err)
		return
	}
	str, err := s.pc.AddTransceiverFromTrack(screen, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
	if err != nil {
		log.Printf("screen: add transceiver: %v", err)
		return
	}
	s.screenTrack = screen
	go readKeyframeRequests(str.Sender(), func() { s.app.screenKeyframeRequested(screen) })

	// And its sound: stereo Opus, its own send-only audio section after the
	// video ones (the server maps only the earlier audio sections by
	// position, so this one at the end doesn't disturb that). Named
	// "screenaudio"; it only carries packets while a share has sound.
	sound, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
		SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1",
	}, "screenaudio", "chriscord-screen")
	if err != nil {
		log.Printf("screen: sound track: %v", err)
		return
	}
	if _, err := s.pc.AddTransceiverFromTrack(sound, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly}); err != nil {
		log.Printf("screen: add sound transceiver: %v", err)
		return
	}
	s.screenAudioTrack = sound
}

// readKeyframeRequests calls onKey (at most every 300 ms) when the server
// passes on a viewer's keyframe request for this sender.
func readKeyframeRequests(sender *webrtc.RTPSender, onKey func()) {
	var last time.Time
	for {
		pkts, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			switch p.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				if time.Since(last) > 300*time.Millisecond {
					last = time.Now()
					onKey()
				}
			}
		}
	}
}

// PushVideoFrame sends one encoded frame from the page's camera encoder
// (H.264 in Annex B form, or VP8 — whichever SetVideoCodec chose) into the
// call. durationMs is how long the frame is shown for.
func (a *App) PushVideoFrame(data string, durationMs int) error {
	a.voiceMu.Lock()
	session := a.voice
	a.voiceMu.Unlock()
	if session == nil || session.videoTrack == nil {
		return fmt.Errorf("not in a call")
	}
	frame, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return err
	}
	if durationMs <= 0 {
		durationMs = 33
	}
	return session.videoTrack.WriteSample(media.Sample{Data: frame, Duration: time.Duration(durationMs) * time.Millisecond})
}

// VideoSendCodec says which codec the current call's camera track is in
// ("h264" / "vp8"; "" when not in a call) — the page encodes to match.
func (a *App) VideoSendCodec() string {
	a.voiceMu.Lock()
	session := a.voice
	a.voiceMu.Unlock()
	if session == nil || session.videoTrack == nil {
		return ""
	}
	if strings.EqualFold(session.videoTrack.Codec().MimeType, webrtc.MimeTypeH264) {
		return "h264"
	}
	return "vp8"
}

// SetVideoEnabled tells everyone whether this camera is on.
func (a *App) SetVideoEnabled(on bool) error {
	a.voiceMu.Lock()
	session := a.voice
	a.voiceMu.Unlock()
	if session == nil {
		return fmt.Errorf("not in a call")
	}
	return a.sendVoiceJSON(map[string]interface{}{"type": "voice_video", "board_id": session.boardID, "video": on})
}

// RequestVideoKeyframe asks someone's camera (via the server) for a fresh
// keyframe — for when this app's picture of them needs to (re)start.
func (a *App) RequestVideoKeyframe(user string) error {
	a.voiceMu.Lock()
	session := a.voice
	a.voiceMu.Unlock()
	if session == nil {
		return fmt.Errorf("not in a call")
	}
	return a.sendVoiceJSON(map[string]interface{}{"type": "voice_keyframe", "board_id": session.boardID, "target": user})
}

// RequestScreenKeyframe: the same for someone's screen share.
func (a *App) RequestScreenKeyframe(user string) error {
	a.voiceMu.Lock()
	session := a.voice
	a.voiceMu.Unlock()
	if session == nil {
		return fmt.Errorf("not in a call")
	}
	return a.sendVoiceJSON(map[string]interface{}{"type": "voice_keyframe", "board_id": session.boardID, "target": user, "kind": "screen"})
}

// SetScreenShareEnabled tells everyone whether this app is sharing its screen.
func (a *App) SetScreenShareEnabled(on bool) error {
	a.voiceMu.Lock()
	session := a.voice
	a.voiceMu.Unlock()
	if session == nil {
		return fmt.Errorf("not in a call")
	}
	return a.sendVoiceJSON(map[string]interface{}{"type": "voice_screen", "board_id": session.boardID, "screen": on})
}

// WatchScreen starts (or stops) receiving someone's screen share — screens
// are only sent to people who click to watch them.
func (a *App) WatchScreen(user string, watch bool) error {
	a.voiceMu.Lock()
	session := a.voice
	a.voiceMu.Unlock()
	if session == nil {
		return fmt.Errorf("not in a call")
	}
	return a.sendVoiceJSON(map[string]interface{}{"type": "voice_watch", "board_id": session.boardID, "target": user, "watch": watch})
}

func (a *App) sendVoiceJSON(v interface{}) error {
	msg, err := json.Marshal(v)
	if err != nil {
		return err
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if a.ws == nil {
		return fmt.Errorf("not connected")
	}
	return a.ws.WriteMessage(websocket.TextMessage, msg)
}

// handleRemoteVideo reassembles someone's camera stream into whole frames
// (H.264 or VP8, whichever they send) and passes each to the page. The
// server names these tracks "video-<username>".
func (s *VoiceSession) handleRemoteVideo(track *webrtc.TrackRemote) {
	user := strings.TrimPrefix(track.ID(), "video-")
	kind := "camera"
	if strings.HasPrefix(track.ID(), "screen-") { // someone's screen share
		user, kind = strings.TrimPrefix(track.ID(), "screen-"), "screen"
	}
	codec := "vp8"
	var depacketizer rtp.Depacketizer = &codecs.VP8Packet{}
	isKey := func(f []byte) bool { return f[0]&0x01 == 0 } // VP8 frame tag: bit 0 clear = keyframe
	if strings.EqualFold(track.Codec().MimeType, webrtc.MimeTypeH264) {
		codec = "h264"
		depacketizer = &codecs.H264Packet{} // gives Annex B, which the page's decoder takes
		isKey = h264HasKeyframe
	}
	log.Printf("video: receiving %s's %s (%s)", user, kind, codec)
	// The picture can only start from a keyframe.
	if kind == "screen" {
		go s.app.RequestScreenKeyframe(user)
	} else {
		go s.app.RequestVideoKeyframe(user)
	}
	builder := samplebuilder.New(512, depacketizer, 90000)
	var once sync.Once
	for {
		select {
		case <-s.stopped:
			return
		default:
		}
		packet, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		builder.Push(packet)
		for {
			sample := builder.Pop()
			if sample == nil {
				break
			}
			if len(sample.Data) == 0 {
				continue
			}
			once.Do(func() { log.Printf("video: first %s frame from %s", kind, user) })
			wailsruntime.EventsEmit(s.ctx, "video:frame", videoFrameEvent{
				User:  user,
				Kind:  kind,
				Codec: codec,
				Key:   isKey(sample.Data),
				Data:  base64.StdEncoding.EncodeToString(sample.Data),
			})
		}
	}
}

// h264HasKeyframe reports whether an Annex B access unit holds an IDR
// picture (or the SPS that comes with one) — something a decoder can
// start from.
func h264HasKeyframe(au []byte) bool {
	for i := 0; i+3 < len(au); i++ {
		// A start code is 00 00 01 (possibly after another 00).
		if au[i] != 0 || au[i+1] != 0 || au[i+2] != 1 {
			continue
		}
		switch au[i+3] & 0x1f {
		case 5, 7: // IDR slice, sequence parameter set
			return true
		}
		i += 2
	}
	return false
}

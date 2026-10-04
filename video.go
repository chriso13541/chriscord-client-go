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
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/samplebuilder"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// Webcam video in calls.
//
// The camera itself is captured and VP8-encoded in the page (getUserMedia
// + WebCodecs — the webview already knows every camera and what it
// supports), and each encoded frame is handed to PushVideoFrame, which
// writes it to a video track on the call's existing WebRTC connection.
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
	User string `json:"user"`
	Key  bool   `json:"key"`
	Data string `json:"data"` // base64 VP8 frame
}

// addVideoSender adds this session's outgoing camera track — called
// after the audio placeholders so it's the last section of the offer.
func (s *VoiceSession) addVideoSender() {
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
		"video", "chriscord-video",
	)
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
	go func(sender *webrtc.RTPSender) {
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
						wailsruntime.EventsEmit(s.ctx, "video:keyframe")
					}
				}
			}
		}
	}(tr.Sender())
}

// PushVideoFrame sends one encoded VP8 frame from the page's camera
// encoder into the call. durationMs is how long the frame is shown for.
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

// handleRemoteVideo reassembles someone's camera stream into whole VP8
// frames and passes each to the page. The server names these tracks
// "video-<username>".
func (s *VoiceSession) handleRemoteVideo(track *webrtc.TrackRemote) {
	user := strings.TrimPrefix(track.ID(), "video-")
	log.Printf("video: receiving %s's camera", user)
	// The picture can only start from a keyframe.
	go s.app.RequestVideoKeyframe(user)
	builder := samplebuilder.New(256, &codecs.VP8Packet{}, 90000)
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
			once.Do(func() { log.Printf("video: first frame from %s", user) })
			wailsruntime.EventsEmit(s.ctx, "video:frame", videoFrameEvent{
				User: user,
				Key:  sample.Data[0]&0x01 == 0, // VP8 frame tag: bit 0 clear = keyframe
				Data: base64.StdEncoding.EncodeToString(sample.Data),
			})
		}
	}
}

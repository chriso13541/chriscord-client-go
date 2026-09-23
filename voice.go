package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gordonklaus/portaudio"
	"github.com/hraban/opus"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	voiceSampleRate = 48000
	voiceChannels   = 1
	voiceFrameMs    = 20
	voiceFrameSize  = voiceSampleRate * voiceFrameMs / 1000 // 960 samples per 20ms frame
)

// voiceRemoteSource is the decode state for one other participant's
// incoming audio track — a small buffer of decoded PCM the mixer drains
// from, filled by that track's own read/decode goroutine.
type voiceRemoteSource struct {
	decoder *opus.Decoder
	mu      sync.Mutex
	buf     []int16
}

// VoiceSession owns everything for one active voice-channel connection. A
// fresh one is created on every join AND on every roster-change refresh
// (the previous one is closed first) — see the matching design note in
// the server's voice.rs for why that's deliberate: it's what makes the
// "simple" version able to evolve into seamless renegotiation later
// without a rewrite, since the actual capture/encode/decode/mix pipeline
// here doesn't care how many times a negotiation happens over its life.
type VoiceSession struct {
	ctx         context.Context
	app         *App // back-reference so the capture loop can signal speaking state to the server
	boardID     string
	micName     string
	speakerName string
	pc          *webrtc.PeerConnection
	localTrack  *webrtc.TrackLocalStaticSample
	encoder     *opus.Encoder

	captureStream   *portaudio.Stream
	captureBuf      []int16
	wasSpeaking     bool // only touched from within the capture goroutine — no mutex needed
	quietFrameCount int
	// Points at the App's own voiceMuted flag, not an owned value — a
	// roster-change refresh tears down and rebuilds this whole session,
	// and muting shouldn't silently reset just because someone else
	// joined or left in the background. See App.voiceMuted.
	muted *atomic.Bool
	// Same reasoning and pattern as muted above — points at App.voiceDeafened.
	deafened *atomic.Bool

	playStream *portaudio.Stream
	playBuf    []int16

	remotesMu sync.Mutex
	remotes   map[string]*voiceRemoteSource // keyed by remote track ID

	closeOnce sync.Once
	stopped   chan struct{}
}

// findDeviceByName does a case-insensitive substring match against
// PortAudio's own device list — needed because the browser-side Web Audio
// API (used to populate Settings > Voice) and PortAudio have entirely
// separate device identifiers; matching by the human-readable name is the
// practical bridge between them. Falls back to the default device for
// that direction if no match is found or name is empty.
func findDeviceByName(name string, wantInput bool) (*portaudio.DeviceInfo, error) {
	if name != "" {
		devices, err := portaudio.Devices()
		if err == nil {
			lower := strings.ToLower(name)
			for _, d := range devices {
				if wantInput && d.MaxInputChannels < 1 {
					continue
				}
				if !wantInput && d.MaxOutputChannels < 1 {
					continue
				}
				if strings.Contains(strings.ToLower(d.Name), lower) || strings.Contains(lower, strings.ToLower(d.Name)) {
					return d, nil
				}
			}
		}
	}
	if wantInput {
		return portaudio.DefaultInputDevice()
	}
	return portaudio.DefaultOutputDevice()
}

// JoinVoiceChannel connects presence to a voice board AND establishes the
// real WebRTC audio connection: capturing from micName and playing back on
// speakerName, both the device labels shown in Settings > Voice (not the
// browser's deviceId — see findDeviceByName for why). Returns once the
// offer has been sent, not once the call is fully connected; the frontend
// should treat "connected" as a separate, later state signaled over the
// existing voice:state mechanism plus connection-state events emitted
// below.
func (a *App) JoinVoiceChannel(boardID, micName, speakerName string, knownOthers []string) error {
	a.writeMu.Lock()
	conn := a.ws
	a.writeMu.Unlock()
	if conn == nil {
		return fmt.Errorf("not connected")
	}

	log.Printf("voice: JoinVoiceChannel called for board %s (explicit user join, known others: %v)", boardID, knownOthers)
	if err := a.startVoiceSession(boardID, micName, speakerName, knownOthers, "initial join"); err != nil {
		return fmt.Errorf("failed to start voice session: %w", err)
	}

	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	msg, _ := json.Marshal(map[string]string{"type": "join_voice", "board_id": boardID})
	return a.ws.WriteMessage(websocket.TextMessage, msg)
}

func (a *App) LeaveVoiceChannel() error {
	a.stopVoiceSession()
	a.voiceMuted.Store(false)
	a.voiceDeafened.Store(false)

	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if a.ws == nil {
		return fmt.Errorf("not connected")
	}
	msg, _ := json.Marshal(map[string]string{"type": "leave_voice"})
	return a.ws.WriteMessage(websocket.TextMessage, msg)
}

// sendSpeaking tells the server this participant's local speaking state
// changed, for it to rebroadcast to everyone else — so a speaking
// indicator can show up next to their avatar server-wide, not just
// locally to themselves. Best-effort: silently does nothing if not
// currently connected, same as any other send during a voice session
// that might be in the middle of a reconnect.
func (a *App) sendSpeaking(boardID string, speaking bool) {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if a.ws == nil {
		return
	}
	msg, _ := json.Marshal(map[string]interface{}{"type": "speaking", "board_id": boardID, "speaking": speaking})
	_ = a.ws.WriteMessage(websocket.TextMessage, msg)
}

// sendVoiceMuteState tells the server this participant's mute/deafen
// state changed, for it to rebroadcast to everyone else — so a mute or
// deafen icon can show up next to their name. Best-effort, same as
// sendSpeaking above.
func (a *App) sendVoiceMuteState(boardID string, muted, deafened bool) {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if a.ws == nil {
		return
	}
	msg, _ := json.Marshal(map[string]interface{}{
		"type": "voice_mute_state", "board_id": boardID, "muted": muted, "deafened": deafened,
	})
	_ = a.ws.WriteMessage(websocket.TextMessage, msg)
}

// ToggleMute flips whether this participant's microphone audio is being
// transmitted during the current voice session, and returns the new
// muted state. Purely client-side — muting simply stops sending anything
// at all, so the server and other participants never need to know or be
// told; there's nothing to forward when nothing gets sent. No-ops
// (returns false) if not currently in a voice call. The flag lives on
// the App, not the session, so it survives a roster-change refresh
// tearing down and rebuilding the underlying connection.
func (a *App) ToggleMute() bool {
	a.voiceMu.Lock()
	session := a.voice
	a.voiceMu.Unlock()
	if session == nil {
		return false
	}
	newState := !a.voiceMuted.Load()
	a.voiceMuted.Store(newState)
	a.sendVoiceMuteState(session.boardID, newState, a.voiceDeafened.Load())
	return newState
}

// IsMuted reports the current mute state, for the UI to sync against
// (e.g. on startup or after a reconnect) without toggling it.
func (a *App) IsMuted() bool {
	return a.voiceMuted.Load()
}

// ToggleDeafen flips whether incoming audio from everyone else is being
// played, and returns the new deafened state. Deafening also mutes, and
// undeafening also unmutes — a clean, symmetric toggle rather than
// leaving mute as a separate thing to manage after undeafening. Purely
// client-side, same as mute: deafened audio is still received and
// decoded as normal, just not written to the output device, so nothing
// needs telling the server or other participants. No-ops (returns false)
// if not currently in a voice call.
func (a *App) ToggleDeafen() bool {
	a.voiceMu.Lock()
	session := a.voice
	a.voiceMu.Unlock()
	if session == nil {
		return false
	}
	newState := !a.voiceDeafened.Load()
	a.voiceDeafened.Store(newState)
	a.voiceMuted.Store(newState)
	a.sendVoiceMuteState(session.boardID, newState, newState)
	return newState
}

// IsDeafened reports the current deafen state, for the UI to sync
// against without toggling it.
func (a *App) IsDeafened() bool {
	return a.voiceDeafened.Load()
}

// startVoiceSession tears down any existing session (this IS the "refresh"
// path — a roster-change re-join is handled identically to a first-time
// join, see startup.go/wsReader's voice:state handling) and builds a fresh
// one: opens mic capture, creates the PeerConnection, wires local and
// remote track handling, and sends the initial offer. knownOthers is the
// currently-known roster of the channel (excluding self) — the offer
// declares one receive-only placeholder transceiver per entry, in this
// exact order, so the server has somewhere valid to attach each
// participant's audio without needing to add media sections the offer
// never asked for. Always declares at least one placeholder even with no
// known others, to avoid a known pion/webrtc-rs bug where a single-media-
// section offer's on_track callback never fires at all.
func (a *App) startVoiceSession(boardID, micName, speakerName string, knownOthers []string, reason string) error {
	log.Printf("voice: startVoiceSession(board=%s, reason=%q, knownOthers=%v) — tearing down any existing session and sending a fresh offer", boardID, reason, knownOthers)
	a.stopVoiceSession()

	encoder, err := opus.NewEncoder(voiceSampleRate, voiceChannels, opus.AppVoIP)
	if err != nil {
		return fmt.Errorf("opus encoder: %w", err)
	}

	localTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: voiceSampleRate, Channels: voiceChannels},
		"audio", "chriscord-mic",
	)
	if err != nil {
		return fmt.Errorf("local track: %w", err)
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		return fmt.Errorf("peer connection: %w", err)
	}
	if _, err := pc.AddTrack(localTrack); err != nil {
		pc.Close()
		return fmt.Errorf("add local track: %w", err)
	}

	// One receive-only placeholder per known other, always at least one —
	// see the function comment above for why even the solo case needs
	// this. expected_others (sent with the offer below) is what tells the
	// server which placeholder, by position, belongs to which username.
	placeholderCount := len(knownOthers)
	if placeholderCount < 1 {
		placeholderCount = 1
	}
	for i := 0; i < placeholderCount; i++ {
		if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			log.Printf("voice: failed to add placeholder transceiver %d: %v", i, err)
		}
	}

	session := &VoiceSession{
		ctx:         a.ctx,
		app:         a,
		boardID:     boardID,
		micName:     micName,
		speakerName: speakerName,
		pc:          pc,
		localTrack:  localTrack,
		encoder:     encoder,
		remotes:     make(map[string]*voiceRemoteSource),
		stopped:     make(chan struct{}),
		muted:       &a.voiceMuted,
		deafened:    &a.voiceDeafened,
	}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		a.writeMu.Lock()
		defer a.writeMu.Unlock()
		if a.ws == nil {
			return
		}
		msg, _ := json.Marshal(map[string]interface{}{
			"type": "voice_ice", "board_id": boardID,
			"candidate": init.Candidate, "sdp_mid": init.SDPMid, "sdp_mline_index": init.SDPMLineIndex,
		})
		a.ws.WriteMessage(websocket.TextMessage, msg)
	})

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		wailsruntime.EventsEmit(a.ctx, "voice:connectionState", s.String())
	})

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		session.handleRemoteTrack(track)
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		pc.Close()
		return fmt.Errorf("create offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		pc.Close()
		return fmt.Errorf("set local description: %w", err)
	}

	a.voiceMu.Lock()
	a.voice = session
	a.voiceMu.Unlock()

	if err := session.startCapture(micName); err != nil {
		wailsruntime.EventsEmit(a.ctx, "voice:error", fmt.Sprintf("microphone: %v", err))
	}
	if err := session.startPlayback(speakerName); err != nil {
		wailsruntime.EventsEmit(a.ctx, "voice:error", fmt.Sprintf("speaker: %v", err))
	}

	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if a.ws == nil {
		return fmt.Errorf("not connected")
	}
	msg, _ := json.Marshal(map[string]interface{}{
		"type": "voice_offer", "board_id": boardID, "sdp": offer.SDP, "expected_others": knownOthers,
	})
	return a.ws.WriteMessage(websocket.TextMessage, msg)
}

func (a *App) stopVoiceSession() {
	a.voiceMu.Lock()
	session := a.voice
	a.voice = nil
	a.voiceMu.Unlock()
	if session != nil {
		session.close()
	}
}

// handleVoiceAnswer applies the SDP answer the server sent back for the
// current session's offer. Called from wsReader on a "voice_answer"
// message.
func (a *App) handleVoiceAnswer(sdp string) {
	a.voiceMu.Lock()
	session := a.voice
	a.voiceMu.Unlock()
	if session == nil {
		return
	}
	answer := webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}
	if err := session.pc.SetRemoteDescription(answer); err != nil {
		wailsruntime.EventsEmit(a.ctx, "voice:error", fmt.Sprintf("failed to apply answer: %v", err))
	}
}

// handleVoiceICE applies an ICE candidate the server sent for the current
// session. Called from wsReader on a "voice_ice" message.
func (a *App) handleVoiceICE(candidate, sdpMid string, sdpMLineIndex *uint16) {
	a.voiceMu.Lock()
	session := a.voice
	a.voiceMu.Unlock()
	if session == nil {
		return
	}
	init := webrtc.ICECandidateInit{Candidate: candidate}
	if sdpMid != "" {
		init.SDPMid = &sdpMid
	}
	if sdpMLineIndex != nil {
		init.SDPMLineIndex = sdpMLineIndex
	}
	_ = session.pc.AddICECandidate(init)
}

// handleVoiceRenegotiate applies a server-initiated renegotiation to the
// CURRENT, already-established PeerConnection — this is the seamless
// mid-call update mechanism: when someone else joins the channel after
// this session already exists, the server adds a transceiver to this
// exact connection and sends a fresh offer for it, rather than this
// client tearing anything down and re-joining from scratch. Nothing about
// the existing capture/playback streams, mute/deafen state, or already-
// flowing audio to/from anyone else is touched — only the underlying
// PeerConnection negotiates a new media section and answers it.
func (a *App) handleVoiceRenegotiate(sdp string) {
	a.voiceMu.Lock()
	session := a.voice
	a.voiceMu.Unlock()
	if session == nil {
		return
	}
	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}
	if err := session.pc.SetRemoteDescription(offer); err != nil {
		log.Printf("voice: renegotiate SetRemoteDescription failed: %v", err)
		return
	}
	answer, err := session.pc.CreateAnswer(nil)
	if err != nil {
		log.Printf("voice: renegotiate CreateAnswer failed: %v", err)
		return
	}
	if err := session.pc.SetLocalDescription(answer); err != nil {
		log.Printf("voice: renegotiate SetLocalDescription failed: %v", err)
		return
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if a.ws == nil {
		return
	}
	msg, _ := json.Marshal(map[string]string{
		"type": "voice_renegotiate_answer", "board_id": session.boardID, "sdp": answer.SDP,
	})
	_ = a.ws.WriteMessage(websocket.TextMessage, msg)
}

func (s *VoiceSession) startCapture(micName string) error {
	device, err := findDeviceByName(micName, true)
	if err != nil {
		return fmt.Errorf("no microphone available: %w", err)
	}

	started := make(chan error, 1)
	go func() {
		// PortAudio's Windows backend (WASAPI) appears to require a
		// stream's entire lifecycle — open, start, read, stop, close —
		// to happen on the same OS thread; Go's scheduler is otherwise
		// free to migrate a goroutine between OS threads between calls,
		// which caused a real, reproducible access violation when open/
		// start ran on one thread and the read loop ran on another.
		// Locking this goroutine to one thread for the whole duration,
		// including the final Stop/Close, avoids that entirely.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		s.captureBuf = make([]int16, voiceFrameSize)
		params := portaudio.StreamParameters{
			Input: portaudio.StreamDeviceParameters{
				Device:   device,
				Channels: voiceChannels,
				Latency:  device.DefaultLowInputLatency,
			},
			SampleRate:      voiceSampleRate,
			FramesPerBuffer: voiceFrameSize,
		}
		stream, err := portaudio.OpenStream(params, s.captureBuf)
		if err != nil {
			started <- fmt.Errorf("open capture stream: %w", err)
			return
		}
		if err := stream.Start(); err != nil {
			stream.Close()
			started <- fmt.Errorf("start capture stream: %w", err)
			return
		}
		s.captureStream = stream
		started <- nil

		encoded := make([]byte, 4000)
		for {
			select {
			case <-s.stopped:
				stream.Stop()
				stream.Close()
				if s.wasSpeaking {
					wailsruntime.EventsEmit(s.ctx, "voice:speaking", false)
					s.app.sendSpeaking(s.boardID, false)
				}
				return
			default:
			}
			if err := stream.Read(); err != nil {
				return
			}
			if s.muted.Load() {
				// Still draining the stream above to keep it healthy, but
				// nothing gets encoded or sent while muted — this is a
				// real "not transmitting" mute, not just silence sent
				// over the wire. Also clear the local speaking indicator
				// if it was on, since it would be misleading to show
				// "speaking" while muted.
				if s.wasSpeaking {
					s.wasSpeaking = false
					s.quietFrameCount = 0
					wailsruntime.EventsEmit(s.ctx, "voice:speaking", false)
					s.app.sendSpeaking(s.boardID, false)
				}
				continue
			}
			s.updateSpeakingState()
			n, err := s.encoder.Encode(s.captureBuf, encoded)
			if err != nil {
				continue
			}
			sample := media.Sample{Data: append([]byte(nil), encoded[:n]...), Duration: voiceFrameMs * 1e6}
			_ = s.localTrack.WriteSample(sample)
		}
	}()
	return <-started
}

// Rough threshold for "this frame contains speech" against int16 PCM
// (range -32768..32767) — a reasonable starting point, not precisely
// tuned against real hardware; may need adjusting once tested against an
// actual microphone and room. speakingHangoverFrames keeps the indicator
// on through brief pauses between words rather than flickering on every
// syllable break — quick to detect the start of speech, slower to detect
// the end, which is how real voice-activity detectors behave.
const (
	speakingRMSThreshold   = 600
	speakingHangoverFrames = 15 // ~300ms at the 20ms frame size used here
)

func rmsLevel(samples []int16) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, v := range samples {
		f := float64(v)
		sum += f * f
	}
	return math.Sqrt(sum / float64(len(samples)))
}

// updateSpeakingState runs once per captured frame and emits voice:speaking
// only when the speaking/not-speaking state actually changes, not on every
// frame.
func (s *VoiceSession) updateSpeakingState() {
	if rmsLevel(s.captureBuf) > speakingRMSThreshold {
		s.quietFrameCount = 0
		if !s.wasSpeaking {
			s.wasSpeaking = true
			wailsruntime.EventsEmit(s.ctx, "voice:speaking", true)
			s.app.sendSpeaking(s.boardID, true)
		}
		return
	}
	s.quietFrameCount++
	if s.wasSpeaking && s.quietFrameCount > speakingHangoverFrames {
		s.wasSpeaking = false
		wailsruntime.EventsEmit(s.ctx, "voice:speaking", false)
		s.app.sendSpeaking(s.boardID, false)
	}
}

func (s *VoiceSession) startPlayback(speakerName string) error {
	device, err := findDeviceByName(speakerName, false)
	if err != nil {
		return fmt.Errorf("no speaker available: %w", err)
	}

	started := make(chan error, 1)
	go func() {
		// Same reasoning as startCapture above — the whole stream
		// lifecycle stays on one locked OS thread.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		s.playBuf = make([]int16, voiceFrameSize)
		params := portaudio.StreamParameters{
			Output: portaudio.StreamDeviceParameters{
				Device:   device,
				Channels: voiceChannels,
				Latency:  device.DefaultLowOutputLatency,
			},
			SampleRate:      voiceSampleRate,
			FramesPerBuffer: voiceFrameSize,
		}
		stream, err := portaudio.OpenStream(params, s.playBuf)
		if err != nil {
			started <- fmt.Errorf("open playback stream: %w", err)
			return
		}
		if err := stream.Start(); err != nil {
			stream.Close()
			started <- fmt.Errorf("start playback stream: %w", err)
			return
		}
		s.playStream = stream
		started <- nil

		for {
			select {
			case <-s.stopped:
				stream.Stop()
				stream.Close()
				return
			default:
			}
			for i := range s.playBuf {
				s.playBuf[i] = 0
			}
			deafened := s.deafened.Load()
			s.remotesMu.Lock()
			for _, r := range s.remotes {
				r.mu.Lock()
				n := len(r.buf)
				if n > voiceFrameSize {
					n = voiceFrameSize
				}
				if !deafened {
					for i := 0; i < n; i++ {
						sum := int32(s.playBuf[i]) + int32(r.buf[i])
						if sum > 32767 {
							sum = 32767
						} else if sum < -32768 {
							sum = -32768
						}
						s.playBuf[i] = int16(sum)
					}
				}
				r.buf = r.buf[n:]
				r.mu.Unlock()
			}
			s.remotesMu.Unlock()
			if err := stream.Write(); err != nil {
				return
			}
		}
	}()
	return <-started
}

// handleRemoteTrack reads and decodes one other participant's incoming
// audio track for as long as it lasts, feeding decoded PCM into that
// track's own buffer for the playback loop above to mix in.
func (s *VoiceSession) handleRemoteTrack(track *webrtc.TrackRemote) {
	decoder, err := opus.NewDecoder(voiceSampleRate, voiceChannels)
	if err != nil {
		return
	}
	source := &voiceRemoteSource{decoder: decoder}
	s.remotesMu.Lock()
	s.remotes[track.ID()] = source
	s.remotesMu.Unlock()
	defer func() {
		s.remotesMu.Lock()
		delete(s.remotes, track.ID())
		s.remotesMu.Unlock()
	}()

	pcm := make([]int16, voiceFrameSize)
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
		n, err := decoder.Decode(packet.Payload, pcm)
		if err != nil {
			continue
		}
		source.mu.Lock()
		source.buf = append(source.buf, pcm[:n]...)
		source.mu.Unlock()
	}
}

func (s *VoiceSession) close() {
	s.closeOnce.Do(func() {
		close(s.stopped)
		// captureStream and playStream are stopped and closed by their
		// own goroutines above, on whichever OS thread each was opened
		// and started on — not here. Touching them from this different
		// goroutine is exactly the cross-thread access pattern that
		// caused a real crash; the streams' own loops notice s.stopped
		// closing and clean themselves up on their own locked thread.
		if s.pc != nil {
			s.pc.Close()
		}
	})
}

// ── MIC TEST ─────────────────────────────────────────────────────
// A pure local loopback — captured audio gets written straight back out
// to the selected speaker, with no encoding, no network, and no server
// involved at all. Exists purely so device selection and the whole local
// capture/playback pipeline can be verified on their own, separately from
// whether a call actually connects.
type micTestSession struct {
	captureStream *portaudio.Stream
	playStream    *portaudio.Stream
	stopped       chan struct{}
	closeOnce     sync.Once

	// Only set when starting this test auto-deafened an active call, to
	// isolate the test's own loopback audio from live call audio playing
	// at the same time. Remembers the exact pre-test state so StopMicTest
	// restores it precisely — including leaving someone who was already
	// deafened before the test untouched — rather than assuming
	// "undeafened and unmuted" is always the right end state.
	autoDeafened    bool
	preTestMuted    bool
	preTestDeafened bool
}

var (
	micTestMu   sync.Mutex
	micTestSess *micTestSession
)

func (a *App) StartMicTest(micName, speakerName string) error {
	micTestMu.Lock()
	if micTestSess != nil {
		micTestMu.Unlock()
		return fmt.Errorf("mic test already running")
	}
	micTestMu.Unlock()

	inDevice, err := findDeviceByName(micName, true)
	if err != nil {
		return fmt.Errorf("no microphone available: %w", err)
	}
	outDevice, err := findDeviceByName(speakerName, false)
	if err != nil {
		return fmt.Errorf("no speaker available: %w", err)
	}

	// If a voice call is active, isolate the test's own loopback audio
	// from it by deafening for the duration — otherwise live call audio
	// and the test's mic-to-speaker loopback would play simultaneously
	// and be hard to tell apart. Only auto-deafen if not already
	// deafened, and remember the exact pre-test state either way so
	// StopMicTest can restore it precisely.
	var autoDeafened, preTestMuted, preTestDeafened bool
	a.voiceMu.Lock()
	inCall := a.voice != nil
	a.voiceMu.Unlock()
	if inCall {
		preTestMuted = a.voiceMuted.Load()
		preTestDeafened = a.voiceDeafened.Load()
		if !preTestDeafened {
			autoDeafened = true
			a.voiceDeafened.Store(true)
			a.voiceMuted.Store(true)
		}
	}

	started := make(chan error, 1)
	// If setup fails partway through below, the auto-deafen above already
	// applied but no session object will exist for StopMicTest to revert
	// it from — this covers that so a failed test never leaves an active
	// call stuck deafened.
	revertAutoDeafen := func() {
		if autoDeafened {
			a.voiceDeafened.Store(preTestDeafened)
			a.voiceMuted.Store(preTestMuted)
		}
	}
	go func() {
		// Same reasoning as VoiceSession's capture/playback above — the
		// whole lifecycle of both streams stays on one locked OS thread.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		captureBuf := make([]int16, voiceFrameSize)
		playBuf := make([]int16, voiceFrameSize)

		inParams := portaudio.StreamParameters{
			Input:           portaudio.StreamDeviceParameters{Device: inDevice, Channels: voiceChannels, Latency: inDevice.DefaultLowInputLatency},
			SampleRate:      voiceSampleRate,
			FramesPerBuffer: voiceFrameSize,
		}
		inStream, err := portaudio.OpenStream(inParams, captureBuf)
		if err != nil {
			revertAutoDeafen()
			started <- fmt.Errorf("open microphone: %w", err)
			return
		}
		outParams := portaudio.StreamParameters{
			Output:          portaudio.StreamDeviceParameters{Device: outDevice, Channels: voiceChannels, Latency: outDevice.DefaultLowOutputLatency},
			SampleRate:      voiceSampleRate,
			FramesPerBuffer: voiceFrameSize,
		}
		outStream, err := portaudio.OpenStream(outParams, playBuf)
		if err != nil {
			inStream.Close()
			revertAutoDeafen()
			started <- fmt.Errorf("open speaker: %w", err)
			return
		}
		if err := inStream.Start(); err != nil {
			inStream.Close()
			outStream.Close()
			revertAutoDeafen()
			started <- fmt.Errorf("start microphone: %w", err)
			return
		}
		if err := outStream.Start(); err != nil {
			inStream.Stop()
			inStream.Close()
			outStream.Close()
			revertAutoDeafen()
			started <- fmt.Errorf("start speaker: %w", err)
			return
		}

		sess := &micTestSession{
			captureStream: inStream, playStream: outStream, stopped: make(chan struct{}),
			autoDeafened: autoDeafened, preTestMuted: preTestMuted, preTestDeafened: preTestDeafened,
		}
		micTestMu.Lock()
		micTestSess = sess
		micTestMu.Unlock()
		started <- nil

		for {
			select {
			case <-sess.stopped:
				inStream.Stop()
				inStream.Close()
				outStream.Stop()
				outStream.Close()
				return
			default:
			}
			if err := inStream.Read(); err != nil {
				return
			}
			copy(playBuf, captureBuf)
			if err := outStream.Write(); err != nil {
				return
			}
		}
	}()
	return <-started
}

func (a *App) StopMicTest() {
	micTestMu.Lock()
	sess := micTestSess
	micTestSess = nil
	micTestMu.Unlock()
	if sess != nil {
		sess.closeOnce.Do(func() { close(sess.stopped) })
		if sess.autoDeafened {
			a.voiceDeafened.Store(sess.preTestDeafened)
			a.voiceMuted.Store(sess.preTestMuted)
		}
	}
}

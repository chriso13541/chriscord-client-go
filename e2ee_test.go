package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3/pkg/media/samplebuilder"
)

func testKey(t *testing.T) []byte {
	k := make([]byte, 32)
	rand.Read(k)
	return k
}

func TestFrameRoundTrip(t *testing.T) {
	aead, _ := newFrameAEAD(testKey(t))
	for _, kind := range []byte{kindAudio, kindVP8, kindH264} {
		for _, n := range []int{0, 1, 3, 100, 5000} {
			frame := make([]byte, n)
			rand.Read(frame)
			sealed := sealFrameWith(aead, 7, 42, kind, frame)
			body, kid, ctr, err := frameHeader(kind, sealed)
			if err != nil || kid != 7 || ctr != 42 {
				t.Fatalf("kind %c n %d: header %v %d %d", kind, n, err, kid, ctr)
			}
			got, err := openSealed(aead, kind, body)
			if err != nil || !bytes.Equal(got, frame) {
				t.Fatalf("kind %c n %d: open %v", kind, n, err)
			}
			// The wrong kind doesn't open (frames can't be moved between tracks).
			if kind == kindAudio {
				if _, err := openSealed(aead, kindVP8, body); err == nil {
					t.Fatal("opened as the wrong kind")
				}
			}
			// Nor does a tampered one.
			if n > 0 {
				bad := append([]byte(nil), body...)
				bad[0] ^= 1
				if _, err := openSealed(aead, kind, bad); err == nil {
					t.Fatal("tampered frame opened")
				}
			}
		}
	}
	// Plain media isn't mistaken for a sealed frame.
	if _, _, _, err := frameHeader(kindAudio, []byte{0xfc, 0xff, 0xfe}); err == nil {
		t.Fatal("plain frame accepted")
	}
}

// Sealed H.264 frames go through pion's packetiser and the same
// depacketiser + sample builder video.go uses, and come out intact.
func TestH264ThroughPion(t *testing.T) {
	aead, _ := newFrameAEAD(testKey(t))
	pay := &codecs.H264Payloader{}
	sb := samplebuilder.New(512, &codecs.H264Packet{}, 90000)
	var frames [][]byte
	seq := uint16(1000)
	for i := 0; i < 60; i++ {
		n := 50 + i*997
		if i%10 == 0 {
			n = 150000 // a big keyframe
		}
		f := make([]byte, n)
		rand.Read(f)
		// Plenty of zero runs, so the escaping is exercised.
		for j := 0; j+4 < n; j += 37 {
			f[j], f[j+1], f[j+2] = 0, 0, byte(j%4)
		}
		frames = append(frames, f)
		ps := pay.Payload(1200, sealFrameWith(aead, 1, uint32(i), kindH264, f))
		for k, p := range ps {
			// The last packet of a frame is marked, like rtpOut does.
			sb.Push(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(i * 3000), Marker: k == len(ps)-1}, Payload: p})
			seq++
		}
	}
	// The sample builder only releases a frame once the next one has
	// started, so push one more.
	sb.Push(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: 999999}, Payload: []byte{0x01, 0xaa}})
	got := 0
	for s := sb.Pop(); s != nil; s = sb.Pop() {
		body, _, ctr, err := frameHeader(kindH264, s.Data)
		if err != nil {
			t.Fatalf("frame %d: %v", got, err)
		}
		plain, err := openSealed(aead, kindH264, body)
		if err != nil || !bytes.Equal(plain, frames[ctr]) {
			t.Fatalf("frame %d didn't survive the trip: %v", ctr, err)
		}
		got++
	}
	if got != len(frames) {
		t.Fatalf("got %d of %d frames back", got, len(frames))
	}
}

func TestVP8ThroughPion(t *testing.T) {
	aead, _ := newFrameAEAD(testKey(t))
	pay := &codecs.VP8Payloader{}
	sb := samplebuilder.New(512, &codecs.VP8Packet{}, 90000)
	var frames [][]byte
	seq := uint16(5)
	for i := 0; i < 20; i++ {
		f := make([]byte, 100+i*3000)
		rand.Read(f)
		frames = append(frames, f)
		ps := pay.Payload(1200, sealFrameWith(aead, 1, uint32(i), kindVP8, f))
		for k, p := range ps {
			sb.Push(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(i * 3000), Marker: k == len(ps)-1}, Payload: p})
			seq++
		}
	}
	sb.Push(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: 999999}, Payload: []byte{0x10, 0xaa}})
	got := 0
	for s := sb.Pop(); s != nil; s = sb.Pop() {
		body, _, ctr, err := frameHeader(kindVP8, s.Data)
		if err != nil {
			t.Fatal(err)
		}
		plain, err := openSealed(aead, kindVP8, body)
		if err != nil || !bytes.Equal(plain, frames[ctr]) {
			t.Fatalf("vp8 frame %d: %v", ctr, err)
		}
		got++
	}
	if got != len(frames) {
		t.Fatalf("got %d of %d", got, len(frames))
	}
}

func TestReplayWindow(t *testing.T) {
	var w replayWindow
	for _, c := range []uint32{10, 12, 11} {
		if !w.check(c) {
			t.Fatalf("%d rejected", c)
		}
	}
	if w.check(11) || w.check(10) {
		t.Fatal("replay accepted")
	}
	if !w.check(5000) {
		t.Fatal("jump rejected")
	}
	if w.check(12) {
		t.Fatal("too-old accepted")
	}
	if !w.check(4999) || w.check(4999) {
		t.Fatal("in-window handling wrong")
	}
}

func TestKeyWrapAndHello(t *testing.T) {
	aPriv, aPub, _ := newEphemeral()
	bPriv, bPub, _ := newEphemeral()
	key := testKey(t)
	box, err := wrapKey(aPriv, aPub, bPub, "board", "alice", "bob", 3, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := unwrapKey(bPriv, bPub, aPub, "board", "alice", "bob", 3, box)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("unwrap: %v", err)
	}
	// Bound to the names, the channel and the key id.
	for _, tc := range [][4]interface{}{{"board", "alice", "carol", uint32(3)}, {"other", "alice", "bob", uint32(3)}, {"board", "alice", "bob", uint32(4)}} {
		if _, err := unwrapKey(bPriv, bPub, aPub, tc[0].(string), tc[1].(string), tc[2].(string), tc[3].(uint32), box); err == nil {
			t.Fatalf("unwrapped with %v", tc)
		}
	}
	idPub, idPriv, _ := ed25519.GenerateKey(rand.Reader)
	sig := ed25519.Sign(idPriv, helloMessage("board", "alice", aPub[:]))
	if !verifyHello(idPub, "board", "alice", aPub[:], sig) {
		t.Fatal("good hello rejected")
	}
	if verifyHello(idPub, "board", "mallory", aPub[:], sig) || verifyHello(idPub, "board2", "alice", aPub[:], sig) {
		t.Fatal("hello accepted for the wrong name or channel")
	}
}

func TestCallCode(t *testing.T) {
	a := callCode(map[string]string{"alice": "aa", "bob": "BB"})
	b := callCode(map[string]string{"bob": "bb", "alice": "aa"})
	if a != b || len(a) != 23 || strings.Count(a, " ") != 3 {
		t.Fatalf("codes %q %q", a, b)
	}
	if a == callCode(map[string]string{"alice": "aa", "bob": "bc"}) {
		t.Fatal("a swapped key gave the same code")
	}
}

// ── A whole call, through a pretend server ────────────────────────────────────

type fakeServer struct {
	mu      sync.Mutex
	clients map[string]*callCrypto
	ids     map[string]string
	queue   [][]byte
	board   string
}

func (s *fakeServer) add(t *testing.T, user string) *App {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	a := &App{username: user, domain: "test", account: &Account{PublicKey: pub, PrivateKey: priv}}
	a.e2ee = newCallCrypto(a)
	a.e2ee.transport = func(v interface{}) error {
		raw, _ := json.Marshal(v)
		var m struct {
			BoardID string          `json:"board_id"`
			To      string          `json:"to"`
			Data    json.RawMessage `json:"data"`
		}
		json.Unmarshal(raw, &m)
		out := map[string]interface{}{"type": "voice_e2ee", "board_id": m.BoardID, "from": user,
			"from_key": s.ids[user], "data": m.Data}
		if m.To != "" {
			out["to"] = m.To
		}
		b, _ := json.Marshal(out)
		s.mu.Lock()
		s.queue = append(s.queue, b)
		s.mu.Unlock()
		return nil
	}
	s.mu.Lock()
	s.clients[user] = a.e2ee
	s.ids[user] = hex.EncodeToString(pub)
	s.mu.Unlock()
	return a
}

// pump delivers everything queued to whoever it's for, like ws.rs does.
func (s *fakeServer) pump() {
	for {
		s.mu.Lock()
		if len(s.queue) == 0 {
			s.mu.Unlock()
			return
		}
		msg := s.queue[0]
		s.queue = s.queue[1:]
		clients := map[string]*callCrypto{}
		for k, v := range s.clients {
			clients[k] = v
		}
		s.mu.Unlock()
		var m struct{ From, To string }
		json.Unmarshal(msg, &m)
		for u, c := range clients {
			if u != m.From && (m.To == "" || m.To == u) {
				c.handleMessage(msg)
			}
		}
	}
}

func (s *fakeServer) roster(users ...string) {
	ch := map[string][]string{s.board: users}
	s.mu.Lock()
	clients := map[string]*callCrypto{}
	for k, v := range s.clients {
		clients[k] = v
	}
	s.mu.Unlock()
	for _, u := range users {
		clients[u].updateRoster(ch)
	}
}

// settle lets key changes happen and arrive.
func (s *fakeServer) settle() {
	s.pump()
	time.Sleep(rotateDebounce + 100*time.Millisecond)
	s.pump()
	time.Sleep(switchDelay + 100*time.Millisecond)
	s.pump()
}

func mustOpen(t *testing.T, to *App, from string, kind byte, sealed, want []byte) {
	t.Helper()
	got, ok := to.e2ee.open(from, kind, sealed)
	if !ok || !bytes.Equal(got, want) {
		t.Fatalf("%s couldn't open %s's frame", to.username, from)
	}
}

func TestCallKeyExchange(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AppData", t.TempDir())
	s := &fakeServer{clients: map[string]*callCrypto{}, ids: map[string]string{}, board: "vc1"}
	alice, bob, carol := s.add(t, "alice"), s.add(t, "bob"), s.add(t, "carol")

	alice.e2ee.begin("vc1")
	s.roster("alice")
	s.pump()

	// Bob joins: join_voice → voice_state → his hello.
	bob.e2ee.begin("vc1")
	s.roster("alice", "bob")
	s.settle()

	voice := []byte("hello bob")
	mustOpen(t, bob, "alice", kindAudio, alice.e2ee.seal(kindAudio, voice), voice)
	pic := bytes.Repeat([]byte{0, 0, 1, 0x65, 0, 0, 0}, 300)
	mustOpen(t, alice, "bob", kindH264, bob.e2ee.seal(kindH264, pic), pic)

	// Replayed frames are dropped.
	f := alice.e2ee.seal(kindAudio, voice)
	mustOpen(t, bob, "alice", kindAudio, f, voice)
	if _, ok := bob.e2ee.open("alice", kindAudio, f); ok {
		t.Fatal("replay accepted")
	}

	// A frame sent before Carol joined stays closed to her.
	before := alice.e2ee.seal(kindAudio, []byte("before carol"))
	carol.e2ee.begin("vc1")
	s.roster("alice", "bob", "carol")
	s.settle()
	if _, ok := carol.e2ee.open("alice", kindAudio, before); ok {
		t.Fatal("a newcomer read what was sent before they joined")
	}
	now := []byte("hi carol")
	mustOpen(t, carol, "alice", kindAudio, alice.e2ee.seal(kindAudio, now), now)
	mustOpen(t, carol, "bob", kindAudio, bob.e2ee.seal(kindAudio, now), now)
	mustOpen(t, bob, "carol", kindAudio, carol.e2ee.seal(kindAudio, now), now)

	// Everyone sees the same call code.
	ca, cb, cc := alice.GetVoiceEncryption(), bob.GetVoiceEncryption(), carol.GetVoiceEncryption()
	if ca.Code != cb.Code || cb.Code != cc.Code {
		t.Fatalf("codes differ: %s / %s / %s", ca.Code, cb.Code, cc.Code)
	}
	for _, p := range ca.Peers {
		if p.Status != "ok" {
			t.Fatalf("alice sees %s as %s", p.User, p.Status)
		}
	}

	// Bob leaves; what Alice sends after can't be read with the keys he had.
	bob.e2ee.mu.Lock()
	bobHad := append([]*peerKey(nil), bob.e2ee.peers["alice"].keys...)
	bob.e2ee.mu.Unlock()
	bob.e2ee.end()
	s.mu.Lock()
	delete(s.clients, "bob")
	s.mu.Unlock()
	s.roster("alice", "carol")
	s.settle()
	after := alice.e2ee.seal(kindAudio, []byte("bob's gone"))
	body, _, _, _ := frameHeader(kindAudio, after)
	for _, k := range bobHad {
		if _, err := openSealed(k.aead, kindAudio, body); err == nil {
			t.Fatal("someone who left could still listen")
		}
	}
	mustOpen(t, carol, "alice", kindAudio, after, []byte("bob's gone"))

	// A hello that doesn't verify is ignored.
	s.mu.Lock()
	s.ids["mallory"] = s.ids["carol"]
	s.mu.Unlock()
	bad, _ := json.Marshal(map[string]interface{}{"type": "voice_e2ee", "board_id": "vc1", "from": "mallory",
		"from_key": s.ids["carol"], "data": map[string]interface{}{"t": "hello", "eph": "AAAA", "sig": "AAAA"}})
	alice.e2ee.handleMessage(bad)
	alice.e2ee.mu.Lock()
	_, has := alice.e2ee.peers["mallory"]
	alice.e2ee.mu.Unlock()
	if has {
		t.Fatal("unverified hello accepted")
	}

	alice.e2ee.end()
	if alice.e2ee.seal(kindAudio, voice) != nil {
		t.Fatal("still sealing after leaving")
	}
}

// Bob's connection drops (he vanishes from the call for a moment) and comes
// back with the same call key: both directions work again afterwards.
func TestCallReconnect(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AppData", t.TempDir())
	s := &fakeServer{clients: map[string]*callCrypto{}, ids: map[string]string{}, board: "vc1"}
	alice, bob := s.add(t, "alice"), s.add(t, "bob")
	alice.e2ee.begin("vc1")
	s.roster("alice")
	bob.e2ee.begin("vc1")
	s.roster("alice", "bob")
	s.settle()

	// Dropped: the server lists only Alice; Bob hears nothing meanwhile.
	alice.e2ee.updateRoster(map[string][]string{"vc1": {"alice"}})
	s.settle()
	// Back: Bob's app rebuilds its connection (same call, same keys).
	bob.e2ee.begin("vc1")
	s.roster("alice", "bob")
	s.settle()

	msg := []byte("welcome back")
	mustOpen(t, bob, "alice", kindAudio, alice.e2ee.seal(kindAudio, msg), msg)
	mustOpen(t, alice, "bob", kindAudio, bob.e2ee.seal(kindAudio, msg), msg)
}

// The real order on the one joining: the voice_state from their join_voice
// usually arrives before their call's connection (and begin) has started.
func TestJoinStateBeforeBegin(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AppData", t.TempDir())
	s := &fakeServer{clients: map[string]*callCrypto{}, ids: map[string]string{}, board: "vc1"}
	alice, bob := s.add(t, "alice"), s.add(t, "bob")
	alice.e2ee.begin("vc1")
	s.roster("alice")
	s.settle()
	s.roster("alice", "bob") // bob's join_voice → voice_state, to both
	bob.e2ee.begin("vc1")    // then his call's connection starts
	s.settle()
	ca, cb := alice.GetVoiceEncryption(), bob.GetVoiceEncryption()
	if ca.Code != cb.Code || len(cb.Peers) != 1 || cb.Peers[0].Status != "ok" || ca.Peers[0].Status != "ok" {
		t.Fatalf("after joining:\n alice %+v\n bob   %+v", ca, cb)
	}
	msg := []byte("x")
	mustOpen(t, bob, "alice", kindAudio, alice.e2ee.seal(kindAudio, msg), msg)
	mustOpen(t, alice, "bob", kindAudio, bob.e2ee.seal(kindAudio, msg), msg)
}

// A hello lost on the way is made up for by the periodic resync.
func TestLostHelloRecovers(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AppData", t.TempDir())
	s := &fakeServer{clients: map[string]*callCrypto{}, ids: map[string]string{}, board: "vc1"}
	alice, bob := s.add(t, "alice"), s.add(t, "bob")
	alice.e2ee.begin("vc1")
	s.roster("alice", "bob")
	bob.e2ee.begin("vc1")
	s.mu.Lock()
	s.queue = nil // both hellos lost
	s.mu.Unlock()
	s.settle()
	time.Sleep(resyncEvery + 300*time.Millisecond)
	s.settle()
	time.Sleep(resyncEvery)
	s.settle()
	msg := []byte("x")
	mustOpen(t, bob, "alice", kindAudio, alice.e2ee.seal(kindAudio, msg), msg)
	mustOpen(t, alice, "bob", kindAudio, bob.e2ee.seal(kindAudio, msg), msg)
	if alice.GetVoiceEncryption().Code != bob.GetVoiceEncryption().Code {
		t.Fatal("codes differ")
	}
}

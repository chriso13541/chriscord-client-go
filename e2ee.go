package main

// e2ee.go — end-to-end encryption for calls: voice, cameras, screen shares
// and their sound. The server keeps forwarding RTP exactly as before; it
// just can't read what's inside any more.
//
// How it works (the "simple" design — per-sender keys, swapped over the
// existing WebSocket; MLS can replace the key exchange later without
// touching the frame format):
//
//   - Each call, this app makes a fresh X25519 key pair (the call key) and
//     tells everyone in the channel with a "hello", signed by the account's
//     Ed25519 identity key. The server relays it with the sender's identity
//     key attached (from its users table), and the signature is checked
//     against that.
//   - This app seals every frame it sends with its own random sending key
//     (e2ee_frame.go) and hands that key to each other person in the call,
//     wrapped for their call key. Nobody else — the server included — can
//     unwrap it.
//   - Whenever someone joins or leaves, it makes a new sending key, hands it
//     to whoever's in the call now, and switches to it shortly after
//     (switchDelay — time for the new key to arrive). Someone who joins
//     can't read what was sent before, and someone who left can't read
//     what's sent after.
//   - A frame that doesn't open (no key from that person yet, or tampered
//     with) is dropped. Unencrypted media from an older app is dropped too
//     and that person shows as "can't encrypt" until they update.
//   - The call code (callCode) is worked out from everyone's identity keys:
//     everyone in the call should see the same one. Comparing it out loud
//     is what catches a server that swaps someone's key or shows people
//     different calls. Each person's identity key is also remembered per
//     server, and a change is flagged.
//
// What the server still sees: who's in which call and when, how much each
// person sends, and timing — but not the sound or picture.

import (
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	rotateDebounce = 200 * time.Millisecond // joins/leaves this close together make one new key
	switchDelay    = 800 * time.Millisecond // a new key is handed out this long before it's used
	keysKept       = 4                      // per person: their current key and a few before it
	helloGrace     = 5 * time.Second        // no hello after this long: their app can't encrypt
	resyncEvery    = 2 * time.Second        // how often to chase anyone keys haven't reached yet
)

type sendKey struct {
	kid     uint32
	raw     []byte
	aead    cipher.AEAD
	counter uint32
}

type peerKey struct {
	kid  uint32
	aead cipher.AEAD
	win  replayWindow
}

type e2eePeer struct {
	identity    ed25519.PublicKey
	identityHex string
	eph         [32]byte
	keys        []*peerKey // oldest first
	changed     bool       // their identity key isn't the one remembered for them
}

// callCrypto is the encryption state of the call this app is in (one at a
// time). It lives on the App, not the VoiceSession, so a reconnect that
// rebuilds the connection keeps its keys.
type callCrypto struct {
	app *App

	mu      sync.Mutex
	board   string // "" when not in a call
	me      string
	myIDHex string
	domain  string
	ephPriv [32]byte
	ephPub  [32]byte

	cur      *sendKey // frames are sealed with this
	next     *sendKey // handed out already; used from switchAt
	switchAt time.Time
	lastKID  uint32
	rotate   *time.Timer

	peers       map[string]*e2eePeer // everyone whose hello checked out
	roster      map[string]time.Time // the others in this call (voice_state), and since when
	channels    map[string][]string  // the latest voice_state, kept even between calls (see updateRoster)
	resync      *time.Timer          // the periodic check for anyone keys haven't reached (resyncLoop)
	dropped     int                  // frames that didn't open, since the last log line
	droppedLast time.Time

	// transport sends one message to the server (sendVoiceJSON; swapped
	// out in tests).
	transport func(v interface{}) error
}

func newCallCrypto(a *App) *callCrypto { return &callCrypto{app: a, transport: a.sendVoiceJSON} }

// e2eeMsg is one relayed key-exchange message, as the server forwards it.
type e2eeMsg struct {
	BoardID string `json:"board_id"`
	From    string `json:"from"`
	FromKey string `json:"from_key"` // the sender's identity key (hex), from the server's records
	To      string `json:"to"`
	Data    struct {
		T     string `json:"t"` // "hello" or "key"
		Eph   string `json:"eph"`
		Sig   string `json:"sig"`
		Reply bool   `json:"reply"`
		KID   uint32 `json:"kid"`
		Box   string `json:"box"`
	} `json:"data"`
}

type outMsg struct {
	to   string
	data map[string]interface{}
}

func newSendKey(kid uint32) (*sendKey, error) {
	raw := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return nil, err
	}
	aead, err := newFrameAEAD(raw)
	if err != nil {
		return nil, err
	}
	return &sendKey{kid: kid, raw: raw, aead: aead}, nil
}

// begin starts (or, for the same channel, carries on) this call's
// encryption, and says hello to everyone in it. Called when a call's
// connection is built — on joining, and on a reconnect.
func (c *callCrypto) begin(board string) {
	a := c.app
	a.mu.Lock()
	me, domain := a.username, a.domain
	var idHex string
	var priv ed25519.PrivateKey
	if a.account != nil {
		idHex, priv = hex.EncodeToString(a.account.PublicKey), a.account.PrivateKey
	}
	a.mu.Unlock()
	if priv == nil {
		log.Printf("e2ee: no identity key — can't encrypt this call")
		return
	}

	c.mu.Lock()
	if c.board != board || c.me != me || c.domain != domain {
		c.resetLocked()
		var err error
		c.ephPriv, c.ephPub, err = newEphemeral()
		if err == nil {
			c.cur, err = newSendKey(1)
		}
		if err != nil {
			c.mu.Unlock()
			log.Printf("e2ee: making keys: %v", err)
			return
		}
		c.board, c.me, c.myIDHex, c.domain, c.lastKID = board, me, idHex, domain, 1
		c.peers, c.roster = map[string]*e2eePeer{}, map[string]time.Time{}
		c.resync = time.AfterFunc(resyncEvery, c.resyncLoop)
		log.Printf("e2ee: call in %s is end-to-end encrypted", board)
	}
	// Who's in the call already: the voice_state from joining has usually
	// arrived before this (the call's connection takes a moment to set up),
	// while there was no call here yet to apply it to.
	if c.channels != nil {
		c.applyRosterLocked(c.channels)
	}
	hello := c.helloLocked(priv, false)
	c.mu.Unlock()
	c.send([]outMsg{{data: hello}})
	c.emitState()
}

// end forgets the call's keys (leaving it).
func (c *callCrypto) end() {
	c.mu.Lock()
	was := c.board != ""
	c.resetLocked()
	c.mu.Unlock()
	if was {
		c.emitState()
	}
}

func (c *callCrypto) resetLocked() {
	if c.rotate != nil {
		c.rotate.Stop()
		c.rotate = nil
	}
	if c.resync != nil {
		c.resync.Stop()
		c.resync = nil
	}
	for _, k := range []*sendKey{c.cur, c.next} {
		if k != nil {
			clear(k.raw)
		}
	}
	clear(c.ephPriv[:])
	c.board, c.cur, c.next, c.peers, c.roster = "", nil, nil, nil, nil
}

func (c *callCrypto) helloLocked(priv ed25519.PrivateKey, reply bool) map[string]interface{} {
	sig := ed25519.Sign(priv, helloMessage(c.board, c.me, c.ephPub[:]))
	return map[string]interface{}{
		"t": "hello", "v": 1, "reply": reply,
		"eph": base64.StdEncoding.EncodeToString(c.ephPub[:]),
		"sig": base64.StdEncoding.EncodeToString(sig),
	}
}

// keyMsgLocked wraps one of this app's sending keys for one peer.
func (c *callCrypto) keyMsgLocked(user string, p *e2eePeer, k *sendKey) (outMsg, bool) {
	// Only to someone the signed member list lets into this call
	// (roster_app.go): anyone else may speak, but can't listen.
	if !c.app.rosterAllowsCall(p.identityHex, c.board) {
		return outMsg{}, false
	}
	box, err := wrapKey(c.ephPriv, c.ephPub, p.eph, c.board, c.me, user, k.kid, k.raw)
	if err != nil {
		log.Printf("e2ee: wrapping a key for %s: %v", user, err)
		return outMsg{}, false
	}
	return outMsg{to: user, data: map[string]interface{}{
		"t": "key", "v": 1, "kid": k.kid, "box": base64.StdEncoding.EncodeToString(box),
	}}, true
}

func (c *callCrypto) send(msgs []outMsg) {
	c.mu.Lock()
	board := c.board
	c.mu.Unlock()
	if board == "" {
		return
	}
	for _, m := range msgs {
		v := map[string]interface{}{"type": "voice_e2ee", "board_id": board, "data": m.data}
		if m.to != "" {
			v["to"] = m.to
		}
		if err := c.transport(v); err != nil {
			log.Printf("e2ee: sending %v: %v", m.data["t"], err)
		}
	}
}

// ── Frames ────────────────────────────────────────────────────────────────────

// seal encrypts one outgoing frame. nil means don't send it (not in a call).
func (c *callCrypto) seal(kind byte, frame []byte) []byte {
	c.mu.Lock()
	if c.cur == nil {
		c.mu.Unlock()
		return nil
	}
	if c.next != nil && !time.Now().Before(c.switchAt) {
		clear(c.cur.raw)
		c.cur, c.next = c.next, nil
	}
	k := c.cur
	counter := k.counter
	k.counter++
	if k.counter == 0 { // 4 billion frames on one key: get a new one
		c.scheduleRotateLocked()
	}
	c.mu.Unlock()
	return sealFrameWith(k.aead, byte(k.kid), counter, kind, frame)
}

// open decrypts one frame from sender. ok is false if it didn't open, and
// the frame should be dropped.
func (c *callCrypto) open(sender string, kind byte, frame []byte) ([]byte, bool) {
	sealed, kid, counter, err := frameHeader(kind, frame)
	if err != nil {
		c.noteDropped(sender, "not encrypted")
		return nil, false
	}
	c.mu.Lock()
	var pk *peerKey
	if p := c.peers[sender]; p != nil {
		for i := len(p.keys) - 1; i >= 0; i-- {
			if byte(p.keys[i].kid) == kid {
				pk = p.keys[i]
				break
			}
		}
	}
	c.mu.Unlock()
	if pk == nil {
		c.noteDropped(sender, "no key yet")
		return nil, false
	}
	plain, err := openSealed(pk.aead, kind, sealed)
	if err != nil {
		c.noteDropped(sender, "didn't decrypt")
		return nil, false
	}
	// Only a frame that opened counts towards replay protection, so junk
	// can't use up counters.
	c.mu.Lock()
	fresh := pk.win.check(counter)
	c.mu.Unlock()
	if !fresh {
		return nil, false
	}
	return plain, true
}

func (c *callCrypto) noteDropped(sender, why string) {
	c.mu.Lock()
	c.dropped++
	n := c.dropped
	logIt := time.Since(c.droppedLast) > 5*time.Second
	if logIt {
		c.dropped, c.droppedLast = 0, time.Now()
	}
	c.mu.Unlock()
	if logIt {
		log.Printf("e2ee: dropped %d frame(s) recently (latest from %s: %s)", n, sender, why)
	}
}

// ── Key exchange ──────────────────────────────────────────────────────────────

// handleMessage takes one relayed voice_e2ee message.
func (c *callCrypto) handleMessage(raw []byte) {
	var m e2eeMsg
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	c.mu.Lock()
	if c.board == "" || m.BoardID != c.board || m.From == "" || m.From == c.me || (m.To != "" && m.To != c.me) {
		c.mu.Unlock()
		return
	}
	var out []outMsg
	changedState := false
	switch m.Data.T {
	case "hello":
		out, changedState = c.onHelloLocked(&m)
	case "key":
		changedState = c.onKeyLocked(&m)
	}
	c.mu.Unlock()
	c.send(out)
	if changedState {
		c.emitState()
	}
}

func (c *callCrypto) onHelloLocked(m *e2eeMsg) ([]outMsg, bool) {
	identity, err1 := hex.DecodeString(m.FromKey)
	eph, err2 := base64.StdEncoding.DecodeString(m.Data.Eph)
	sig, err3 := base64.StdEncoding.DecodeString(m.Data.Sig)
	if err1 != nil || err2 != nil || err3 != nil || !verifyHello(identity, c.board, m.From, eph, sig) {
		log.Printf("e2ee: %s's call key didn't check out — ignoring it", m.From)
		return nil, false
	}
	var ephArr [32]byte
	copy(ephArr[:], eph)
	idHex := strings.ToLower(m.FromKey)

	p := c.peers[m.From]
	isNew := p == nil || p.eph != ephArr || p.identityHex != idHex
	if isNew {
		p = &e2eePeer{identity: identity, identityHex: idHex, eph: ephArr}
		p.changed = !pinIdentity(c.domain, m.From, idHex)
		if p.changed {
			log.Printf("e2ee: WARNING: %s's identity key is different from the one seen before", m.From)
		}
		c.peers[m.From] = p
	}

	var out []outMsg
	if !m.Data.Reply {
		// They've just joined (or reconnected): they need this app's call
		// key too, to send it theirs.
		a := c.app
		a.mu.Lock()
		var priv ed25519.PrivateKey
		if a.account != nil {
			priv = a.account.PrivateKey
		}
		a.mu.Unlock()
		if priv != nil {
			out = append(out, outMsg{to: m.From, data: c.helloLocked(priv, true)})
		}
	}
	if _, inCall := c.roster[m.From]; inCall {
		if isNew {
			// Someone new: a new key for everyone, which they get too —
			// never the one in use before they arrived.
			c.scheduleRotateLocked()
		} else {
			// The same person, same call key: one of the two connections was
			// rebuilt, and a key may have been missed meanwhile (or they
			// dropped this app's keys while it looked gone). Send them again
			// — they already had them, and duplicates are ignored.
			for _, k := range []*sendKey{c.cur, c.next} {
				if k == nil {
					continue
				}
				if km, ok := c.keyMsgLocked(m.From, p, k); ok {
					out = append(out, km)
				}
			}
		}
	}
	return out, isNew
}

func (c *callCrypto) onKeyLocked(m *e2eeMsg) bool {
	p := c.peers[m.From]
	if p == nil {
		return false // no hello from them (yet): nothing to unwrap it with
	}
	box, err := base64.StdEncoding.DecodeString(m.Data.Box)
	if err != nil {
		return false
	}
	raw, err := unwrapKey(c.ephPriv, c.ephPub, p.eph, c.board, m.From, c.me, m.Data.KID, box)
	if err != nil {
		log.Printf("e2ee: a key from %s didn't unwrap: %v", m.From, err)
		return false
	}
	for _, k := range p.keys {
		if k.kid == m.Data.KID {
			return false // already have it
		}
	}
	aead, err := newFrameAEAD(raw)
	clear(raw)
	if err != nil {
		return false
	}
	p.keys = append(p.keys, &peerKey{kid: m.Data.KID, aead: aead})
	sort.Slice(p.keys, func(i, j int) bool { return p.keys[i].kid < p.keys[j].kid })
	if len(p.keys) > keysKept {
		p.keys = p.keys[len(p.keys)-keysKept:]
	}
	return len(p.keys) == 1 // their first key: they're now audible
}

// updateRoster takes who's in which call (voice_state). Someone arriving
// or leaving this call means a new sending key. Kept even when not in a
// call, for begin to start from.
func (c *callCrypto) updateRoster(channels map[string][]string) {
	c.mu.Lock()
	c.channels = channels
	if c.board == "" {
		c.mu.Unlock()
		return
	}
	changed, added := c.applyRosterLocked(channels)
	c.mu.Unlock()
	if changed || added {
		c.emitState()
	}
}

func (c *callCrypto) applyRosterLocked(channels map[string][]string) (changed, added bool) {
	now := map[string]bool{}
	for _, u := range channels[c.board] {
		if u != c.me {
			now[u] = true
		}
	}
	for u := range c.roster {
		if !now[u] {
			delete(c.roster, u)
			delete(c.peers, u) // they come back with a fresh hello
			changed = true
		}
	}
	for u := range now {
		if _, ok := c.roster[u]; !ok {
			c.roster[u] = time.Now()
			added = true
			if c.peers[u] != nil {
				changed = true // their hello came first: give them a key now
			}
		}
	}
	if changed {
		c.scheduleRotateLocked()
	}
	return changed, added
}

// resyncLoop runs every resyncEvery during a call. Anyone in it whose
// hello hasn't arrived, or whose keys haven't, gets this app's hello again
// (just them): that makes them answer with theirs and send their keys
// again. So a message that got lost on the way — a reconnect, a race at
// joining — only costs a couple of seconds, not the call. It also
// refreshes what the page shows (someone with no hello for helloGrace
// shows as unable to encrypt).
func (c *callCrypto) resyncLoop() {
	a := c.app
	a.mu.Lock()
	var priv ed25519.PrivateKey
	if a.account != nil {
		priv = a.account.PrivateKey
	}
	a.mu.Unlock()

	c.mu.Lock()
	if c.board == "" || priv == nil {
		c.mu.Unlock()
		return
	}
	var out []outMsg
	for u, since := range c.roster {
		p := c.peers[u]
		if (p == nil || len(p.keys) == 0) && time.Since(since) >= resyncEvery {
			out = append(out, outMsg{to: u, data: c.helloLocked(priv, false)})
		}
	}
	c.resync = time.AfterFunc(resyncEvery, c.resyncLoop)
	c.mu.Unlock()
	c.send(out)
	c.emitState()
}

func (c *callCrypto) scheduleRotateLocked() {
	if c.rotate == nil {
		c.rotate = time.AfterFunc(rotateDebounce, c.doRotate)
	}
}

// doRotate makes a new sending key and hands it to everyone in the call.
func (c *callCrypto) doRotate() {
	c.mu.Lock()
	c.rotate = nil
	if c.board == "" {
		c.mu.Unlock()
		return
	}
	if c.next != nil { // the last one's been out for a while already: start using it
		clear(c.cur.raw)
		c.cur = c.next
	}
	k, err := newSendKey(c.lastKID + 1)
	if err != nil {
		c.mu.Unlock()
		log.Printf("e2ee: new key: %v", err)
		return
	}
	c.lastKID++
	c.next, c.switchAt = k, time.Now().Add(switchDelay)
	var out []outMsg
	for u := range c.roster {
		if p := c.peers[u]; p != nil {
			if km, ok := c.keyMsgLocked(u, p, k); ok {
				out = append(out, km)
			}
		}
	}
	c.mu.Unlock()
	c.send(out)
}

// ── What the page shows ───────────────────────────────────────────────────────

type e2eePeerState struct {
	User   string `json:"user"`
	Status string `json:"status"` // "ok", "waiting", "unsupported", "changed"
}

type e2eeState struct {
	Active bool            `json:"active"`
	Code   string          `json:"code"`
	Peers  []e2eePeerState `json:"peers"`
}

func (c *callCrypto) snapshot() e2eeState {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := e2eeState{Peers: []e2eePeerState{}}
	if c.board == "" {
		return st
	}
	st.Active = true
	members := map[string]string{c.me: c.myIDHex}
	for u, since := range c.roster {
		p := c.peers[u]
		s := "waiting"
		switch {
		case p == nil && time.Since(since) > helloGrace:
			s = "unsupported"
		case p != nil && p.changed:
			s = "changed"
		case p != nil && c.app.rosterIsAutomated(p.identityHex):
			s = "playonly" // a bot or plugin: its sound reaches you, yours never reaches it
		case p != nil && !c.app.rosterAllowsCall(p.identityHex, c.board):
			s = "unapproved"
		case p != nil && len(p.keys) > 0:
			s = "ok"
		}
		if p != nil {
			members[u] = p.identityHex
		} else {
			members[u] = "?"
		}
		st.Peers = append(st.Peers, e2eePeerState{User: u, Status: s})
	}
	sort.Slice(st.Peers, func(i, j int) bool { return st.Peers[i].User < st.Peers[j].User })
	st.Code = callCode(members)
	return st
}

func (c *callCrypto) emitState() {
	if c.app.ctx == nil {
		return
	}
	wailsruntime.EventsEmit(c.app.ctx, "voice:e2ee", c.snapshot())
}

// GetVoiceEncryption is the call's encryption state, for the page: whether
// it's on, the call code, and each other person's status.
func (a *App) GetVoiceEncryption() e2eeState { return a.e2ee.snapshot() }

// handleVoiceE2EE: a voice_e2ee message from the server (wsReader).
func (a *App) handleVoiceE2EE(raw []byte) { a.e2ee.handleMessage(raw) }

// ── Remembered identity keys ──────────────────────────────────────────────────

const e2eePinsFile = "chriscord_e2ee_keys.json"

var pinsMu sync.Mutex

// pinIdentity remembers the identity key seen for user on this server, and
// reports whether it matches the one seen before (true the first time). A
// new key replaces the old one, so it's flagged once, in the call where it
// changed.
func pinIdentity(domain, user, idHex string) bool {
	pinsMu.Lock()
	defer pinsMu.Unlock()
	path := filepath.Join(chriscordConfigDir(), e2eePinsFile)
	pins := map[string]map[string]string{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &pins)
	}
	if pins[domain] == nil {
		pins[domain] = map[string]string{}
	}
	old, seen := pins[domain][user]
	if seen && old == idHex {
		noteIdentity(domain, user, idHex, "") // the directory check (keycheck.go)
		return true
	}
	pins[domain][user] = idHex
	if data, err := json.MarshalIndent(pins, "", "  "); err == nil {
		os.MkdirAll(chriscordConfigDir(), 0700)
		os.WriteFile(path, data, 0600)
	}
	noteIdentity(domain, user, idHex, old) // warns if it changed (keycheck.go)
	return !seen
}

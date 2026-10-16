package main

// presence.go — your status, sealed so the server can't read it.
//
// Online / away / invisible and the game you're playing (games.go) go to the
// server as one encrypted blob (ws "presence"), sealed with a server-wide
// key that every member has — the same generations-and-boxes scheme as a
// channel's (textcrypt.go, key id "@presence"), replaced when someone
// leaves. The server stores the latest blob and passes it on; each app opens
// it and shows you accordingly — invisible included: your app says so
// inside, and everyone's app hides you.
//
// So nothing in what the server sees changes with your status: every blob is
// padded to the same size, and one goes out about once a minute whether
// anything changed or not (and a moment after a change — with a little
// random delay, so a change doesn't stand out from the regular ones). What
// the server can still see is that you're connected.
//
// Servers from before this (no encrypted_presence when joining) get the old
// readable set_status / set_activity instead.

import (
	"encoding/json"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	presenceKeyID = "@presence"
	presenceEvery = 60 * time.Second
	// Every status is padded to this many bytes before sealing (the whole
	// sealed message, name and all), so they're all the same size whatever's
	// in them. Room for the longest: a 128-character game, custom status
	// and shared window's title, and a signed directory log head.
	presencePadTo    = 2800
	presenceNewGrace = 3 * time.Second // someone just connected: their status should arrive by then
	// How long a fetched status key is used before checking with the server
	// again (whether someone's left and it needs replacing).
	presenceKeyRecheck = 5 * time.Minute
)

// encPresence: the connected server takes encrypted statuses.
var encPresence atomic.Bool

// presencePlain is what's inside a status blob.
type presencePlain struct {
	S string `json:"s"`           // "online" | "idle" | "invisible"
	G string `json:"g,omitempty"` // game
	T string `json:"t,omitempty"` // playing since (RFC 3339)
	C string `json:"c,omitempty"` // custom status text
	E string `json:"e,omitempty"` // custom status emoji
	K string `json:"k,omitempty"` // when the custom status was set (RFC 3339)
	V bool   `json:"v,omitempty"` // this device is in a call
	W string `json:"w,omitempty"` // the window this device is sharing in the call (activity.go)
	L string `json:"l,omitempty"` // the newest chriscord.app log head this app has checked (translog.go)
	P string `json:"p"`           // padding
}

type peerPresence struct{ status, game, since, custom, emoji, window string }

// devicePresence is one of someone's devices' status. Someone signed in on
// two computers has two, and the member list shows one person: see
// mergeDevices.
type devicePresence struct {
	status, game, since, custom, emoji, customAt string
	window                                       string // the window they're sharing ("" for none, or a whole screen)
	inCall                                       bool
}

var presenceState struct {
	mu        sync.Mutex
	peers     map[string]map[string]devicePresence // username → device → its opened status
	blobs     map[string]map[string]string         // username → device → its latest blob (to open again once the key arrives)
	firstSeen map[string]time.Time                 // when each online person was first listed
	lastUsers *serverMsg                           // the last "users" message, to show again with statuses applied
	kick      chan struct{}                        // publish soon (something changed)
	stop      chan struct{}
	jobs      chan func() // opening blobs, one at a time, off the reader
}

func init() {
	presenceState.jobs = make(chan func(), 256)
	go func() {
		for job := range presenceState.jobs {
			job()
		}
	}()
}

func presenceJob(f func()) {
	select {
	case presenceState.jobs <- f:
	default: // backed up: drop it — the next regular blob replaces it anyway
	}
}

// startPresence begins publishing for a newly connected server.
func (a *App) startPresence() {
	presenceState.mu.Lock()
	if presenceState.stop != nil {
		close(presenceState.stop)
	}
	stop := make(chan struct{})
	kick := make(chan struct{}, 1)
	presenceState.stop, presenceState.kick = stop, kick
	presenceState.peers = map[string]map[string]devicePresence{}
	presenceState.blobs = map[string]map[string]string{}
	presenceState.firstSeen = map[string]time.Time{}
	presenceState.lastUsers = nil
	presenceState.mu.Unlock()
	go func() {
		a.publishPresence()
		tick := time.NewTicker(presenceEvery)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				a.publishPresence()
			case <-kick:
				select { // a moment later, a little at random
				case <-stop:
					return
				case <-time.After(500*time.Millisecond + time.Duration(rand.Intn(2000))*time.Millisecond):
				}
				a.publishPresence()
			}
		}
	}()
}

// presenceChanged: your status or game changed — send it soon.
func (a *App) presenceChanged() {
	presenceState.mu.Lock()
	kick := presenceState.kick
	presenceState.mu.Unlock()
	if kick != nil {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
}

func (a *App) publishPresence() {
	if !encPresence.Load() {
		return
	}
	p := presencePlain{S: a.currentPresence()}
	gameState.mu.Lock()
	if gameState.current != "" {
		p.G = gameState.current
		p.T = gameState.since.UTC().Format(time.RFC3339)
	}
	gameState.mu.Unlock()
	if cs := a.activeCustomStatus(); cs != nil {
		p.C, p.E, p.K = cs.Text, cs.Emoji, cs.SetAt
	}
	p.L = ownLogHead()
	p.V = a.InVoiceCall()
	if p.V {
		p.W = sharingWindowTitle()
	}
	data := presenceJSON(p)
	// Pad so the sealed message is always presencePadTo bytes: measured
	// as it'll be sealed (inside the message with your name), since that's
	// what the server sees the size of.
	if id, ok := a.text.identity(); ok {
		outer, _ := json.Marshal(textPlain{U: id.me, T: string(data)})
		if n := presencePadTo - len(outer); n > 0 {
			p.P = strings.Repeat(" ", n)
			data = presenceJSON(p)
		}
	}
	// The key as fetched in the last few minutes, if there is one — a fresh
	// look at the server each minute (who's a member, does it need a new
	// generation) would be needless work for it; every few minutes is
	// enough. Otherwise the full way (sealWith fetches, and makes a new
	// generation when someone's left).
	blob, ok := a.text.sealCached(presenceKeyID, string(data), presenceKeyRecheck)
	var err error
	if !ok {
		blob, err = a.text.sealWith(presenceKeyID, string(data), nil)
	}
	if err != nil || blob == "" {
		return // no key yet / not connected: the next one tries again
	}
	a.sendWS(map[string]interface{}{"type": "presence", "blob": blob})
}

// presenceJSON: the status as JSON, without HTML escaping (so "<" stays one
// byte and the size is predictable).
func presenceJSON(p presencePlain) []byte {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(p)
	return []byte(strings.TrimRight(b.String(), "\n"))
}

// presenceUsers handles a "users" message on a server with encrypted
// statuses: opens the blobs in it, then shows the list.
func (a *App) presenceUsers(msg serverMsg) {
	m := msg
	presenceJob(func() {
		now := time.Now()
		presenceState.mu.Lock()
		online := map[string]bool{}
		for _, u := range m.Online {
			online[u] = true
			if _, ok := presenceState.firstSeen[u]; !ok {
				presenceState.firstSeen[u] = now
			}
		}
		for u := range presenceState.firstSeen {
			if !online[u] {
				delete(presenceState.firstSeen, u)
				delete(presenceState.peers, u)
				delete(presenceState.blobs, u)
			}
		}
		presenceState.lastUsers = &m
		// Who has which devices now: the list is the whole truth, so a
		// device that's gone (and its status) is dropped.
		devices := m.PresenceDevices
		if devices == nil { // a server from before devices: one each
			devices = map[string]map[string]string{}
			for u, b := range m.PresenceBlobs {
				devices[u] = map[string]string{"": b}
			}
		}
		for u, have := range presenceState.peers {
			for d := range have {
				if _, ok := devices[u][d]; !ok {
					delete(have, d)
					delete(presenceState.blobs[u], d)
				}
			}
		}
		presenceState.mu.Unlock()
		for u, ds := range devices {
			for d, blob := range ds {
				a.openPresence(u, d, blob)
			}
		}
		a.emitUsers()
	})
}

// presenceBlob handles someone's new status blob, from one of their devices.
func (a *App) presenceBlob(user, device, blob string) {
	presenceJob(func() {
		if a.openPresence(user, device, blob) {
			a.emitUsers()
		}
	})
}

// presenceGone: one of someone's devices disconnected (they're still on
// another): its status no longer counts.
func (a *App) presenceGone(user, device string) {
	presenceJob(func() {
		presenceState.mu.Lock()
		delete(presenceState.peers[user], device)
		delete(presenceState.blobs[user], device)
		presenceState.mu.Unlock()
		a.emitUsers()
	})
}

// openPresence opens one blob into peers; false if it couldn't be (no key
// yet, or not theirs), leaving what was known.
func (a *App) openPresence(user, device, blob string) bool {
	presenceState.mu.Lock()
	if presenceState.blobs != nil {
		if presenceState.blobs[user] == nil {
			presenceState.blobs[user] = map[string]string{}
		}
		presenceState.blobs[user][device] = blob
	}
	presenceState.mu.Unlock()
	text, state := a.text.openContent(presenceKeyID, user, "", blob)
	if state != "ok" {
		return false
	}
	var p presencePlain
	if json.Unmarshal([]byte(text), &p) != nil {
		return false
	}
	switch p.S {
	case "online", "idle", "invisible":
	default:
		p.S = "online"
	}
	noteLogHead(user, p.L) // compare it with this app's copy of the log
	presenceState.mu.Lock()
	if presenceState.peers != nil {
		if presenceState.peers[user] == nil {
			presenceState.peers[user] = map[string]devicePresence{}
		}
		presenceState.peers[user][device] = devicePresence{status: p.S, game: cleanGameName(p.G), since: p.T,
			custom: cleanGameName(p.C), emoji: cleanEmoji(p.E), customAt: p.K, inCall: p.V, window: cleanGameName(p.W)}
	}
	presenceState.mu.Unlock()
	return true
}

// mergeDevices makes one status out of someone's devices':
//   - What they're playing comes from the device that's playing something
//     (and of those, the one in a call); with none playing, nothing.
//   - Their custom status is the most recently set one.
//   - Invisible on any device hides them; otherwise online if any device
//     is, else away.
func mergeDevices(devs map[string]devicePresence) (peerPresence, bool) {
	if len(devs) == 0 {
		return peerPresence{}, false
	}
	var out peerPresence
	anyOnline, anyInvisible := false, false
	var best *devicePresence
	bestKey := ""
	score := func(d *devicePresence) int {
		n := 0
		if d.game != "" {
			n += 2
		}
		if d.inCall {
			n++
		}
		return n
	}
	var newestCustom *devicePresence
	for k := range devs {
		d := devs[k]
		switch d.status {
		case "invisible":
			anyInvisible = true
		case "online":
			anyOnline = true
		}
		// A tie (both playing, neither in a call): the game started first,
		// so it doesn't flip between them each time one sends its status.
		if best == nil || score(&d) > score(best) ||
			(score(&d) == score(best) && (d.since < best.since || (d.since == best.since && k < bestKey))) {
			dd := d
			best, bestKey = &dd, k
		}
		if d.window != "" {
			out.window = d.window // the device that's sharing
		}
		if d.custom != "" || d.emoji != "" {
			if newestCustom == nil || d.customAt > newestCustom.customAt {
				dd := d
				newestCustom = &dd
			}
		}
	}
	switch {
	case anyInvisible:
		out.status = "invisible"
	case anyOnline:
		out.status = "online"
	default:
		out.status = "idle"
	}
	out.game, out.since = best.game, best.since
	if newestCustom != nil {
		out.custom, out.emoji = newestCustom.custom, newestCustom.emoji
	}
	return out, true
}

// presenceKeysArrived: the status key reached this app (someone shared it)
// — open the statuses that were waiting on it.
func (a *App) presenceKeysArrived() {
	presenceJob(func() {
		presenceState.mu.Lock()
		type w struct{ user, device, blob string }
		var waiting []w
		for u, ds := range presenceState.blobs {
			for d, b := range ds {
				if _, ok := presenceState.peers[u][d]; !ok {
					waiting = append(waiting, w{u, d, b})
				}
			}
		}
		presenceState.mu.Unlock()
		opened := false
		for _, x := range waiting {
			opened = a.openPresence(x.user, x.device, x.blob) || opened
		}
		if opened {
			a.emitUsers()
		}
	})
}

// emitUsers shows the member list with the opened statuses: invisible
// people left out, away shown, games added. Someone who's only just
// connected is held back a moment, until their status arrives — so an
// invisible person doesn't flash up as online.
func (a *App) emitUsers() {
	presenceState.mu.Lock()
	m := presenceState.lastUsers
	if m == nil || a.ctx == nil {
		presenceState.mu.Unlock()
		return
	}
	ev := usersEvent{All: m.All, Owner: m.Owner, Nicknames: m.Nicknames, GlobalNicknames: m.GlobalNicknames,
		Statuses: map[string]string{}, Activities: map[string]Activity{}, Custom: map[string]CustomStatusView{}, Sharing: map[string]string{}}
	waiting := false
	for _, u := range m.Online {
		p, known := mergeDevices(presenceState.peers[u])
		if !known {
			if time.Since(presenceState.firstSeen[u]) < presenceNewGrace {
				waiting = true
				continue
			}
			// An app from before encrypted statuses: what the server says.
			ev.Online = append(ev.Online, u)
			if st, ok := m.Presence[u]; ok {
				ev.Statuses[u] = st
			}
			if act, ok := m.Activities[u]; ok {
				ev.Activities[u] = act
			}
			continue
		}
		if p.status == "invisible" {
			continue
		}
		ev.Online = append(ev.Online, u)
		ev.Statuses[u] = p.status
		if p.game != "" {
			ev.Activities[u] = Activity{Name: p.game, Since: p.since}
		}
		if p.custom != "" || p.emoji != "" {
			ev.Custom[u] = CustomStatusView{Text: p.custom, Emoji: p.emoji}
		}
		if p.window != "" {
			ev.Sharing[u] = p.window
		}
	}
	presenceState.mu.Unlock()
	runtime.EventsEmit(a.ctx, "chat:users", ev)
	if waiting {
		time.AfterFunc(presenceNewGrace, func() { presenceJob(a.emitUsers) })
	}
}

// sealCached seals with the board's newest key as last fetched, if that was
// within maxAge and it didn't need replacing then; ok is false otherwise.
func (t *textCrypto) sealCached(board, text string, maxAge time.Duration) (string, bool) {
	id, ok := t.identity()
	if !ok {
		return "", false
	}
	t.mu.Lock()
	bk := t.boards[board]
	var key []byte
	gen := 0
	if bk != nil && !bk.rotate && bk.latest > 0 && time.Since(bk.fetchedAt) < maxAge {
		gen, key = bk.latest, bk.keys[bk.latest]
	}
	t.mu.Unlock()
	if key == nil {
		return "", false
	}
	blob, err := sealPlain(key, board, gen, textPlain{U: id.me, T: text})
	return blob, err == nil
}

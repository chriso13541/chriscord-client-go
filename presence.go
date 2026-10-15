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
	presenceKeyID    = "@presence"
	presenceEvery    = 60 * time.Second
	presencePadTo    = 384             // every status is this long (bytes of JSON) before sealing
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
	P string `json:"p"`           // padding
}

type peerPresence struct{ status, game, since string }

var presenceState struct {
	mu        sync.Mutex
	peers     map[string]peerPresence // username → their opened status
	blobs     map[string]string       // username → their latest blob (to open again once the key arrives)
	firstSeen map[string]time.Time    // when each online person was first listed
	lastUsers *serverMsg              // the last "users" message, to show again with statuses applied
	kick      chan struct{}           // publish soon (something changed)
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
	presenceState.peers = map[string]peerPresence{}
	presenceState.blobs = map[string]string{}
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
	data, _ := json.Marshal(p)
	if n := presencePadTo - len(data); n > 0 {
		p.P = strings.Repeat(" ", n)
		data, _ = json.Marshal(p)
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
		presenceState.mu.Unlock()
		for u, blob := range m.PresenceBlobs {
			a.openPresence(u, blob)
		}
		a.emitUsers()
	})
}

// presenceBlob handles someone's new status blob.
func (a *App) presenceBlob(user, blob string) {
	presenceJob(func() {
		if a.openPresence(user, blob) {
			a.emitUsers()
		}
	})
}

// openPresence opens one blob into peers; false if it couldn't be (no key
// yet, or not theirs), leaving what was known.
func (a *App) openPresence(user, blob string) bool {
	presenceState.mu.Lock()
	if presenceState.blobs != nil {
		presenceState.blobs[user] = blob
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
	game := []rune(strings.TrimSpace(p.G))
	if len(game) > 128 {
		game = game[:128]
	}
	presenceState.mu.Lock()
	if presenceState.peers != nil {
		presenceState.peers[user] = peerPresence{status: p.S, game: string(game), since: p.T}
	}
	presenceState.mu.Unlock()
	return true
}

// presenceKeysArrived: the status key reached this app (someone shared it)
// — open the statuses that were waiting on it.
func (a *App) presenceKeysArrived() {
	presenceJob(func() {
		presenceState.mu.Lock()
		waiting := map[string]string{}
		for u, b := range presenceState.blobs {
			if _, ok := presenceState.peers[u]; !ok {
				waiting[u] = b
			}
		}
		presenceState.mu.Unlock()
		opened := false
		for u, b := range waiting {
			opened = a.openPresence(u, b) || opened
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
		Statuses: map[string]string{}, Activities: map[string]Activity{}}
	waiting := false
	for _, u := range m.Online {
		p, known := presenceState.peers[u]
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

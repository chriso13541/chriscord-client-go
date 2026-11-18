package main

// pfpcache.go — profile pictures and banners kept on disk, fetched only
// when they change.
//
// Before, every picture was downloaded again on every start (the page only
// kept them in memory). Now:
//
//   - On connecting, the app asks the server once for its picture index
//     (GET /api/pfps): for each member, a cache id and the version of their
//     picture and banner (the timestamp of their last change); for unclaimed
//     imported people, their avatar's version.
//   - A picture is read from the disk cache if that version is there, and
//     downloaded (then saved, replacing older versions) only if not.
//   - pfp_updated / profile_updated from the server move the version on, so
//     the next look at that person fetches the new one. Reconnecting marks
//     the index stale (changes may have been missed meanwhile) and it's
//     asked for again on next use; so is a name it doesn't list yet (a new
//     member), at most every pfpIndexRetry.
//   - A server without the index (older) gets the old behaviour: fetched
//     each time, kept in memory by the page.
//
// Cache entries are per server: the id is a hash of the server's address
// and the person's identity key. Sharing them across servers would save a
// download now and then, but a server could then tell whether you'd
// already seen someone's picture elsewhere (you wouldn't ask for it) —
// worth not leaking, with servers treated as untrusted.
//
// Files: <user cache dir>/chriscord/pfps/<kind>-<id>-<version>, readable
// only by you. Pictures are what every member of that server can see; the
// cache adds nothing the page didn't already show.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const pfpIndexRetry = 15 * time.Second // most often an unlisted name refetches the index

type pfpIndexEntry struct {
	id     string // cache id (per server, see above)
	pfp    int64  // picture version; 0 = none
	banner int64  // banner version; 0 = none
}

type pfpIndexState struct {
	mu        sync.Mutex
	domain    string
	users     map[string]pfpIndexEntry
	loadedAt  time.Time
	stale     bool
	supported bool // the server answered /api/pfps
	tried     bool // asked this server at least once
}

var pfpIdx = &pfpIndexState{}

// pfpIndexWire is what GET /api/pfps answers.
type pfpIndexWire struct {
	Users []struct {
		Username string `json:"username"`
		Key      string `json:"key"`
		Pfp      int64  `json:"pfp"`
		Banner   int64  `json:"banner"`
	} `json:"users"`
	Imported []struct {
		Username string `json:"username"`
		ID       string `json:"id"`
		V        int64  `json:"v"`
	} `json:"imported"`
}

func pfpCacheID(domain, kind, who string) string {
	h := sha256.Sum256([]byte("chriscord-pfp-cache\x00" + strings.ToLower(domain) + "\x00" + kind + "\x00" + strings.ToLower(who)))
	return hex.EncodeToString(h[:20])
}

// pfpIndexStale: the connection was (re)made — changes may have been missed.
func pfpIndexStale() {
	pfpIdx.mu.Lock()
	pfpIdx.stale = true
	pfpIdx.mu.Unlock()
}

// pfpIndexSaw applies a pfp_updated (banner=false) or profile_updated
// (banner=true) broadcast. A profile change doesn't say whether it has a
// banner now, so it marks the index stale instead.
func pfpIndexSaw(domain, username string, version int64, banner bool) {
	pfpIdx.mu.Lock()
	defer pfpIdx.mu.Unlock()
	if !strings.EqualFold(pfpIdx.domain, domain) {
		return
	}
	e, ok := pfpIdx.users[username]
	if !ok || banner {
		pfpIdx.stale = true
		return
	}
	e.pfp = version
	pfpIdx.users[username] = e
}

// pfpLookup is username's index entry. supported is false when the server
// has no index (older server): fetch the old way then.
func (a *App) pfpLookup(username string) (e pfpIndexEntry, known, supported bool) {
	a.mu.Lock()
	domain := a.domain
	a.mu.Unlock()
	if domain == "" {
		return e, false, false
	}
	x := pfpIdx
	x.mu.Lock()
	if !strings.EqualFold(x.domain, domain) {
		x.domain, x.users, x.supported, x.tried, x.stale = domain, nil, false, false, false
	}
	e, known = x.users[username]
	need := !x.tried || x.stale || (x.supported && !known && time.Since(x.loadedAt) > pfpIndexRetry)
	x.mu.Unlock()
	if need {
		a.loadPfpIndex(domain)
		x.mu.Lock()
		e, known = x.users[username]
		x.mu.Unlock()
	}
	x.mu.Lock()
	supported = x.supported
	x.mu.Unlock()
	return e, known, supported
}

func (a *App) loadPfpIndex(domain string) {
	var w pfpIndexWire
	err := a.doJSON("GET", "/api/pfps", nil, &w)
	x := pfpIdx
	x.mu.Lock()
	defer x.mu.Unlock()
	if !strings.EqualFold(x.domain, domain) {
		return // switched servers meanwhile
	}
	x.tried, x.loadedAt = true, time.Now()
	if err != nil {
		var h *httpError
		if errors.As(err, &h) && h.code == http.StatusNotFound {
			x.supported, x.users, x.stale = false, nil, false // an older server
		}
		return // otherwise (a network blip) keep what there was; tried again later
	}
	users := make(map[string]pfpIndexEntry, len(w.Users)+len(w.Imported))
	for _, u := range w.Users {
		if u.Key == "" {
			continue
		}
		users[u.Username] = pfpIndexEntry{id: pfpCacheID(domain, "member", u.Key), pfp: u.Pfp, banner: u.Banner}
	}
	for _, m := range w.Imported {
		if _, taken := users[m.Username]; !taken && m.ID != "" {
			users[m.Username] = pfpIndexEntry{id: pfpCacheID(domain, "imported", m.ID), pfp: m.V}
		}
	}
	x.users, x.supported, x.stale = users, true, false
}

// ── On disk ───────────────────────────────────────────────────────────────────

func pfpCacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = filepath.Join(chriscordConfigDir(), "cache")
	} else {
		base = filepath.Join(base, "chriscord")
	}
	return filepath.Join(base, "pfps")
}

func pfpCacheName(kind, id string, version int64) string {
	return fmt.Sprintf("%s-%s-%d", kind, id, version)
}

func pfpCacheRead(name string) []byte {
	data, err := os.ReadFile(filepath.Join(pfpCacheDir(), name))
	if err != nil || len(data) == 0 {
		return nil
	}
	return data
}

// pfpCacheWrite saves a version and removes that person's older ones.
func pfpCacheWrite(kind, id string, version int64, data []byte) {
	dir := pfpCacheDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return
	}
	name := pfpCacheName(kind, id, version)
	if err := writeFileAtomic(filepath.Join(dir, name), data, 0600); err != nil {
		log.Printf("pfp: caching %s: %v", name, err)
		return
	}
	old, _ := filepath.Glob(filepath.Join(dir, kind+"-"+id+"-*"))
	for _, p := range old {
		if filepath.Base(p) != name {
			os.Remove(p)
		}
	}
}

// cachedImage returns kind/id/version from disk, or fetches it with fetch
// and keeps it.
func cachedImage(kind, id string, version int64, fetch func() ([]byte, error)) ([]byte, error) {
	name := pfpCacheName(kind, id, version)
	if data := pfpCacheRead(name); data != nil {
		return data, nil
	}
	data, err := fetch()
	if err != nil || len(data) == 0 {
		return data, err
	}
	pfpCacheWrite(kind, id, version, data)
	return data, nil
}

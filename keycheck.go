package main

// keycheck.go — a second opinion on other people's keys, from the
// chriscord.app directory.
//
// A server tells this app which identity key each member has. On its own,
// the app can only notice when a server changes its story about someone
// (pinIdentity, e2ee.go). With an online account it also asks
// chriscord.app who each key belongs to, and checks the answer itself
// instead of taking the directory's word for it:
//
//   - Every name in a key's history must be signed by that key (a
//     "claim"), so the directory can't put a name on a key that never
//     asked for it.
//   - A chriscord.app name, once seen with a key, is remembered here
//     (chriscord_directory_names.json). Keys never change, so if the
//     directory later shows that name on a different key, that's flagged:
//     it's what a hacked directory would have to do to impersonate
//     someone. The one innocent case, a name given up (the old key's own
//     signed history says it was deleted) and taken by someone new, is
//     noted rather than alarmed.
//
// A key change on a server is now a visible warning too (it used to only
// be logged), with what the directory says about the new key.
//
// Lookups are batched, cached for a few hours, and only made while an
// online account is unlocked: an offline account never contacts
// chriscord.app.

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	keyCheckFresh  = 6 * time.Hour // how long a directory answer is reused
	keyCheckRetry  = time.Minute   // after a failed lookup
	keyCheckBatch  = 300 * time.Millisecond
	keyCheckMax    = 100 // keys per lookup request (the service's limit)
	dirNamesFile   = "chriscord_directory_names.json"
	keyCheckWaitUI = 6 * time.Second
)

type dirHistory struct {
	Seq       int64  `json:"seq"`
	Event     string `json:"event"`
	Username  string `json:"username"`
	Nonce     string `json:"nonce"`
	Signature string `json:"signature"`
	At        int64  `json:"at"`
	Prev      string `json:"prev"` // its place in the public log (translog.go)
	Hash      string `json:"hash"`
}

type dirEntry struct {
	PublicKey     string       `json:"public_key"`
	Username      string       `json:"username"`
	EmailVerified bool         `json:"email_verified"`
	CreatedAt     int64        `json:"created_at"`
	Online        bool         `json:"online"`
	History       []dirHistory `json:"history"`
}

// keyVerdict is what this app concluded about one key.
type keyVerdict struct {
	Status  string // verified | unlisted | problem | error
	Name    string // its chriscord.app username (verified), or the name it had (unlisted)
	Note    string // worth knowing, not alarming
	Problem string // why it can't be trusted (problem)
	at      time.Time
}

type keyCheck struct {
	c *cloudClient

	mu      sync.Mutex
	cache   map[string]*keyVerdict
	waiting map[string][]chan struct{}
	timer   *time.Timer
	changes map[string]string // domain\x00user → the new key, for keys that changed this session
	warned  map[string]bool   // warnings already shown (domain\x00user\x00key)
	namesMu sync.Mutex        // chriscord_directory_names.json

	log *transLog // this app's copy of the directory's public log (translog.go)
}

func newKeyCheck(c *cloudClient) *keyCheck {
	k := &keyCheck{c: c, cache: map[string]*keyVerdict{}, waiting: map[string][]chan struct{}{},
		changes: map[string]string{}, warned: map[string]bool{}}
	k.log = newTransLog(k)
	return k
}

// server is chriscord.app (or wherever this online account lives), or ""
// when lookups are off (no online account unlocked).
func (k *keyCheck) server() string {
	k.c.mu.Lock()
	slug := k.c.linked
	k.c.mu.Unlock()
	if slug == "" {
		return ""
	}
	st, err := readCloudState(slug)
	if err != nil {
		return ""
	}
	return st.Server
}

// noteIdentity is called by pinIdentity every time a member's key is seen.
// old is the key remembered before ("" the first time, or when it hasn't
// changed).
func noteIdentity(domain, user, idHex, old string) {
	if theCloud != nil && theCloud.keys != nil {
		go theCloud.keys.note(domain, user, idHex, old)
	}
}

func (k *keyCheck) note(domain, user, idHex, old string) {
	changed := old != "" && old != idHex
	if changed {
		k.mu.Lock()
		k.changes[domain+"\x00"+user] = idHex
		k.mu.Unlock()
	}
	if k.server() == "" {
		if changed {
			k.warn(domain, user, idHex, keyChangedText(user, domain))
		}
		return
	}
	v := k.verdict(idHex, time.Minute) // on its own goroutine: waiting is fine
	switch {
	case changed:
		msg := keyChangedText(user, domain)
		switch v.Status {
		case "verified":
			msg += fmt.Sprintf(" The new key belongs to chriscord.app user %s.", v.Name)
		case "unlisted":
			msg += " The new key isn't on chriscord.app."
		case "problem":
			msg += " chriscord.app's record for the new key doesn't check out: " + v.Problem
		}
		k.warn(domain, user, idHex, msg)
	case v.Status == "problem":
		k.warn(domain, user, idHex, fmt.Sprintf("Couldn't verify %s on chriscord.app: %s", user, v.Problem))
	}
}

func keyChangedText(user, domain string) string {
	return fmt.Sprintf("%s's key on %s has changed. If they didn't make a new account, someone may be pretending to be them: check with them before trusting it.",
		user, strings.TrimPrefix(strings.TrimPrefix(domain, "https://"), "http://"))
}

func (k *keyCheck) warn(domain, user, idHex, msg string) {
	key := domain + "\x00" + user + "\x00" + idHex
	k.mu.Lock()
	seen := k.warned[key]
	k.warned[key] = true
	k.mu.Unlock()
	if seen {
		return
	}
	log.Printf("identity: WARNING: %s", msg)
	k.c.emit("identity:warning", map[string]string{"user": user, "server": domain, "message": msg})
}

// verdict returns what's known about pub, looking it up (batched with
// whatever else is waiting) if it isn't cached, waiting at most timeout.
func (k *keyCheck) verdict(pub string, timeout time.Duration) keyVerdict {
	pub = strings.ToLower(pub)
	k.mu.Lock()
	if v := k.cache[pub]; v != nil {
		ttl := keyCheckFresh
		if v.Status == "error" {
			ttl = keyCheckRetry
		}
		if time.Since(v.at) < ttl {
			out := *v
			k.mu.Unlock()
			return out
		}
	}
	done := make(chan struct{})
	k.waiting[pub] = append(k.waiting[pub], done)
	if k.timer == nil {
		k.timer = time.AfterFunc(keyCheckBatch, k.flush)
	}
	k.mu.Unlock()
	select {
	case <-done:
	case <-time.After(timeout):
		return keyVerdict{Status: "error", Note: "chriscord.app didn't answer in time"}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if v := k.cache[pub]; v != nil {
		return *v
	}
	return keyVerdict{Status: "error"}
}

// flush looks up everything waiting, in batches.
func (k *keyCheck) flush() {
	k.mu.Lock()
	k.timer = nil
	var keys []string
	for pub := range k.waiting {
		keys = append(keys, pub)
	}
	k.mu.Unlock()
	server := k.server()
	for len(keys) > 0 {
		n := min(len(keys), keyCheckMax)
		batch := keys[:n]
		keys = keys[n:]
		verdicts := map[string]*keyVerdict{}
		if server == "" {
			for _, pub := range batch {
				verdicts[pub] = &keyVerdict{Status: "error", Note: "not signed in to chriscord.app"}
			}
		} else if entries, err := dirLookup(server, batch); err != nil {
			log.Printf("identity: directory lookup failed: %v", err)
			for _, pub := range batch {
				verdicts[pub] = &keyVerdict{Status: "error", Note: "couldn't reach chriscord.app"}
			}
		} else {
			for _, pub := range batch {
				verdicts[pub] = k.judge(server, pub, entries[pub])
			}
		}
		k.mu.Lock()
		for pub, v := range verdicts {
			v.at = time.Now()
			k.cache[pub] = v
			for _, ch := range k.waiting[pub] {
				close(ch)
			}
			delete(k.waiting, pub)
		}
		k.mu.Unlock()
	}
}

func dirLookup(server string, keys []string) (map[string]*dirEntry, error) {
	var resp struct {
		Entries map[string]*dirEntry `json:"entries"`
	}
	if err := cloudCall(server, "POST", "/directory/lookup", "", map[string][]string{"keys": keys}, &resp); err != nil {
		return nil, err
	}
	if resp.Entries == nil {
		resp.Entries = map[string]*dirEntry{}
	}
	return resp.Entries, nil
}

// checkHistory verifies every signed entry in a key's directory history,
// and that the current name is the one the key last claimed. It returns
// a problem, or "".
func checkHistory(pub string, e *dirEntry) string {
	if !strings.EqualFold(e.PublicKey, pub) {
		return "the directory answered about a different key"
	}
	last := ""
	for _, h := range e.History {
		switch h.Event {
		case "register", "rename":
			if !VerifyCloudSig(pub, h.Signature, CloudSigMessage("claim", h.Nonce, pub, h.Username)) {
				return "a name in its history isn't signed by the key"
			}
			last = h.Username
		case "delete":
			if !VerifyCloudSig(pub, h.Signature, CloudSigMessage("delete", h.Nonce, pub)) {
				return "a deletion in its history isn't signed by the key"
			}
			last = ""
		case "expired":
			last = ""
		}
	}
	if e.Online && e.Username != last {
		return "its current name isn't one the key claimed"
	}
	return ""
}

// judge turns a directory entry into a verdict, including the
// remembered-name check.
func (k *keyCheck) judge(server, pub string, e *dirEntry) *keyVerdict {
	// First, the directory's answer has to match its own public log (as
	// this app has checked it): same entries, nothing hidden or added.
	logOK := false
	if err := k.log.sync(server, false); err == nil {
		m := k.log.matches(pub, e)
		if m != "" && k.log.sync(server, true) == nil { // the answer may be newer than this app's copy
			m = k.log.matches(pub, e)
		}
		if m != "" {
			return &keyVerdict{Status: "problem", Problem: "chriscord.app " + m + "."}
		}
		logOK = true
	} else if p := k.log.Problem(); p != "" {
		return &keyVerdict{Status: "problem", Problem: "chriscord.app's public log doesn't check out: " + p}
	} // else: couldn't reach the log; the checks below still apply
	if e == nil {
		return &keyVerdict{Status: "unlisted"}
	}
	if p := checkHistory(pub, e); p != "" {
		return &keyVerdict{Status: "problem", Problem: p}
	}
	if !e.Online {
		v := &keyVerdict{Status: "unlisted"}
		for i := len(e.History) - 1; i >= 0; i-- {
			if n := e.History[i].Username; n != "" {
				v.Name, v.Note = n, "used to be chriscord.app user "+n
				break
			}
		}
		return v
	}
	v := &keyVerdict{Status: "verified", Name: e.Username}
	if !e.EmailVerified {
		v.Note = "hasn't confirmed their email yet"
	}
	nameKey := CloudUsernameKey(e.Username)
	k.namesMu.Lock()
	defer k.namesMu.Unlock()
	names := loadDirNames()
	old, seen := names[nameKey]
	switch {
	case !seen || old == pub:
		if !seen {
			names[nameKey] = pub
			saveDirNames(names)
		}
	default:
		// The name was on another key before. Fine only if that key gave
		// it up itself (or its unconfirmed account lapsed): from the log
		// when it's been checked, otherwise by asking.
		if logOK {
			recs := k.log.recordsFor(old)
			if n := len(recs); n > 0 && (recs[n-1].Event == "delete" || recs[n-1].Event == "expired") {
				names[nameKey] = pub
				saveDirNames(names)
				v.Note = "this name belonged to someone else before (they deleted that account)"
				return v
			}
		} else if prev, err := dirLookup(server, []string{old}); err == nil {
			if pe := prev[old]; pe != nil && !pe.Online && checkHistory(old, pe) == "" {
				names[nameKey] = pub
				saveDirNames(names)
				v.Note = "this name belonged to someone else before (they deleted that account)"
				return v
			}
		} else {
			return &keyVerdict{Status: "error", Note: "couldn't reach chriscord.app"}
		}
		return &keyVerdict{Status: "problem", Name: e.Username,
			Problem: fmt.Sprintf("chriscord.app now lists %s under a different key than before. Keys never change, so this may be someone impersonating them.", e.Username)}
	}
	return v
}

func dirNamesPath() string { return filepath.Join(chriscordConfigDir(), dirNamesFile) }

func loadDirNames() map[string]string {
	names := map[string]string{}
	if data, err := os.ReadFile(dirNamesPath()); err == nil {
		json.Unmarshal(data, &names)
	}
	return names
}

func saveDirNames(names map[string]string) {
	data, err := json.MarshalIndent(names, "", "  ")
	if err != nil {
		return
	}
	os.MkdirAll(chriscordConfigDir(), 0700)
	os.WriteFile(dirNamesPath(), data, 0600)
}

// pinnedIdentity is the key remembered for user on domain, or "".
func pinnedIdentity(domain, user string) string {
	pinsMu.Lock()
	defer pinsMu.Unlock()
	pins := map[string]map[string]string{}
	if data, err := os.ReadFile(filepath.Join(chriscordConfigDir(), e2eePinsFile)); err == nil {
		json.Unmarshal(data, &pins)
	}
	return pins[domain][user]
}

// ── For the profile card ───────────────────────────────────────────────────

// MemberIdentityView is the "who is this, really" line on a profile card.
type MemberIdentityView struct {
	Known       bool   `json:"known"` // this app has seen their key
	Self        bool   `json:"self"`
	Fingerprint string `json:"fingerprint"` // their key, grouped for reading out loud
	Directory   string `json:"directory"`   // verified | unlisted | problem | error | off
	Name        string `json:"name"`        // chriscord.app username
	Note        string `json:"note"`
	Problem     string `json:"problem"`
	Changed     bool   `json:"changed"` // their key changed on this server since this app started
}

func groupedFingerprint(pubHex string) string {
	if len(pubHex) < 16 {
		return pubHex
	}
	f := pubHex[:16]
	return f[0:4] + " " + f[4:8] + " " + f[8:12] + " " + f[12:16]
}

// MemberIdentity describes the key behind a member of the server you're on.
func (a *App) MemberIdentity(username string) (*MemberIdentityView, error) {
	a.mu.Lock()
	domain := a.domain
	var self string
	if a.account != nil && username == a.username {
		self = hex.EncodeToString(a.account.PublicKey)
	}
	a.mu.Unlock()
	if domain == "" {
		return nil, errors.New("not connected")
	}
	v := &MemberIdentityView{Self: self != ""}
	pub := self
	if pub == "" {
		pub = pinnedIdentity(domain, username)
	}
	if pub == "" {
		return v, nil
	}
	v.Known, v.Fingerprint = true, groupedFingerprint(pub)
	k := a.cloud.keys
	k.mu.Lock()
	v.Changed = k.changes[domain+"\x00"+username] == pub
	k.mu.Unlock()
	if k.server() == "" {
		v.Directory = "off"
		return v, nil
	}
	r := k.verdict(pub, keyCheckWaitUI)
	v.Directory, v.Name, v.Note, v.Problem = r.Status, r.Name, r.Note, r.Problem
	return v, nil
}

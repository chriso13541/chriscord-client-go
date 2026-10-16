package main

// translog.go — this app's own copy of chriscord.app's public log (key
// transparency).
//
// The directory keeps every register, rename, delete and expiry in one
// append-only log, each entry hash-chained to the one before, and signs
// the newest hash (the "head") with its log key (cloudproto.go). This app:
//
//   - downloads new entries as they appear, and checks each one chains
//     onto what it already has and carries its key's own signature;
//   - remembers the log key the first time (or uses logKeyPinned) and
//     treats any other key as a warning;
//   - never accepts a head older than one it has seen, or a different
//     hash for an entry it already has;
//   - checks what the directory says about a key against its own copy, so
//     the directory can't answer one thing and log another;
//   - shares its newest signed head inside its encrypted status on chat
//     servers, and checks the heads other people's apps share. Two signed
//     heads that disagree are proof the directory showed different people
//     different histories.
//
// Only while an online account is unlocked, like the other directory
// checks (keycheck.go).

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// logKeyPinned, when filled in before building, is the only log key this
// app accepts (chriscord-accounts prints its key when it starts). Empty:
// the first key seen is remembered and any other is a warning.
const logKeyPinned = ""

const (
	dirLogFile   = "chriscord_directory_log.json"
	logSyncEvery = 30 * time.Second
)

type logRec struct {
	Seq   int64  `json:"s"`
	Pub   string `json:"k"`
	Event string `json:"e"`
	Name  string `json:"n"`
	Hash  string `json:"h"`
}

type localLog struct {
	Server  string   `json:"server"`
	Key     string   `json:"key"` // the log key, remembered the first time
	Head    LogHead  `json:"head"`
	Entries []logRec `json:"entries"`
}

type transLog struct {
	k *keyCheck

	mu       sync.Mutex
	l        *localLog
	lastSync time.Time
	problem  string          // why the log can't be trusted (sticky for this run)
	compared map[string]bool // other people's heads already checked
}

func newTransLog(k *keyCheck) *transLog { return &transLog{k: k, compared: map[string]bool{}} }

func dirLogPath() string { return filepath.Join(chriscordConfigDir(), dirLogFile) }

// load reads this app's copy for server (a new, empty one if it's for
// another server, or there isn't one). Caller holds t.mu.
func (t *transLog) load(server string) *localLog {
	if t.l != nil && t.l.Server == server {
		return t.l
	}
	l := &localLog{}
	if data, err := os.ReadFile(dirLogPath()); err == nil {
		json.Unmarshal(data, l)
	}
	if l.Server != server {
		l = &localLog{Server: server}
	}
	t.l = l
	return l
}

func (t *transLog) save() {
	data, err := json.Marshal(t.l)
	if err != nil {
		return
	}
	os.MkdirAll(chriscordConfigDir(), 0700)
	tmp := dirLogPath() + ".tmp"
	if os.WriteFile(tmp, data, 0600) == nil {
		os.Rename(tmp, dirLogPath())
	}
}

// fail records that the log can't be trusted, and says so once.
func (t *transLog) fail(msg string) error {
	t.mu.Lock()
	first := t.problem == ""
	if first {
		t.problem = msg
	}
	t.mu.Unlock()
	if first {
		log.Printf("identity: WARNING: directory log: %s", msg)
		t.k.c.emit("identity:warning", map[string]string{"user": "", "server": "chriscord.app",
			"message": "chriscord.app's public log doesn't check out: " + msg + " Until this is sorted out, don't trust its names."})
	}
	return errors.New(msg)
}

// Problem is why the log can't be trusted, or "".
func (t *transLog) Problem() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.problem
}

// Seq is how far this app's verified copy goes.
func (t *transLog) Seq() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.l == nil {
		return 0
	}
	return t.l.Head.Seq
}

// sync brings this app's copy up to the directory's newest head, checking
// everything on the way. force skips the "synced recently" shortcut.
func (t *transLog) sync(server string, force bool) error {
	t.mu.Lock()
	if t.problem != "" {
		p := t.problem
		t.mu.Unlock()
		return errors.New(p)
	}
	l := t.load(server)
	if !force && time.Since(t.lastSync) < logSyncEvery && l.Head.Seq > 0 {
		t.mu.Unlock()
		return nil
	}
	have, haveHash, key := l.Head.Seq, l.Head.Hash, l.Key
	t.mu.Unlock()
	if have == 0 {
		haveHash = LogGenesis // nothing yet: the log starts from here
	}

	var head LogHead
	if err := cloudCall(server, "GET", "/log/head", "", nil, &head); err != nil {
		return err
	}
	if !head.Valid() {
		return t.fail("its newest entry isn't signed properly.")
	}
	want := key
	if logKeyPinned != "" {
		want = logKeyPinned
	}
	if want != "" && !strings.EqualFold(head.Key, want) {
		return t.fail("it's signed with a different key than before. If you run chriscord.app and didn't reset its log key, someone else may be answering for it.")
	}
	switch {
	case head.Seq < have:
		return t.fail(fmt.Sprintf("it went back from entry %d to %d.", have, head.Seq))
	case head.Seq == have && !strings.EqualFold(head.Hash, haveHash):
		return t.fail(fmt.Sprintf("entry %d isn't the one it was before.", have))
	}

	// New entries, chained onto what's here.
	running := haveHash
	var added []logRec
	for last := have; last < head.Seq; {
		var page struct {
			Entries []LogEntry `json:"entries"`
		}
		if err := cloudCall(server, "GET", "/log?after="+strconv.FormatInt(last, 10), "", nil, &page); err != nil {
			return err
		}
		if len(page.Entries) == 0 {
			return t.fail(fmt.Sprintf("it says it has %d entries but stops at %d.", head.Seq, last))
		}
		for _, e := range page.Entries {
			if e.Seq > head.Seq {
				break
			}
			switch {
			case e.Seq <= last:
				return t.fail(fmt.Sprintf("entry %d is out of order.", e.Seq))
			case !strings.EqualFold(e.Prev, running) || LogEntryHash(e) != strings.ToLower(e.Hash):
				return t.fail(fmt.Sprintf("entry %d doesn't chain onto the one before it.", e.Seq))
			case !LogEntrySigned(e):
				return t.fail(fmt.Sprintf("entry %d (%s %q) isn't signed by its own key.", e.Seq, e.Event, e.Username))
			}
			running, last = strings.ToLower(e.Hash), e.Seq
			added = append(added, logRec{Seq: e.Seq, Pub: strings.ToLower(e.PublicKey), Event: e.Event, Name: e.Username, Hash: running})
		}
	}
	if !strings.EqualFold(running, head.Hash) {
		return t.fail(fmt.Sprintf("its entries don't add up to the head it signed (entry %d).", head.Seq))
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	l = t.load(server)
	if l.Head.Seq != have { // another sync got there first
		return nil
	}
	if l.Key == "" {
		l.Key = strings.ToLower(head.Key)
		log.Printf("identity: remembering chriscord.app's log key %s", l.Key)
	}
	l.Entries = append(l.Entries, added...)
	l.Head = head
	t.lastSync = time.Now()
	t.save()
	return nil
}

// recordsFor is everything the log says about a key, in order.
func (t *transLog) recordsFor(pub string) []logRec {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []logRec
	if t.l == nil {
		return out
	}
	pub = strings.ToLower(pub)
	for _, r := range t.l.Entries {
		if r.Pub == pub {
			out = append(out, r)
		}
	}
	return out
}

// owners replays the log: who holds each name now, and each key's name.
func (t *transLog) owners() (byName map[string]string, byKey map[string]string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	byName, byKey = map[string]string{}, map[string]string{}
	if t.l == nil {
		return
	}
	for _, r := range t.l.Entries {
		if old, ok := byKey[r.Pub]; ok && byName[CloudUsernameKey(old)] == r.Pub {
			delete(byName, CloudUsernameKey(old))
		}
		switch r.Event {
		case "register", "rename":
			byKey[r.Pub] = r.Name
			byName[CloudUsernameKey(r.Name)] = r.Pub
		default:
			delete(byKey, r.Pub)
		}
	}
	return
}

// matches checks the directory's answer about a key against the log: the
// same entries, nothing left out, nothing added.
func (t *transLog) matches(pub string, e *dirEntry) string {
	recs := t.recordsFor(pub)
	var hist []dirHistory
	if e != nil {
		hist = e.History
	}
	if len(hist) != len(recs) {
		return fmt.Sprintf("its answer about this key has %d entries, but its public log has %d", len(hist), len(recs))
	}
	for i, h := range hist {
		r := recs[i]
		le := LogEntry{Seq: h.Seq, PublicKey: pub, Event: h.Event, Username: h.Username, Nonce: h.Nonce,
			Signature: h.Signature, At: h.At, Prev: h.Prev}
		if h.Seq != r.Seq || h.Event != r.Event || h.Username != r.Name || LogEntryHash(le) != r.Hash {
			return fmt.Sprintf("its answer about this key doesn't match entry %d of its public log", r.Seq)
		}
	}
	return ""
}

// ── Comparing heads with other people's apps ───────────────────────────────

// headToken is this app's newest verified head, for its status ("" if
// there isn't one): seq.hash.time.key.signature.
func (t *transLog) headToken() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.l == nil || t.l.Head.Seq == 0 || t.problem != "" {
		return ""
	}
	h := t.l.Head
	return fmt.Sprintf("%d.%s.%d.%s.%s", h.Seq, h.Hash, h.Time, h.Key, h.Signature)
}

func parseHeadToken(s string) (LogHead, bool) {
	p := strings.Split(s, ".")
	if len(p) != 5 {
		return LogHead{}, false
	}
	seq, err1 := strconv.ParseInt(p[0], 10, 64)
	tm, err2 := strconv.ParseInt(p[2], 10, 64)
	if err1 != nil || err2 != nil {
		return LogHead{}, false
	}
	h := LogHead{Seq: seq, Hash: strings.ToLower(p[1]), Time: tm, Key: strings.ToLower(p[3]), Signature: p[4]}
	return h, h.Valid()
}

// compareHead checks a head someone else's app saw against this app's
// copy of the log.
func (t *transLog) compareHead(from, token string) {
	h, ok := parseHeadToken(token)
	if !ok {
		return // not a real signed head: nothing to learn from it
	}
	server := t.k.server()
	if server == "" {
		return
	}
	cacheKey := fmt.Sprintf("%d.%s", h.Seq, h.Hash)
	t.mu.Lock()
	l := t.load(server)
	if t.compared[cacheKey] || l.Key == "" || !strings.EqualFold(h.Key, l.Key) {
		// Seen it, nothing to compare with yet, or a different directory
		// altogether (someone using their own copy of the service).
		t.mu.Unlock()
		return
	}
	mine := l.Head.Seq
	t.mu.Unlock()

	if h.Seq > mine {
		if err := t.sync(server, true); err != nil {
			return
		}
		t.mu.Lock()
		mine = t.l.Head.Seq
		t.mu.Unlock()
		if h.Seq > mine {
			t.fail(fmt.Sprintf("%s's app was shown entry %d, but this app is shown only %d.", from, h.Seq, mine))
			return
		}
	}
	t.mu.Lock()
	var at string
	for _, r := range t.l.Entries {
		if r.Seq == h.Seq {
			at = r.Hash
			break
		}
	}
	t.compared[cacheKey] = true
	t.mu.Unlock()
	if h.Seq > 0 && at != h.Hash {
		t.fail(fmt.Sprintf("%s's app was shown a different entry %d than this app was: it's showing people different histories.", from, h.Seq))
	}
}

// noteLogHead is called with each head found in someone's status.
func noteLogHead(from, token string) {
	if token != "" && theCloud != nil && theCloud.keys != nil {
		go theCloud.keys.log.compareHead(from, token)
	}
}

// ownLogHead is this app's head to share in its status ("" when there's
// none, or directory checks are off).
func ownLogHead() string {
	if theCloud == nil || theCloud.keys == nil || theCloud.keys.server() == "" {
		return ""
	}
	return theCloud.keys.log.headToken()
}

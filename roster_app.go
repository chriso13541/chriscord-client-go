package main

// roster_app.go — the signed member list (roster.go) in the app: fetched
// from the server, checked, remembered per server, and used to decide who
// gets this app's channel keys (textcrypt.go) and call keys (e2ee.go).
//
// The states, for a server:
//
//	loading   not fetched and checked yet: keys go to nobody
//	none      the server has no member list (and never had one, as far as
//	          this app knows): keys go to whoever the server says, as before
//	active    a list that checks out: keys only go to who it allows
//	replaced  this app saw a list here before, and now there's none or a
//	          different one (started over — or a server trying to drop it):
//	          keys go to nobody until you accept the new one
//
// "Seen before" is remembered (roster_pins.json): the list's id, how far
// this app had got, and the channels that need a new key because someone
// lost access (so a restart in between doesn't forget them).

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

const rosterPinsFile = "chriscord_roster_pins.json"

const (
	rosterLoading  = "loading"
	rosterNone     = "none"
	rosterActive   = "active"
	rosterReplaced = "replaced"
)

// rosterPin is what this app remembers about a server's list.
type rosterPin struct {
	ID      string          `json:"id"`
	Applied int             `json:"applied"`        // entries this app had worked through
	Head    string          `json:"head,omitempty"` // the list's head after them (first 16 characters)
	Rotate  map[string]bool `json:"rotate,omitempty"`
	Joins   []string        `json:"joins,omitempty"` // invite secrets waiting to be used (base64url)
}

type rosterProposal struct {
	Entry     RosterEntry
	Rejects   []RosterSig // approvers turning it down (rosterRejectMessage)
	CreatedAt string
}

type rosterClient struct {
	app *App

	mu        sync.Mutex
	scope     string // domain|identity
	domain    string
	me        string // this account's identity key
	entries   []RosterEntry
	lastSeq   int64
	r         *Roster
	proposals []rosterProposal
	state     string
	problem   string
	heads     map[string]bool // every head (first 16 characters) the list has had
	pin       *rosterPin
	forks     map[string]string // member → the unknown head their message carried (a different list?)
	refreshT  *time.Timer
	loading   bool
	again     bool
	idle      *sync.Cond // signalled when a refresh (and any it owed) has finished
}

func newRosterClient(a *App) *rosterClient {
	rc := &rosterClient{app: a, state: rosterNone}
	rc.idle = sync.NewCond(&rc.mu)
	return rc
}

func (a *App) rosterPinPath() string { return filepath.Join(chriscordConfigDir(), rosterPinsFile) }

var rosterPinsMu sync.Mutex

// readRosterPinsLocked reads every server's pin. A file that's there but
// can't be read (from before writes were atomic, or damaged) is moved
// aside rather than taken as empty: taken as empty, the next save wrote
// over it and forgot every server's list, and each one was then accepted
// as if seen for the first time.
func readRosterPinsLocked(path string) map[string]*rosterPin {
	all := map[string]*rosterPin{}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("roster: reading %s: %v", path, err)
		}
		return all
	}
	if err := json.Unmarshal(data, &all); err != nil {
		to := quarantine(path)
		log.Printf("roster: WARNING: %s is damaged (%v) — moved to %s; member lists here will be checked from scratch", path, err, to)
		return map[string]*rosterPin{}
	}
	for k, p := range all {
		if p == nil {
			delete(all, k)
		}
	}
	return all
}

func loadRosterPin(path, key string) *rosterPin {
	rosterPinsMu.Lock()
	defer rosterPinsMu.Unlock()
	p := readRosterPinsLocked(path)[key]
	if p == nil {
		return nil
	}
	c := *p // its own copy: callers change it under rc.mu
	return &c
}

// saveRosterPin writes key's pin (nil: forgets it), leaving the others as
// they are. The whole file is replaced in one go (writeFileAtomic).
func saveRosterPin(path, key string, p *rosterPin) {
	rosterPinsMu.Lock()
	defer rosterPinsMu.Unlock()
	all := readRosterPinsLocked(path)
	if p == nil {
		delete(all, key)
	} else {
		all[key] = p
	}
	data, err := json.MarshalIndent(all, "", "  ")
	if err == nil {
		err = writeFileAtomic(path, data, 0600)
	}
	if err != nil {
		log.Printf("roster: WARNING: couldn't save what this app knows of the member list (%s): %v", path, err)
	}
}

// begin: connected to a server — load what's remembered, then the list.
func (rc *rosterClient) begin() {
	id, ok := rc.app.text.identity()
	if !ok {
		return
	}
	scope := id.domain + "|" + id.idHex
	rc.mu.Lock()
	if rc.scope != scope {
		rc.scope, rc.domain, rc.me = scope, id.domain, id.idHex
		rc.entries, rc.lastSeq, rc.r, rc.proposals = nil, 0, nil, nil
		rc.heads, rc.forks = map[string]bool{}, map[string]string{}
		rc.pin = loadRosterPin(rc.app.rosterPinPath(), scope)
		// Until the list is fetched and checked, nobody gets keys — not
		// even on a server that (says it) has none.
		rc.state, rc.problem = rosterLoading, "checking the member list"
		_ = rc.pin
	}
	rc.mu.Unlock()
	rc.refresh()
}

// changed: the server says the list changed — fetch it soon.
func (rc *rosterClient) changed() {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.refreshT != nil {
		rc.refreshT.Stop()
	}
	rc.refreshT = time.AfterFunc(300*time.Millisecond, rc.refresh)
}

type rosterResp struct {
	ID        string        `json:"id"`
	Entries   []RosterEntry `json:"entries"`
	More      bool          `json:"more"`
	Proposals []struct {
		Body      string      `json:"body"`
		Sigs      []RosterSig `json:"sigs"`
		Rejects   []RosterSig `json:"rejects"`
		CreatedAt string      `json:"created_at"`
	} `json:"proposals"`
}

// refresh fetches what's new in the list and works it in. If a fetch is
// already running, it fetches once more when that's done (a change may
// have come after it started), and this returns straight away; waitIdle
// waits for all of it.
func (rc *rosterClient) refresh() {
	rc.mu.Lock()
	if rc.idle == nil {
		rc.idle = sync.NewCond(&rc.mu)
	}
	if rc.loading {
		rc.again = true
		rc.mu.Unlock()
		return
	}
	rc.loading = true
	rc.mu.Unlock()
	for {
		rc.refreshOnce()
		rc.mu.Lock()
		if !rc.again {
			rc.loading = false
			rc.idle.Broadcast()
			rc.mu.Unlock()
			return
		}
		rc.again = false
		rc.mu.Unlock()
	}
}

// waitIdle fetches the list and waits until it's done, including a fetch
// that was already running (and any owed after it).
func (rc *rosterClient) waitIdle() {
	rc.refresh()
	rc.mu.Lock()
	for rc.loading && rc.idle != nil {
		rc.idle.Wait()
	}
	rc.mu.Unlock()
}

// refreshOnce is one fetch: everything new, worked in together or not at
// all (a failed fetch changes nothing).
func (rc *rosterClient) refreshOnce() {
	rc.mu.Lock()
	scope := rc.scope
	rc.mu.Unlock()
	if scope == "" {
		return
	}
	var all []RosterEntry
	var proposals []rosterProposal
	rc.mu.Lock()
	after := rc.lastSeq
	known := rc.r
	rc.mu.Unlock()
	serverID := ""
	for page := 0; page < 100; page++ {
		var resp rosterResp
		if err := rc.app.doJSON("GET", fmt.Sprintf("/api/roster?after=%d", after), nil, &resp); err != nil {
			var h *httpError
			if errors.As(err, &h) && h.code == 404 {
				resp = rosterResp{} // a server from before member lists
				rc.mu.Lock()
				rc.problem = "this server is too old for signed member lists — it needs updating before anyone's app shares keys there"
				rc.mu.Unlock()
			} else {
				log.Printf("roster: fetching: %v", err)
				return
			}
		}
		serverID = resp.ID
		if known != nil && serverID != known.ID {
			// Started over (or a different list): from the beginning.
			known, after, all = nil, 0, nil
			rc.mu.Lock()
			rc.entries, rc.lastSeq, rc.r = nil, 0, nil
			rc.mu.Unlock()
			page = -1
			continue
		}
		all = append(all, resp.Entries...)
		for _, p := range resp.Proposals {
			proposals = append(proposals, rosterProposal{Entry: RosterEntry{Body: p.Body, Sigs: p.Sigs}, Rejects: p.Rejects, CreatedAt: p.CreatedAt})
		}
		if len(resp.Entries) > 0 {
			after = resp.Entries[len(resp.Entries)-1].Seq
		}
		if !resp.More {
			break
		}
		proposals = nil
	}

	rc.mu.Lock()
	if rc.scope != scope {
		rc.mu.Unlock()
		return
	}
	rc.proposals = proposals
	rc.integrateLocked(all, after)
	pending := rc.pendingJoinsLocked()
	rc.mu.Unlock()
	for _, seed := range pending {
		rc.tryJoin(seed)
	}
	rc.app.text.changed() // who gets keys may have changed: share with newcomers, etc.
	if rc.app.e2ee != nil {
		rc.app.e2ee.rosterChanged() // and who may hear this app's call (e2ee.go)
	}
	rc.emit()
}

// integrateLocked works newly fetched entries into the list.
func (rc *rosterClient) integrateLocked(fresh []RosterEntry, lastSeq int64) {
	rc.entries = append(rc.entries, fresh...)
	rc.lastSeq = lastSeq
	path := rc.app.rosterPinPath()
	if len(rc.entries) == 0 {
		rc.r = nil
		if rc.pin != nil && rc.pin.ID != "" {
			rc.state = rosterReplaced
			rc.problem = "this server's member list is gone — it was started over, or the server is hiding it"
		} else if rc.state != rosterNone || !strings.Contains(rc.problem, "too old") {
			rc.state, rc.problem = rosterNone, ""
		}
		return
	}
	if rc.r == nil {
		// From scratch. If this app had seen this list before, entries past
		// where it got to are new to it: worked in one by one, so it
		// notices anyone losing access.
		k := len(rc.entries)
		if rc.pin != nil && rc.pin.Applied > 0 && rc.pin.Applied < k {
			k = rc.pin.Applied
		}
		r, err := BuildRoster(rc.entries[:k])
		if err != nil {
			rc.state, rc.problem = rosterReplaced, "the member list doesn't check out: "+err.Error()
			return
		}
		if rc.pin != nil && rc.pin.ID == r.ID && rc.pin.Applied > 0 {
			// Same list: it has to hold at least what this app already
			// worked through, the same way. Fewer entries means the server
			// lost some (a restored backup) or is hiding them — a removal,
			// say; a different head, that they were changed.
			why := ""
			switch {
			case rc.pin.Applied > len(rc.entries):
				why = fmt.Sprintf("is missing changes this app has already seen (it has %d, this app had %d) — it was rolled back, or is hiding some", len(rc.entries), rc.pin.Applied)
			case rc.pin.Head != "" && headPrefix(r.Head) != rc.pin.Head:
				why = "isn't the one this app saw before: changes it had seen were altered"
			}
			if why != "" {
				rc.r, rc.entries, rc.lastSeq = nil, nil, 0
				rc.state, rc.problem = rosterReplaced, "the server's member list "+why
				log.Printf("roster: WARNING: %s", rc.problem)
				return
			}
		}
		rc.r = r
		// Every head it has had, not just the newest: messages sent before
		// the last change carry an earlier one.
		rc.heads = map[string]bool{headPrefix(r.Head): true}
		for h := range r.Heads {
			rc.heads[h] = true
		}
		rc.applyLocked(rc.entries[k:], rc.pin != nil && rc.pin.ID == r.ID)
	} else {
		rc.applyLocked(fresh, true)
	}
	switch {
	case rc.pin == nil || rc.pin.ID == "":
		// The first time this app sees a list here: remember it.
		joins := []string(nil)
		if rc.pin != nil {
			joins = rc.pin.Joins // invites waiting to be used (RosterJoin)
		}
		rc.pin = &rosterPin{ID: rc.r.ID, Applied: rc.r.Applied + len(rc.r.Rejected), Head: headPrefix(rc.r.Head), Joins: joins}
		rc.state, rc.problem = rosterActive, ""
	case rc.pin.ID != rc.r.ID:
		rc.state = rosterReplaced
		rc.problem = "this server's member list was started over — check with the owner before accepting the new one"
		return
	default:
		rc.state, rc.problem = rosterActive, ""
		rc.pin.Applied = rc.r.Applied + len(rc.r.Rejected)
		rc.pin.Head = headPrefix(rc.r.Head)
	}
	// Someone whose message carried a head this app didn't have yet, which
	// it has now: they weren't on a different list after all.
	for u, h := range rc.forks {
		if rc.heads[h] {
			delete(rc.forks, u)
		}
	}
	saveRosterPin(path, rc.scope, rc.pin)
}

// applyLocked applies entries one by one; with notice, a removal marks the
// channels it affects as needing a new key.
func (rc *rosterClient) applyLocked(es []RosterEntry, notice bool) {
	r := rc.r
	for _, e := range es {
		var losing []string
		if notice {
			losing = rc.losingAccess(e)
		}
		if err := r.Apply(e); err != nil {
			r.Rejected = append(r.Rejected, RosterRejected{Index: r.Applied + len(r.Rejected), Why: err.Error()})
			continue
		}
		rc.heads[headPrefix(r.Head)] = true
		if len(losing) > 0 && rc.pin != nil {
			if rc.pin.Rotate == nil {
				rc.pin.Rotate = map[string]bool{}
			}
			for _, b := range losing {
				rc.pin.Rotate[b] = true
			}
		}
	}
}

// losingAccess: the channels whose readers this entry would take someone
// away from (worked out before it's applied).
func (rc *rosterClient) losingAccess(e RosterEntry) []string {
	r := rc.r
	op, err := e.op()
	if err != nil {
		return nil
	}
	var out []string
	switch op.Op {
	case opRemove:
		for _, b := range append(r.Boards(), presenceKeyID) {
			if b == presenceKeyID || r.CanRead(op.Key, b) {
				out = append(out, b)
			}
		}
	case opAccess:
		all, _ := r.Access(op.Board)
		if len(op.Remove) > 0 || (op.All != nil && !*op.All && all) {
			out = append(out, op.Board)
		}
	}
	return out
}

func headPrefix(h string) string {
	if len(h) > 16 {
		return h[:16]
	}
	return h
}

// ── What the rest of the app asks ────────────────────────────────────────────

// rosterAllows: whether key may be given board's keys (presence's too).
// Only ever by the signed member list: a server without one gets nobody's
// keys (the list is required — its owner's app sets it up).
func (a *App) rosterAllows(key, board string) bool {
	if a == nil || a.roster == nil {
		return false
	}
	rc := a.roster
	rc.mu.Lock()
	defer rc.mu.Unlock()
	switch rc.state {
	case rosterActive:
		key = strings.ToLower(key)
		if board == presenceKeyID {
			m, ok := rc.r.Members[key]
			return ok && m.Kind != kindPlugin
		}
		return rc.r.CanRead(key, board)
	}
	return false
}

// rosterAllowsCall: whether key may be given this app's call keys in
// board — a member (never a bot or plugin: they may play into a call, but
// never hear it) the list lets into that channel.
func (a *App) rosterAllowsCall(key, board string) bool {
	if a == nil || a.roster == nil {
		return false
	}
	rc := a.roster
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.state != rosterActive {
		return false
	}
	key = strings.ToLower(key)
	m, ok := rc.r.Members[key]
	return ok && m.Kind == kindMember && rc.r.CanRead(key, board)
}

// rosterKindOf: what the signed member list says key is, and its state.
func (a *App) rosterKindOf(key string) (kind, state string) {
	if a.roster == nil {
		return "", rosterNone
	}
	rc := a.roster
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.state == rosterActive && rc.r != nil {
		if m, ok := rc.r.Members[strings.ToLower(key)]; ok {
			kind = m.Kind
		}
	}
	return kind, rc.state
}

// rosterKnew: whether key has ever been on the signed member list (a
// member who has since left wrote their old messages legitimately).
func (a *App) rosterKnew(key string) bool {
	if a == nil || a.roster == nil {
		return false
	}
	rc := a.roster
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.r == nil {
		return false
	}
	key = strings.ToLower(key)
	if _, ok := rc.r.Members[key]; ok {
		return true
	}
	for _, ch := range rc.r.Changes {
		if ch.Op.Key == key {
			return true
		}
		for _, m := range ch.Op.Members {
			if m.Key == key {
				return true
			}
		}
	}
	return false
}

// rosterSignedSince: whether a message sent at createdAt (RFC 3339) came
// after this server's signed member list started — when every app that
// could get keys signs its messages (allowing a few minutes either way).
func (a *App) rosterSignedSince(createdAt string) bool {
	if a == nil || a.roster == nil {
		return false
	}
	rc := a.roster
	rc.mu.Lock()
	var start int64
	if rc.state == rosterActive && rc.r != nil {
		start = rc.r.StartedAt
	}
	rc.mu.Unlock()
	if start == 0 {
		return false
	}
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		t, err = time.Parse("2006-01-02 15:04:05", createdAt)
	}
	return err == nil && t.Unix() > start+10*60
}

// rosterVouchesAutomated: whether domain's signed member list (this app's
// current server, once checked) has key as a bot or plugin. Waits a little
// while the list is still being checked.
func (a *App) rosterVouchesAutomated(domain, key string) bool {
	if a == nil || a.roster == nil {
		return false
	}
	rc := a.roster
	for i := 0; ; i++ {
		rc.mu.Lock()
		state, same := rc.state, rc.domain == domain
		rc.mu.Unlock()
		if !same || state != rosterLoading || i >= 40 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.domain != domain || rc.state != rosterActive || rc.r == nil {
		return false
	}
	m, ok := rc.r.Members[strings.ToLower(key)]
	return ok && (m.Kind == kindBot || m.Kind == kindPlugin)
}

// rosterIsAutomated: whether the list has key as a bot or plugin.
func (a *App) rosterIsAutomated(key string) bool {
	k, _ := a.rosterKindOf(key)
	return k == kindBot || k == kindPlugin
}

// rosterWhyNoKeys: why this app won't share keys here yet ("" if it will).
func (a *App) rosterWhyNoKeys() string {
	if a == nil || a.roster == nil {
		return "still checking this server's signed member list"
	}
	rc := a.roster
	rc.mu.Lock()
	defer rc.mu.Unlock()
	switch rc.state {
	case rosterActive:
		return ""
	case rosterNone:
		if rc.problem != "" {
			return rc.problem
		}
		return "this server doesn't have its signed member list yet — nobody's app shares keys until the owner sets it up"
	case rosterLoading:
		return "still checking this server's signed member list"
	}
	return rc.problem
}

// rosterNeedsNewKey: someone lost access to board since its newest key.
func (a *App) rosterNeedsNewKey(board string) bool {
	if a == nil || a.roster == nil {
		return false
	}
	rc := a.roster
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.pin != nil && rc.pin.Rotate[board]
}

// rosterMadeNewKey: board has a new key now.
func (a *App) rosterMadeNewKey(board string) {
	if a == nil || a.roster == nil {
		return
	}
	rc := a.roster
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.pin != nil && rc.pin.Rotate[board] {
		delete(rc.pin.Rotate, board)
		saveRosterPin(rc.app.rosterPinPath(), rc.scope, rc.pin)
	}
}

// rosterHead: the list's head as messages carry it ("" without a list).
func (a *App) rosterHead() string {
	if a == nil || a.roster == nil {
		return ""
	}
	rc := a.roster
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.state != rosterActive {
		return ""
	}
	return headPrefix(rc.r.Head)
}

// rosterNoteHead: a checked message from user, sent at createdAt, says
// they saw the list at head. One this app has never had: fetch the list
// (waiting for a fetch already running — a message often arrives with the
// change it was sent after); still unknown after that, they're being shown
// a different list — warn. Messages from before this list started carry
// an earlier list's heads (one that was started over), so they don't count.
func (a *App) rosterNoteHead(user, head, createdAt string) {
	if a == nil || a.roster == nil || head == "" || user == "" {
		return
	}
	rc := a.roster
	rc.mu.Lock()
	if rc.state != rosterActive || rc.r == nil || rc.heads[head] || !sentDuring(rc.r, createdAt) {
		rc.mu.Unlock()
		return
	}
	rc.mu.Unlock()
	go func() {
		rc.waitIdle()
		rc.mu.Lock()
		unknown := rc.state == rosterActive && !rc.heads[head]
		fresh := unknown && rc.forks[user] == ""
		if unknown {
			rc.forks[user] = head
		}
		rc.mu.Unlock()
		if fresh {
			log.Printf("roster: WARNING: %s's message was sent with a member list this app has never seen", user)
			rc.emit()
		}
	}()
}

// sentDuring: whether a message sent at createdAt (RFC 3339, the server's
// clock) came after r was started (the owner's clock), allowing a couple
// of minutes either way. Unknown times count as during.
func sentDuring(r *Roster, createdAt string) bool {
	if r.StartedAt == 0 || createdAt == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		t, err = time.Parse("2006-01-02 15:04:05", createdAt)
	}
	return err != nil || t.Unix() > r.StartedAt+2*60
}

func (rc *rosterClient) emit() {
	if rc.app.ctx != nil {
		wailsruntime.EventsEmit(rc.app.ctx, "roster:changed")
	}
}

// ── For the page ─────────────────────────────────────────────────────────────

type rosterMemberInfo struct {
	Key     string   `json:"key"`
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	Manager bool     `json:"manager"`
	Owner   bool     `json:"owner"`
	Boards  []string `json:"boards"` // channels they're given by name
}

type rosterProposalInfo struct {
	ID        string          `json:"id"`
	Op        json.RawMessage `json:"op"`
	By        string          `json:"by"`
	Signers   []string        `json:"signers"`
	Approvals int             `json:"approvals"`
	Needed    int             `json:"needed"`
	Ready     bool            `json:"ready"`
	Problem   string          `json:"problem"`
	Mine      bool            `json:"mine"`        // proposed by this account
	Signed    bool            `json:"signed"`      // this account has approved it
	Rejecters []string        `json:"rejecters"`   // approvers who turned it down
	Rejected  bool            `json:"rejected"`    // this account has turned it down
	TurnedDn  bool            `json:"turned_down"` // it can't get the approvals it needs any more
	CreatedAt string          `json:"created_at"`
}

type rosterChangeInfo struct {
	Op      json.RawMessage `json:"op"`
	Signers []string        `json:"signers"`
}

type rosterAccessInfo struct {
	Board   string   `json:"board"`
	All     bool     `json:"all"`
	Readers []string `json:"readers"`
}

type rosterInfo struct {
	State       string               `json:"state"`
	Problem     string               `json:"problem"`
	ID          string               `json:"id"`
	Head        string               `json:"head"`
	Owner       string               `json:"owner"`
	Policy      RosterPolicy         `json:"policy"`
	Approvers   []string             `json:"approvers"`
	Needed      int                  `json:"needed"`
	Me          string               `json:"me"`
	Members     []rosterMemberInfo   `json:"members"`
	Access      []rosterAccessInfo   `json:"access"`
	Proposals   []rosterProposalInfo `json:"proposals"`
	Changes     []rosterChangeInfo   `json:"changes"`
	Rejected    int                  `json:"rejected"`
	Forks       []string             `json:"forks"`
	PendingJoin bool                 `json:"pending_join"`
}

// RosterInfo is the member list as this app sees it, for the page.
func (a *App) RosterInfo() rosterInfo {
	rc := a.roster
	rc.mu.Lock()
	defer rc.mu.Unlock()
	info := rosterInfo{State: rc.state, Problem: rc.problem, Me: rc.me, Members: []rosterMemberInfo{}, Access: []rosterAccessInfo{},
		Proposals: []rosterProposalInfo{}, Changes: []rosterChangeInfo{}, Forks: []string{}, Approvers: []string{}}
	for u := range rc.forks {
		info.Forks = append(info.Forks, u)
	}
	sort.Strings(info.Forks)
	info.PendingJoin = rc.pin != nil && len(rc.pin.Joins) > 0
	r := rc.r
	if r == nil {
		return info
	}
	info.ID, info.Head, info.Owner, info.Policy = r.ID, headPrefix(r.Head), r.Owner, r.Policy
	info.Approvers, info.Needed, info.Rejected = r.Approvers(), r.Needed(), len(r.Rejected)
	named := map[string][]string{}
	for _, b := range r.Boards() {
		all, readers := r.Access(b)
		if readers == nil {
			readers = []string{}
		}
		info.Access = append(info.Access, rosterAccessInfo{Board: b, All: all, Readers: readers})
		for _, k := range readers {
			named[k] = append(named[k], b)
		}
	}
	for k, m := range r.Members {
		bs := named[k]
		if bs == nil {
			bs = []string{}
		}
		info.Members = append(info.Members, rosterMemberInfo{Key: k, Name: m.Name, Kind: m.Kind, Manager: r.Managers[k], Owner: k == r.Owner, Boards: bs})
	}
	sort.Slice(info.Members, func(i, j int) bool { return info.Members[i].Key < info.Members[j].Key })
	for _, p := range rc.proposals {
		c := r.Consider(p.Entry)
		if c.Op.ID == "" {
			continue
		}
		signers := p.Entry.signers()
		var who []string
		for k := range signers {
			who = append(who, k)
		}
		sort.Strings(who)
		rejected := rejecters(p.Entry.Body, p.Rejects)
		rej := []string{}
		for k := range rejected {
			if r.isApprover(k) {
				rej = append(rej, k)
			}
		}
		sort.Strings(rej)
		info.Proposals = append(info.Proposals, rosterProposalInfo{ID: c.Op.ID, Op: json.RawMessage(p.Entry.Body), By: c.Op.By, Signers: who,
			Approvals: c.Approvals, Needed: c.Needed, Ready: c.Ready, Problem: c.Problem, Mine: c.Op.By == rc.me, Signed: signers[rc.me], CreatedAt: p.CreatedAt,
			Rejecters: rej, Rejected: rejected[rc.me], TurnedDn: r.TurnedDown(rejected)})
	}
	for _, ch := range r.Changes {
		body, _ := json.Marshal(ch.Op)
		info.Changes = append(info.Changes, rosterChangeInfo{Op: body, Signers: ch.Signers})
	}
	return info
}

// signer: this account's identity key.
func (a *App) rosterSigner() (ed25519.PrivateKey, string, error) {
	id, ok := a.text.identity()
	if !ok {
		return nil, "", errors.New("not signed in")
	}
	return id.idPriv, id.idHex, nil
}

// RosterStartReq: the owner turning the list on.
type RosterStartReq struct {
	Policy   RosterPolicy   `json:"policy"`
	Members  []RosterMember `json:"members"`
	Managers []string       `json:"managers"`
	Access   []RosterAccess `json:"access"`
}

// RosterStart turns the signed member list on for this server (the owner
// only — the server refuses anyone else): its first entry, from what the
// page gathered and the owner checked.
func (a *App) RosterStart(req RosterStartReq) error {
	priv, me, err := a.rosterSigner()
	if err != nil {
		return err
	}
	op := newRosterOp("", me, opStart)
	op.Policy, op.Members, op.Managers, op.Access = &req.Policy, req.Members, req.Managers, req.Access
	for i := range op.Members {
		op.Members[i].Key = strings.ToLower(op.Members[i].Key)
	}
	e, err := makeRosterEntry(op, priv)
	if err != nil {
		return err
	}
	// Checked here first, the same way every app will check it.
	if _, err := BuildRoster([]RosterEntry{e}); err != nil {
		return err
	}
	if err := a.doJSON("POST", "/api/roster", e, nil); err != nil {
		return err
	}
	a.roster.refresh()
	return nil
}

// RosterPropose proposes a change (opJSON: the op's own fields — op, key,
// kind, name, boards, board, all, add, remove, on, policy, invite, note).
// It goes straight into the list if this account's approval is enough;
// else it waits for the others'. Returns "done" or "waiting".
func (a *App) RosterPropose(opJSON string) (string, error) {
	priv, me, err := a.rosterSigner()
	if err != nil {
		return "", err
	}
	rc := a.roster
	rc.refresh() // proposed against the list as it is now
	rc.mu.Lock()
	r := rc.r
	active := rc.state == rosterActive
	rc.mu.Unlock()
	if !active || r == nil {
		return "", errors.New("this server's member list isn't on (or doesn't check out)")
	}
	var in RosterOp
	if err := json.Unmarshal([]byte(opJSON), &in); err != nil {
		return "", fmt.Errorf("that change isn't readable: %v", err)
	}
	op := newRosterOp(r.ID, me, in.Op)
	op.Key, op.Kind, op.Name, op.Boards, op.Board, op.All = strings.ToLower(in.Key), in.Kind, in.Name, in.Boards, in.Board, in.All
	op.Add, op.Remove, op.On, op.Policy, op.Invite, op.Note = lowerAll(in.Add), lowerAll(in.Remove), in.On, in.Policy, in.Invite, in.Note
	e, err := makeRosterEntry(op, priv)
	if err != nil {
		return "", err
	}
	return a.rosterSubmit(r, e)
}

func lowerAll(ks []string) []string {
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = strings.ToLower(k)
	}
	return out
}

// rosterSubmit: into the list if it's ready, else a proposal.
func (a *App) rosterSubmit(_ *Roster, e RosterEntry) (string, error) {
	c := a.roster.consider(e)
	if c.Ready {
		if err := a.doJSON("POST", "/api/roster", e, nil); err != nil {
			return "", err
		}
		a.roster.refresh()
		return "done", nil
	}
	// Not enough approvals yet is expected; anything else is a real problem.
	if !strings.HasPrefix(c.Problem, "needs ") {
		return "", errors.New(c.Problem)
	}
	if err := a.doJSON("POST", "/api/roster/proposals", e, nil); err != nil {
		return "", err
	}
	a.roster.refresh()
	return "waiting", nil
}

// RosterApprove adds this account's approval to a waiting change (and puts
// it in the list once it has enough).
func (a *App) RosterApprove(id string) (string, error) {
	priv, me, err := a.rosterSigner()
	if err != nil {
		return "", err
	}
	rc := a.roster
	rc.mu.Lock()
	r := rc.r
	approver := r != nil && r.isApprover(me)
	var e *RosterEntry
	for i := range rc.proposals {
		if op, err := rc.proposals[i].Entry.op(); err == nil && op.ID == id {
			c := rc.proposals[i].Entry
			c.Sigs = append([]RosterSig(nil), c.Sigs...)
			e = &c
		}
	}
	rc.mu.Unlock()
	if e == nil || r == nil {
		return "", errors.New("that change isn't waiting any more")
	}
	if !approver {
		return "", errors.New("you're not one of this server's approvers")
	}
	e.sign(priv)
	if c := rc.consider(*e); c.Ready {
		if err := a.doJSON("POST", "/api/roster", e, nil); err != nil {
			return "", err
		}
		rc.refresh()
		return "done", nil
	}
	var mine RosterSig
	for _, s := range e.Sigs {
		if s.Key == me {
			mine = s
		}
	}
	if err := a.doJSON("POST", "/api/roster/proposals/"+id+"/sign", mine, nil); err != nil {
		return "", err
	}
	rc.refresh()
	return "waiting", nil
}

// RosterReject turns a proposed change down, as one of the approvers:
// signed, so every app can check it was really them. Once approvers have
// turned it down so it can't get the approvals it needs, every app shows it
// as turned down, and the proposer's or the owner's app clears it away.
func (a *App) RosterReject(id string) (string, error) {
	priv, me, err := a.rosterSigner()
	if err != nil {
		return "", err
	}
	rc := a.roster
	rc.mu.Lock()
	r := rc.r
	approver := r != nil && r.isApprover(me)
	body := ""
	for i := range rc.proposals {
		if op, err := rc.proposals[i].Entry.op(); err == nil && op.ID == id {
			body = rc.proposals[i].Entry.Body
		}
	}
	rc.mu.Unlock()
	if body == "" || r == nil {
		return "", errors.New("that change isn't waiting any more")
	}
	if !approver {
		return "", errors.New("you're not one of this server's approvers")
	}
	sig := RosterSig{Key: me, Sig: hex.EncodeToString(ed25519.Sign(priv, rosterRejectMessage(body)))}
	if err := a.doJSON("POST", "/api/roster/proposals/"+id+"/reject", sig, nil); err != nil {
		var h *httpError
		if errors.As(err, &h) && h.code == 404 {
			return "", errors.New("this server is too old to turn changes down — update it (the owner can still turn it down)")
		}
		return "", err
	}
	rc.refresh()
	return "rejected", nil
}

// consider checks an entry against the list as it is now.
func (rc *rosterClient) consider(e RosterEntry) Approval {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.r == nil {
		return Approval{Problem: "there's no member list"}
	}
	return rc.r.Consider(e)
}

// RosterWithdraw takes back (or, for the owner, turns down) a waiting change.
func (a *App) RosterWithdraw(id string) error {
	if err := a.doJSON("DELETE", "/api/roster/proposals/"+id, nil, nil); err != nil {
		return err
	}
	a.roster.refresh()
	return nil
}

// RosterAccept trusts the member list as it is now, after it was started
// over (the page asks first).
func (a *App) RosterAccept() error {
	rc := a.roster
	rc.mu.Lock()
	rc.pin = nil
	saveRosterPin(a.rosterPinPath(), rc.scope, nil)
	rc.entries, rc.lastSeq, rc.r = nil, 0, nil
	rc.state = rosterNone
	rc.mu.Unlock()
	rc.refresh()
	return nil
}

// RosterNewInvite makes an invite for the list: its secret (for the link,
// base64url) and whether it's ready or waiting for approvals.
func (a *App) RosterNewInvite(uses int, expires int64, boards []string) (map[string]string, error) {
	seed, pub := NewInviteSecret()
	inv := RosterInvite{Key: pub, Uses: uses, Expires: expires, Boards: boards}
	data, _ := json.Marshal(map[string]interface{}{"op": opInvite, "invite": inv})
	status, err := a.RosterPropose(string(data))
	if err != nil {
		return nil, err
	}
	return map[string]string{"secret": base64.RawURLEncoding.EncodeToString(seed), "key": pub, "status": status}, nil
}

// RosterJoin joins the list with an invite link's secret (after joining the
// server with its code). If the invite isn't in the list yet (approvals
// still coming), it's remembered and tried again as the list changes.
func (a *App) RosterJoin(secret string) error {
	seed, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(secret))
	if err != nil || len(seed) != ed25519.SeedSize {
		return errors.New("that invite link's secret is damaged")
	}
	rc := a.roster
	rc.begin() // (the connection may still be setting it up)
	rc.mu.Lock()
	if rc.scope == "" {
		rc.mu.Unlock()
		return errors.New("not connected")
	}
	if rc.pin == nil {
		rc.pin = &rosterPin{}
	}
	if !containsStr(rc.pin.Joins, secret) {
		rc.pin.Joins = append(rc.pin.Joins, secret)
	}
	saveRosterPin(a.rosterPinPath(), rc.scope, rc.pin)
	rc.mu.Unlock()
	rc.refresh()
	return nil
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func (rc *rosterClient) pendingJoinsLocked() []string {
	if rc.pin == nil || len(rc.pin.Joins) == 0 || rc.r == nil {
		return nil
	}
	if _, in := rc.r.Members[rc.me]; in {
		rc.pin.Joins = nil // already in
		saveRosterPin(rc.app.rosterPinPath(), rc.scope, rc.pin)
		return nil
	}
	return append([]string(nil), rc.pin.Joins...)
}

// tryJoin posts this account's join, if the invite is in the list.
func (rc *rosterClient) tryJoin(secret string) {
	seed, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || len(seed) != ed25519.SeedSize {
		return
	}
	id, ok := rc.app.text.identity()
	if !ok {
		return
	}
	pub := hex.EncodeToString(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
	rc.mu.Lock()
	usable, rid := rc.r != nil && rc.r.InviteUsable(pub, time.Now().Unix()), ""
	if rc.r != nil {
		rid = rc.r.ID
	}
	rc.mu.Unlock()
	if !usable {
		return // not approved yet (or no longer usable): waits
	}
	e, err := JoinEntry(rid, seed, id.idPriv, id.me)
	if err != nil {
		return
	}
	if c := rc.consider(e); !c.Ready {
		log.Printf("roster: joining: %s", c.Problem)
		return
	}
	if err := rc.app.doJSON("POST", "/api/roster", e, nil); err != nil {
		log.Printf("roster: joining: %v", err)
		return
	}
	log.Printf("roster: joined this server's member list with an invite")
	rc.changed()
}

// ── What the server says, to compare ─────────────────────────────────────────

type rosterServerMember struct {
	Key      string `json:"key"`
	Username string `json:"username"`
	Kind     string `json:"kind"`
}

type rosterServerView struct {
	Members []rosterServerMember `json:"members"`
	// Bots: every bot and plugin the server has (identity keys), and
	// whether that list loaded — so the page can tell one that was removed
	// from the server but is still on the member list.
	Bots   []string `json:"bots"`
	BotsOK bool     `json:"bots_ok"`
	// Boards: channel → the identity keys the server says can see it.
	// Channels this account can't see are missing (the server won't say).
	Boards map[string][]string `json:"boards"`
}

// RosterServerView is who the server says can see each of these channels
// (and which members are bots or plugins) — for turning the list on, and
// for showing approvers where it and the list disagree. It's only what the
// server claims: the list is what counts.
func (a *App) RosterServerView(boards []string) (rosterServerView, error) {
	v := rosterServerView{Members: []rosterServerMember{}, Boards: map[string][]string{}, Bots: []string{}}
	kinds := map[string]string{}
	var bots struct {
		Bots []struct {
			PublicKey string `json:"public_key"`
			Plugin    bool   `json:"plugin"`
		} `json:"bots"`
	}
	if err := a.doJSON("GET", "/api/bots", nil, &bots); err == nil {
		v.BotsOK = true
		for _, b := range bots.Bots {
			k := strings.ToLower(b.PublicKey)
			kinds[k] = map[bool]string{true: kindPlugin, false: kindBot}[b.Plugin]
			v.Bots = append(v.Bots, k)
		}
	}
	seen := map[string]bool{}
	for _, b := range boards {
		var r keysResp
		if err := a.doJSON("GET", "/api/boards/"+url.PathEscape(b)+"/keys", nil, &r); err != nil {
			continue
		}
		keys := []string{}
		for _, m := range r.Members {
			k := strings.ToLower(m.PublicKey)
			if !validKeyHex(k) {
				continue
			}
			keys = append(keys, k)
			if !seen[k] {
				seen[k] = true
				kind := kinds[k]
				if kind == "" {
					kind = kindMember
				}
				v.Members = append(v.Members, rosterServerMember{Key: k, Username: m.Username, Kind: kind})
			}
		}
		sort.Strings(keys)
		v.Boards[b] = keys
	}
	sort.Slice(v.Members, func(i, j int) bool { return v.Members[i].Username < v.Members[j].Username })
	return v, nil
}

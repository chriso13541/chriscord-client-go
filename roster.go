package main

// roster.go — the signed member list ("den of thieves").
//
// Who may read a channel used to be whatever the server said. A server
// run by someone hostile (or a box someone else controls) could add a
// member of its own and every app would hand that member the channel's
// keys. With a signed member list, the apps decide for themselves:
//
//   - The list is a log of changes, each one signed. The server stores and
//     orders it, and can't forge, change or (unnoticed) reorder anything in
//     it: every app checks every entry itself and works out the list.
//   - Channel keys and call keys are only ever given to someone the list
//     says may read that channel (textcrypt.go, e2ee.go). If the server
//     says otherwise, the app says so and keeps the keys to itself.
//
// Who signs (the owner's setting, in the list itself):
//   - approvers: the owner only, or the owner and everyone with Manage
//     Members (the list's "managers");
//   - how many must sign a change that lets someone read more: 1, a
//     number (never more than there are approvers), or a majority.
//
// What needs the approvers ("quorum"), and what one person can do:
//
//	quorum:     adding a member, bot or plugin directly; giving anyone
//	            access to a channel (or making a channel everyone's);
//	            making an invite; adding or removing a manager; changing
//	            the setting; handing over the server
//	one:        removing a member (not a manager) or access; revoking an
//	            invite — these only take away, so the worst is vandalism
//	            you can undo, never a hidden reader. Anyone may leave.
//	invite:     joining with an invite needs no approver at the time: the
//	            approvers signed the invite. Its link carries a secret key
//	            the server never sees, and the join is signed with it.
//
// Bots and plugins are members of their own kind: channels that are
// "everyone's" don't include them — only channels they're given by name.
//
// Wire format: each entry is {"body": "<JSON text>", "sigs": [{key, sig}]}.
// The body is kept as the exact text that was signed (no re-encoding), and
// each signature is Ed25519 over "chriscord-roster-v1\x00" + body.

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	rosterVersion = 1
	rosterContext = "chriscord-roster-v1\x00"
	// Limits that keep a hostile log from being expensive to check.
	rosterMaxBody    = 64 << 10
	rosterMaxSigs    = 64
	rosterMaxList    = 5000
	rosterMaxInviteN = 10000
)

// Kinds of member.
const (
	kindMember = "member"
	kindBot    = "bot"
	kindPlugin = "plugin"
)

// RosterPolicy is the owner's setting: who approves, and how many must.
type RosterPolicy struct {
	Approvers string `json:"approvers"` // "owner" or "managers" (owner and managers)
	Need      string `json:"need"`      // "1", "n" or "majority"
	N         int    `json:"n,omitempty"`
}

type RosterMember struct {
	Key  string `json:"key"`  // identity key, hex
	Kind string `json:"kind"` // member, bot or plugin
	Name string `json:"name,omitempty"`
}

// RosterAccess is who may read one channel.
type RosterAccess struct {
	Board   string   `json:"board"`
	All     bool     `json:"all,omitempty"` // every member (not bots or plugins)
	Readers []string `json:"readers,omitempty"`
}

// RosterInvite is an invite the approvers signed. Key is the public half
// of the secret in its link; joining with it is signed with that secret.
type RosterInvite struct {
	Key     string   `json:"key"`
	Uses    int      `json:"uses"`              // 0: no limit
	Expires int64    `json:"expires,omitempty"` // unix seconds; 0: never
	Boards  []string `json:"boards,omitempty"`  // channels it gives, beyond everyone's
}

// RosterOp is one change (the signed body).
type RosterOp struct {
	V      int    `json:"v"`
	Roster string `json:"roster,omitempty"` // the list's id (its first entry's id); empty in that one
	ID     string `json:"id"`               // random, unique
	At     int64  `json:"at"`               // unix seconds (as the signer's clock said)
	By     string `json:"by"`               // who proposed it
	Op     string `json:"op"`

	Key      string          `json:"key,omitempty"` // the member, manager or invite it's about
	Kind     string          `json:"kind,omitempty"`
	Name     string          `json:"name,omitempty"`
	Boards   []string        `json:"boards,omitempty"`
	Board    string          `json:"board,omitempty"`
	All      *bool           `json:"all,omitempty"`
	Add      []string        `json:"add,omitempty"`
	Remove   []string        `json:"remove,omitempty"`
	On       *bool           `json:"on,omitempty"`
	Policy   *RosterPolicy   `json:"policy,omitempty"`
	Invite   *RosterInvite   `json:"invite,omitempty"`
	Members  []RosterMember  `json:"members,omitempty"`  // start
	Managers []string        `json:"managers,omitempty"` // start
	Access   []RosterAccess  `json:"access,omitempty"`   // start
	Note     string          `json:"note,omitempty"`     // why (shown to approvers)
	Extra    json.RawMessage `json:"extra,omitempty"`    // reserved
}

// The operations.
const (
	opStart    = "start"    // the first entry: owner (By), setting, members, managers, access
	opAdd      = "add"      // Key joins as Kind, reading Boards (quorum)
	opJoin     = "join"     // Key joins with invite Key… (see below)
	opRemove   = "remove"   // Key leaves or is removed
	opAccess   = "access"   // Board: All, Add readers, Remove readers
	opManager  = "manager"  // Key becomes (On) or stops being a manager
	opPolicy   = "policy"   // the setting changes
	opInvite   = "invite"   // a new invite
	opUninvite = "uninvite" // an invite stops working
	opOwner    = "owner"    // the server is handed to Key
)

// A join is signed by the person joining (By) and the invite's secret. Its
// Key is the invite's key and Name the joiner's name.

type RosterSig struct {
	Key string `json:"key"`
	Sig string `json:"sig"`
}

// RosterEntry is one entry as stored and sent.
type RosterEntry struct {
	Seq  int64       `json:"seq,omitempty"`
	Body string      `json:"body"`
	Sigs []RosterSig `json:"sigs"`
}

func rosterMessage(body string) []byte { return append([]byte(rosterContext), body...) }

// newRosterOp fills in the common fields of an op proposed by "by".
func newRosterOp(roster, by, op string) RosterOp {
	id := make([]byte, 16)
	rand.Read(id)
	return RosterOp{V: rosterVersion, Roster: roster, ID: hex.EncodeToString(id), At: time.Now().Unix(), By: by, Op: op}
}

// makeRosterEntry turns an op into an entry signed by priv.
func makeRosterEntry(op RosterOp, priv ed25519.PrivateKey) (RosterEntry, error) {
	body, err := json.Marshal(op)
	if err != nil {
		return RosterEntry{}, err
	}
	e := RosterEntry{Body: string(body)}
	e.sign(priv)
	return e, nil
}

// sign adds priv's signature (once).
func (e *RosterEntry) sign(priv ed25519.PrivateKey) {
	pub := hex.EncodeToString(priv.Public().(ed25519.PublicKey))
	for _, s := range e.Sigs {
		if s.Key == pub {
			return
		}
	}
	e.Sigs = append(e.Sigs, RosterSig{Key: pub, Sig: hex.EncodeToString(ed25519.Sign(priv, rosterMessage(e.Body)))})
}

// op reads the entry's body (not checked).
func (e RosterEntry) op() (RosterOp, error) {
	var op RosterOp
	if len(e.Body) > rosterMaxBody {
		return op, errors.New("too big")
	}
	dec := json.NewDecoder(strings.NewReader(e.Body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&op); err != nil {
		return op, fmt.Errorf("unreadable: %v", err)
	}
	return op, nil
}

// signers: the keys whose signatures are valid (each once).
func (e RosterEntry) signers() map[string]bool {
	out := map[string]bool{}
	if len(e.Sigs) > rosterMaxSigs {
		return out
	}
	msg := rosterMessage(e.Body)
	for _, s := range e.Sigs {
		k := strings.ToLower(s.Key)
		pub, err1 := hex.DecodeString(k)
		sig, err2 := hex.DecodeString(s.Sig)
		if err1 == nil && err2 == nil && len(pub) == ed25519.PublicKeySize && ed25519.Verify(pub, msg, sig) {
			out[k] = true
		}
	}
	return out
}

// entryHash identifies an entry's body (signatures aside).
func entryHash(body string) [32]byte { return sha256.Sum256([]byte(body)) }

// ── The list, worked out ─────────────────────────────────────────────────────

type rosterInviteState struct {
	RosterInvite
	used    int
	revoked bool
	by      string
}

type rosterAccessState struct {
	all     bool
	readers map[string]bool
}

// Roster is the member list worked out from the log, every entry checked.
type Roster struct {
	ID       string
	Owner    string
	Policy   RosterPolicy
	Members  map[string]RosterMember
	Managers map[string]bool
	access   map[string]*rosterAccessState
	invites  map[string]*rosterInviteState
	seen     map[string]bool
	// Head identifies the whole log so far (a hash chain over the entries
	// applied), so two apps can tell whether they see the same list.
	Head    string
	Applied int
	// Rejected: entries the server stored that don't hold up, and why.
	Rejected []RosterRejected
	// Changes: what each applied entry did, for showing (newest last).
	Changes []RosterChange
}

type RosterRejected struct {
	Index int
	Why   string
}

// RosterChange is one applied entry, for the app to show.
type RosterChange struct {
	Op      RosterOp
	Signers []string
}

func validKeyHex(k string) bool {
	if len(k) != 64 || strings.ToLower(k) != k {
		return false
	}
	_, err := hex.DecodeString(k)
	return err == nil
}

func validPolicy(p RosterPolicy) error {
	switch p.Approvers {
	case "owner", "managers":
	default:
		return errors.New("approvers must be owner or managers")
	}
	switch p.Need {
	case "1", "majority":
	case "n":
		if p.N < 1 {
			return errors.New("the number of approvals must be at least 1")
		}
	default:
		return errors.New("need must be 1, n or majority")
	}
	return nil
}

// Approvers: who may approve changes, sorted.
func (r *Roster) Approvers() []string {
	out := []string{r.Owner}
	if r.Policy.Approvers == "managers" {
		for k := range r.Managers {
			if k != r.Owner {
				out = append(out, k)
			}
		}
	}
	sort.Strings(out[1:])
	return out
}

// Needed: how many approvers must sign a change.
func (r *Roster) Needed() int {
	n := len(r.Approvers())
	switch r.Policy.Need {
	case "n":
		return r.Policy.N
	case "majority":
		return n/2 + 1
	}
	return 1
}

// approvals: how many of signers are approvers.
func (r *Roster) approvals(signers map[string]bool) int {
	n := 0
	for _, a := range r.Approvers() {
		if signers[a] {
			n++
		}
	}
	return n
}

func (r *Roster) quorum(signers map[string]bool) bool { return r.approvals(signers) >= r.Needed() }

// canManage: the owner or a manager (for the changes one person may make).
func (r *Roster) canManage(key string) bool { return key == r.Owner || r.Managers[key] }

func (r *Roster) anySigner(signers map[string]bool, ok func(string) bool) bool {
	for k := range signers {
		if ok(k) {
			return true
		}
	}
	return false
}

// CanRead: whether key may read board (be given its keys).
func (r *Roster) CanRead(key, board string) bool {
	m, ok := r.Members[key]
	if !ok {
		return false
	}
	a := r.access[board]
	if a == nil {
		return false
	}
	return a.readers[key] || (a.all && m.Kind == kindMember)
}

// Readers: everyone who may read board, sorted.
func (r *Roster) Readers(board string) []string {
	var out []string
	for k := range r.Members {
		if r.CanRead(k, board) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Knows: whether the list says anything about board yet.
func (r *Roster) Knows(board string) bool { return r.access[board] != nil }

// Access is board's access, as the list has it.
func (r *Roster) Access(board string) (all bool, readers []string) {
	a := r.access[board]
	if a == nil {
		return false, nil
	}
	for k := range a.readers {
		readers = append(readers, k)
	}
	sort.Strings(readers)
	return a.all, readers
}

// Boards: every channel the list mentions, sorted.
func (r *Roster) Boards() []string {
	var out []string
	for b := range r.access {
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

// Invite: an invite by its key (nil if it's unknown).
func (r *Roster) Invite(key string) *RosterInvite {
	if i := r.invites[key]; i != nil {
		c := i.RosterInvite
		return &c
	}
	return nil
}

// InviteUsable: whether an invite still lets someone in (at unix time now).
func (r *Roster) InviteUsable(key string, now int64) bool {
	i := r.invites[key]
	return i != nil && !i.revoked && (i.Uses == 0 || i.used < i.Uses) && (i.Expires == 0 || now <= i.Expires)
}

func (r *Roster) boardAccess(b string) *rosterAccessState {
	a := r.access[b]
	if a == nil {
		a = &rosterAccessState{readers: map[string]bool{}}
		r.access[b] = a
	}
	return a
}

func validBoard(b string) bool { return b != "" && len(b) <= 100 && !strings.ContainsAny(b, "\x00\n") }

func validKind(k string) bool { return k == kindMember || k == kindBot || k == kindPlugin }

func validName(n string) bool { return len(n) <= 64 && !strings.ContainsAny(n, "\x00\n") }

// BuildRoster works out the list from its log, checking every entry. The
// first entry must be the start; an entry that doesn't hold up is left out
// (and listed in Rejected), so a server can't break the list for everyone
// by storing junk — only refuse to store what's good.
func BuildRoster(entries []RosterEntry) (*Roster, error) {
	if len(entries) == 0 {
		return nil, errors.New("the member list is empty")
	}
	if len(entries) > 200000 {
		return nil, errors.New("the member list is too long")
	}
	r := &Roster{}
	if err := r.start(entries[0]); err != nil {
		return nil, fmt.Errorf("its first entry doesn't hold up: %v", err)
	}
	for i, e := range entries[1:] {
		if err := r.Apply(e); err != nil {
			r.Rejected = append(r.Rejected, RosterRejected{Index: i + 1, Why: err.Error()})
		}
	}
	return r, nil
}

func (r *Roster) start(e RosterEntry) error {
	op, err := e.op()
	if err != nil {
		return err
	}
	if op.V != rosterVersion || op.Op != opStart || op.Roster != "" || len(op.ID) != 32 || !validKeyHex(op.By) {
		return errors.New("not a start")
	}
	if !e.signers()[op.By] {
		return errors.New("not signed by its owner")
	}
	if err := validPolicy(derefPolicy(op.Policy)); err != nil {
		return err
	}
	*r = Roster{ID: op.ID, Owner: op.By, Policy: *op.Policy, Members: map[string]RosterMember{}, Managers: map[string]bool{},
		access: map[string]*rosterAccessState{}, invites: map[string]*rosterInviteState{}, seen: map[string]bool{op.ID: true}}
	if len(op.Members) > rosterMaxList || len(op.Access) > rosterMaxList || len(op.Managers) > rosterMaxList {
		return errors.New("too long")
	}
	for _, m := range op.Members {
		if !validKeyHex(m.Key) || !validKind(m.Kind) || !validName(m.Name) {
			return fmt.Errorf("bad member %q", m.Key)
		}
		r.Members[m.Key] = m
	}
	if _, ok := r.Members[r.Owner]; !ok {
		r.Members[r.Owner] = RosterMember{Key: r.Owner, Kind: kindMember}
	}
	for _, k := range op.Managers {
		if m, ok := r.Members[k]; !ok || m.Kind != kindMember {
			return fmt.Errorf("manager %q isn't a member", k)
		}
		r.Managers[k] = true
	}
	for _, a := range op.Access {
		if !validBoard(a.Board) || len(a.Readers) > rosterMaxList {
			return fmt.Errorf("bad channel %q", a.Board)
		}
		s := r.boardAccess(a.Board)
		s.all = a.All
		for _, k := range a.Readers {
			if _, ok := r.Members[k]; !ok {
				return fmt.Errorf("reader %q isn't a member", k)
			}
			s.readers[k] = true
		}
	}
	if r.Needed() > len(r.Approvers()) {
		return errors.New("it needs more approvals than there are approvers")
	}
	r.applied(e, op, map[string]bool{op.By: true})
	return nil
}

func derefPolicy(p *RosterPolicy) RosterPolicy {
	if p == nil {
		return RosterPolicy{}
	}
	return *p
}

func (r *Roster) applied(e RosterEntry, op RosterOp, signers map[string]bool) {
	prev, _ := hex.DecodeString(r.Head)
	h := entryHash(e.Body)
	sum := sha256.Sum256(append(prev, h[:]...))
	r.Head = hex.EncodeToString(sum[:])
	r.Applied++
	var who []string
	for k := range signers {
		who = append(who, k)
	}
	sort.Strings(who)
	if len(r.Changes) >= 500 {
		r.Changes = r.Changes[1:]
	}
	r.Changes = append(r.Changes, RosterChange{Op: op, Signers: who})
}

// Apply checks one more entry against the list as it stands and, if it
// holds up, applies it.
func (r *Roster) Apply(e RosterEntry) error {
	op, err := e.op()
	if err != nil {
		return err
	}
	if op.V != rosterVersion {
		return errors.New("from a newer version")
	}
	if op.Roster != r.ID {
		return errors.New("for a different member list")
	}
	if len(op.ID) != 32 || r.seen[op.ID] {
		return errors.New("a repeat")
	}
	if !validKeyHex(op.By) || !validName(op.Name) || len(op.Note) > 500 {
		return errors.New("malformed")
	}
	signers := e.signers()
	if !signers[op.By] {
		return errors.New("not signed by whoever proposed it")
	}
	if err := r.check(op, signers); err != nil {
		return err
	}
	r.seen[op.ID] = true
	r.applied(e, op, signers)
	return nil
}

// check sees whether op, signed by signers, may happen — and makes it.
// Every check comes before any change, so a refused op changes nothing.
func (r *Roster) check(op RosterOp, signers map[string]bool) error {
	quorum := r.quorum(signers)
	needQuorum := func() error {
		if !quorum {
			return fmt.Errorf("needs %d approval(s), has %d", r.Needed(), r.approvals(signers))
		}
		return nil
	}
	switch op.Op {
	case opAdd:
		if !validKeyHex(op.Key) || !validKind(op.Kind) {
			return errors.New("malformed")
		}
		if _, ok := r.Members[op.Key]; ok {
			return errors.New("already a member")
		}
		if err := checkBoards(op.Boards); err != nil {
			return err
		}
		if err := needQuorum(); err != nil {
			return err
		}
		r.Members[op.Key] = RosterMember{Key: op.Key, Kind: op.Kind, Name: op.Name}
		for _, b := range op.Boards {
			r.boardAccess(b).readers[op.Key] = true
		}

	case opJoin:
		// By joins, with the invite whose key is Key, signed by both.
		inv := r.invites[op.Key]
		if inv == nil {
			return errors.New("no such invite")
		}
		if !signers[op.Key] {
			return errors.New("not signed with the invite's secret")
		}
		if !r.InviteUsable(op.Key, op.At) {
			return errors.New("the invite is used up, expired or revoked")
		}
		if _, ok := r.Members[op.By]; ok {
			return errors.New("already a member")
		}
		inv.used++
		r.Members[op.By] = RosterMember{Key: op.By, Kind: kindMember, Name: op.Name}
		for _, b := range inv.Boards {
			r.boardAccess(b).readers[op.By] = true
		}

	case opRemove:
		if _, ok := r.Members[op.Key]; !ok {
			return errors.New("not a member")
		}
		if op.Key == r.Owner {
			return errors.New("the owner can't be removed (hand the server over first)")
		}
		self := op.Key == op.By
		approver := r.isApprover(op.Key)
		switch {
		case approver && !self:
			// Removing an approver could lower the bar: the approvers decide.
			if err := needQuorum(); err != nil {
				return err
			}
		case !self && !r.anySigner(signers, r.canManage):
			return errors.New("only a manager can remove someone")
		}
		after := r.clone()
		after.drop(op.Key)
		if after.Needed() > len(after.Approvers()) {
			return errors.New("there'd be fewer approvers than approvals needed — lower the setting first")
		}
		r.drop(op.Key)

	case opAccess:
		if !validBoard(op.Board) || len(op.Add) > rosterMaxList || len(op.Remove) > rosterMaxList {
			return errors.New("malformed")
		}
		a := r.access[op.Board]
		wasAll := a != nil && a.all
		widens := len(op.Add) > 0 || (op.All != nil && *op.All && !wasAll)
		for _, k := range op.Add {
			if _, ok := r.Members[k]; !ok {
				return fmt.Errorf("%s isn't a member", short(k))
			}
		}
		if widens {
			if err := needQuorum(); err != nil {
				return err
			}
		} else if !r.anySigner(signers, r.canManage) {
			return errors.New("only a manager can change access")
		}
		s := r.boardAccess(op.Board)
		if op.All != nil {
			s.all = *op.All
		}
		for _, k := range op.Remove {
			delete(s.readers, k)
		}
		for _, k := range op.Add {
			s.readers[k] = true
		}

	case opManager:
		if op.On == nil {
			return errors.New("malformed")
		}
		m, ok := r.Members[op.Key]
		if !ok || m.Kind != kindMember {
			return errors.New("only a member can be a manager")
		}
		self := op.Key == op.By && !*op.On
		if !self {
			if err := needQuorum(); err != nil {
				return err
			}
		}
		after := r.clone()
		if *op.On {
			after.Managers[op.Key] = true
		} else {
			delete(after.Managers, op.Key)
		}
		if after.Needed() > len(after.Approvers()) {
			return errors.New("there'd be fewer approvers than approvals needed — lower the setting first")
		}
		r.Managers = after.Managers

	case opPolicy:
		if op.Policy == nil {
			return errors.New("malformed")
		}
		if err := validPolicy(*op.Policy); err != nil {
			return err
		}
		if err := needQuorum(); err != nil {
			return err
		}
		after := r.clone()
		after.Policy = *op.Policy
		if after.Needed() > len(after.Approvers()) {
			return fmt.Errorf("it would need %d approvals, but there are only %d approvers", after.Needed(), len(after.Approvers()))
		}
		r.Policy = *op.Policy

	case opInvite:
		inv := op.Invite
		if inv == nil || !validKeyHex(inv.Key) || inv.Uses < 0 || inv.Uses > rosterMaxInviteN || inv.Expires < 0 {
			return errors.New("malformed")
		}
		if r.invites[inv.Key] != nil {
			return errors.New("a repeat")
		}
		if err := checkBoards(inv.Boards); err != nil {
			return err
		}
		if err := needQuorum(); err != nil {
			return err
		}
		r.invites[inv.Key] = &rosterInviteState{RosterInvite: *inv, by: op.By}

	case opUninvite:
		inv := r.invites[op.Key]
		if inv == nil {
			return errors.New("no such invite")
		}
		if !r.anySigner(signers, r.canManage) {
			return errors.New("only a manager can revoke an invite")
		}
		inv.revoked = true

	case opOwner:
		m, ok := r.Members[op.Key]
		if !ok || m.Kind != kindMember || op.Key == r.Owner {
			return errors.New("can only hand the server to another member")
		}
		if !signers[op.Key] {
			return errors.New("the new owner has to sign it too")
		}
		if err := needQuorum(); err != nil {
			return err
		}
		after := r.clone()
		after.Owner = op.Key
		if after.Needed() > len(after.Approvers()) {
			return errors.New("there'd be fewer approvers than approvals needed — lower the setting first")
		}
		r.Owner = op.Key

	default:
		return fmt.Errorf("unknown change %q", op.Op)
	}
	return nil
}

func checkBoards(bs []string) error {
	if len(bs) > rosterMaxList {
		return errors.New("too many channels")
	}
	for _, b := range bs {
		if !validBoard(b) {
			return errors.New("bad channel")
		}
	}
	return nil
}

func (r *Roster) isApprover(key string) bool {
	for _, a := range r.Approvers() {
		if a == key {
			return true
		}
	}
	return false
}

// drop takes someone out of the list: membership, managing, every channel.
func (r *Roster) drop(key string) {
	delete(r.Members, key)
	delete(r.Managers, key)
	for _, a := range r.access {
		delete(a.readers, key)
	}
}

// clone: enough of a copy to try a change on (owner, setting, members,
// managers).
func (r *Roster) clone() *Roster {
	c := &Roster{Owner: r.Owner, Policy: r.Policy, Members: map[string]RosterMember{}, Managers: map[string]bool{}, access: map[string]*rosterAccessState{}}
	for k, v := range r.Members {
		c.Members[k] = v
	}
	for k := range r.Managers {
		c.Managers[k] = true
	}
	return c
}

func short(k string) string {
	if len(k) > 12 {
		return k[:12] + "…"
	}
	return k
}

// ── Making changes ───────────────────────────────────────────────────────────

// Approval tells an approver what a proposed entry would do, and whether
// it can go through yet.
type Approval struct {
	Op        RosterOp
	Approvals int  // approvers who have signed
	Needed    int  // how many must
	Ready     bool // it would apply now
	Problem   string
}

// Consider checks a proposed entry against the list without applying it.
func (r *Roster) Consider(e RosterEntry) Approval {
	op, err := e.op()
	if err != nil {
		return Approval{Problem: err.Error()}
	}
	signers := e.signers()
	a := Approval{Op: op, Approvals: r.approvals(signers), Needed: r.Needed()}
	trial, err := r.copyAll()
	if err != nil {
		a.Problem = err.Error()
		return a
	}
	if err := trial.Apply(e); err != nil {
		a.Problem = err.Error()
	} else {
		a.Ready = true
	}
	return a
}

// copyAll: a full copy of the list (to try an entry on).
func (r *Roster) copyAll() (*Roster, error) {
	c := r.clone()
	c.ID, c.Head, c.Applied = r.ID, r.Head, r.Applied
	c.seen = map[string]bool{}
	for k := range r.seen {
		c.seen[k] = true
	}
	for b, a := range r.access {
		s := &rosterAccessState{all: a.all, readers: map[string]bool{}}
		for k := range a.readers {
			s.readers[k] = true
		}
		c.access[b] = s
	}
	c.invites = map[string]*rosterInviteState{}
	for k, v := range r.invites {
		x := *v
		c.invites[k] = &x
	}
	return c, nil
}

// NewInviteSecret makes the secret an invite link carries: its seed (for
// the link) and its public key (for the list).
func NewInviteSecret() (seed []byte, pubHex string) {
	seed = make([]byte, ed25519.SeedSize)
	rand.Read(seed)
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	return seed, hex.EncodeToString(pub)
}

// JoinEntry: joining the list with an invite's secret, as identity priv.
func JoinEntry(rosterID string, inviteSeed []byte, priv ed25519.PrivateKey, name string) (RosterEntry, error) {
	if len(inviteSeed) != ed25519.SeedSize {
		return RosterEntry{}, errors.New("that invite's secret is damaged")
	}
	inv := ed25519.NewKeyFromSeed(inviteSeed)
	op := newRosterOp(rosterID, hex.EncodeToString(priv.Public().(ed25519.PublicKey)), opJoin)
	op.Key = hex.EncodeToString(inv.Public().(ed25519.PublicKey))
	op.Name = name
	e, err := makeRosterEntry(op, priv)
	if err != nil {
		return e, err
	}
	e.sign(inv)
	return e, nil
}

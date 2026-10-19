package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
)

type rk struct {
	priv ed25519.PrivateKey
	hex  string
}

func newRK() rk {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	return rk{priv, hex.EncodeToString(priv.Public().(ed25519.PublicKey))}
}

// rosterLog builds a log as an app would, checking each entry as it goes.
type rosterLog struct {
	t       *testing.T
	entries []RosterEntry
	r       *Roster
}

func startLog(t *testing.T, owner rk, policy RosterPolicy, members []RosterMember, managers []string, access []RosterAccess) *rosterLog {
	t.Helper()
	op := newRosterOp("", owner.hex, opStart)
	op.Policy, op.Members, op.Managers, op.Access = &policy, members, managers, access
	e, err := makeRosterEntry(op, owner.priv)
	if err != nil {
		t.Fatal(err)
	}
	r, err := BuildRoster([]RosterEntry{e})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return &rosterLog{t: t, entries: []RosterEntry{e}, r: r}
}

// propose makes op by the first signer, signed by all of them; ok says
// whether it should go through.
func (l *rosterLog) propose(op RosterOp, ok bool, signers ...rk) RosterEntry {
	l.t.Helper()
	op.Roster = l.r.ID
	e, err := makeRosterEntry(op, signers[0].priv)
	if err != nil {
		l.t.Fatal(err)
	}
	for _, s := range signers[1:] {
		e.sign(s.priv)
	}
	l.add(e, ok)
	return e
}

func (l *rosterLog) add(e RosterEntry, ok bool) {
	l.t.Helper()
	err := l.r.Apply(e)
	if ok && err != nil {
		op, _ := e.op()
		l.t.Fatalf("%s should go through: %v", op.Op, err)
	}
	if !ok && err == nil {
		op, _ := e.op()
		l.t.Fatalf("%s should be refused", op.Op)
	}
	l.entries = append(l.entries, e)
	// Worked out from scratch, the list comes out the same.
	again, err := BuildRoster(l.entries)
	if err != nil || again.Head != l.r.Head || len(again.Members) != len(l.r.Members) {
		l.t.Fatalf("rebuilding the list gave something else (%v)", err)
	}
}

func opOf(by rk, kind string) RosterOp { return newRosterOp("", by.hex, kind) }

func tru() *bool  { b := true; return &b }
func fals() *bool { b := false; return &b }

func member(k rk) RosterMember { return RosterMember{Key: k.hex, Kind: kindMember} }

func TestRosterOwnerOnly(t *testing.T) {
	owner, sam, eve := newRK(), newRK(), newRK()
	l := startLog(t, owner, RosterPolicy{Approvers: "owner", Need: "1"},
		[]RosterMember{member(owner), member(sam)}, []string{sam.hex},
		[]RosterAccess{{Board: "general", All: true}, {Board: "secret", Readers: []string{owner.hex}}})
	if !l.r.CanRead(sam.hex, "general") || l.r.CanRead(sam.hex, "secret") || !l.r.CanRead(owner.hex, "secret") {
		t.Fatal("start access wrong")
	}
	// A manager isn't an approver here: can't add anyone.
	add := opOf(sam, opAdd)
	add.Key, add.Kind = eve.hex, kindMember
	l.propose(add, false, sam)
	// The owner can.
	add = opOf(owner, opAdd)
	add.Key, add.Kind, add.Boards = eve.hex, kindMember, []string{"secret"}
	l.propose(add, true, owner)
	if !l.r.CanRead(eve.hex, "general") || !l.r.CanRead(eve.hex, "secret") {
		t.Fatal("eve should read both")
	}
	// But a manager can remove her (taking away is one person's call).
	rm := opOf(sam, opRemove)
	rm.Key = eve.hex
	l.propose(rm, true, sam)
	if l.r.CanRead(eve.hex, "general") {
		t.Fatal("eve's gone")
	}
	// The same signed entry, sent again by the server: refused.
	l.add(l.entries[len(l.entries)-2], false)
}

func TestRosterQuorum(t *testing.T) {
	owner, a, b, c, eve := newRK(), newRK(), newRK(), newRK(), newRK()
	l := startLog(t, owner, RosterPolicy{Approvers: "managers", Need: "n", N: 2},
		[]RosterMember{member(owner), member(a), member(b), member(c)}, []string{a.hex, b.hex},
		[]RosterAccess{{Board: "general", All: true}})
	if got := l.r.Approvers(); len(got) != 3 || l.r.Needed() != 2 {
		t.Fatalf("approvers %v, needed %d", got, l.r.Needed())
	}
	add := func() RosterOp { o := opOf(a, opAdd); o.Key, o.Kind = eve.hex, kindMember; return o }
	// One manager alone: no. One manager plus someone who isn't an approver: no.
	l.propose(add(), false, a)
	l.propose(add(), false, a, c)
	l.propose(add(), false, a, eve)
	// The same signature twice doesn't count twice.
	o := add()
	o.Roster = l.r.ID
	e, _ := makeRosterEntry(o, a.priv)
	e.Sigs = append(e.Sigs, e.Sigs[0])
	l.add(e, false)
	// Two approvers: yes.
	l.propose(add(), true, a, b)

	// Can't need more approvals than there are approvers.
	pol := opOf(owner, opPolicy)
	pol.Policy = &RosterPolicy{Approvers: "managers", Need: "n", N: 4}
	l.propose(pol, false, owner, a, b)
	// Nor remove an approver if it would leave too few — or with one signature.
	rm := opOf(a, opRemove)
	rm.Key = b.hex
	l.propose(rm, false, a) // removing an approver needs the approvers
	pol = opOf(owner, opPolicy)
	pol.Policy = &RosterPolicy{Approvers: "managers", Need: "n", N: 3}
	l.propose(pol, true, owner, a)
	rm = opOf(a, opRemove)
	rm.Key = b.hex
	l.propose(rm, false, a, owner, b) // would leave 2 approvers for 3 approvals
	// A manager stepping down by themselves: also not below the bar.
	off := opOf(b, opManager)
	off.Key, off.On = b.hex, fals()
	l.propose(off, false, b)

	// Majority: 3 approvers → 2; add a fourth → 3.
	pol = opOf(owner, opPolicy)
	pol.Policy = &RosterPolicy{Approvers: "managers", Need: "majority"}
	l.propose(pol, true, owner, a, b)
	if l.r.Needed() != 2 {
		t.Fatalf("majority of 3 is 2, got %d", l.r.Needed())
	}
	mgr := opOf(owner, opManager)
	mgr.Key, mgr.On = c.hex, tru()
	l.propose(mgr, true, owner, a)
	if l.r.Needed() != 3 {
		t.Fatalf("majority of 4 is 3, got %d", l.r.Needed())
	}
	// A hijacked manager can't kick the others to lower the bar.
	rm = opOf(c, opRemove)
	rm.Key = a.hex
	l.propose(rm, false, c)
	// Nor make a reader of a channel.
	acc := opOf(c, opAccess)
	acc.Board, acc.Add = "secret", []string{c.hex}
	l.propose(acc, false, c)
	l.propose(acc, true, c, a, b)
	if !l.r.CanRead(c.hex, "secret") {
		t.Fatal("c should read secret now")
	}
	// Taking access away is one manager's call.
	acc = opOf(b, opAccess)
	acc.Board, acc.Remove = "secret", []string{c.hex}
	l.propose(acc, true, b)
	if l.r.CanRead(c.hex, "secret") {
		t.Fatal("c shouldn't read secret any more")
	}
	// …but a plain member can't.
	acc = opOf(eve, opAccess)
	acc.Board, acc.All = "general", fals()
	l.propose(acc, false, eve)
}

func TestRosterInvites(t *testing.T) {
	owner, a, joiner, thief := newRK(), newRK(), newRK(), newRK()
	l := startLog(t, owner, RosterPolicy{Approvers: "managers", Need: "n", N: 2},
		[]RosterMember{member(owner), member(a)}, []string{a.hex},
		[]RosterAccess{{Board: "general", All: true}, {Board: "games", Readers: []string{owner.hex}}})
	seed, invKey := NewInviteSecret()
	inv := opOf(owner, opInvite)
	inv.Invite = &RosterInvite{Key: invKey, Uses: 1, Boards: []string{"games"}}
	l.propose(inv, false, owner) // needs two
	inv = opOf(owner, opInvite)
	inv.Invite = &RosterInvite{Key: invKey, Uses: 1, Boards: []string{"games"}}
	l.propose(inv, true, owner, a)

	// The server knows the invite's public key, not its secret: a join it
	// makes up is refused.
	fake := opOf(thief, opJoin)
	fake.Key = invKey
	l.propose(fake, false, thief)
	// With the link's secret: in, with the invite's channels.
	e, err := JoinEntry(l.r.ID, seed, joiner.priv, "Jo")
	if err != nil {
		t.Fatal(err)
	}
	l.add(e, true)
	if !l.r.CanRead(joiner.hex, "general") || !l.r.CanRead(joiner.hex, "games") || l.r.Members[joiner.hex].Name != "Jo" {
		t.Fatal("joiner's access is wrong")
	}
	// One use only: a second person with the same link is refused.
	e, _ = JoinEntry(l.r.ID, seed, thief.priv, "Thief")
	l.add(e, false)

	// Expired and revoked invites.
	seed2, key2 := NewInviteSecret()
	inv = opOf(owner, opInvite)
	inv.Invite = &RosterInvite{Key: key2, Expires: 1000}
	l.propose(inv, true, owner, a)
	e, _ = JoinEntry(l.r.ID, seed2, thief.priv, "Late")
	l.add(e, false) // its At is now, long after 1000
	seed3, key3 := NewInviteSecret()
	inv = opOf(owner, opInvite)
	inv.Invite = &RosterInvite{Key: key3}
	l.propose(inv, true, owner, a)
	un := opOf(a, opUninvite)
	un.Key = key3
	l.propose(un, true, a)
	e, _ = JoinEntry(l.r.ID, seed3, thief.priv, "Nope")
	l.add(e, false)
	if _, in := l.r.Members[thief.hex]; in {
		t.Fatal("the thief got in")
	}
}

func TestRosterBotsAndPlugins(t *testing.T) {
	owner, bot, plug := newRK(), newRK(), newRK()
	l := startLog(t, owner, RosterPolicy{Approvers: "owner", Need: "1"}, nil, nil,
		[]RosterAccess{{Board: "general", All: true}, {Board: "music", All: true}})
	add := opOf(owner, opAdd)
	add.Key, add.Kind, add.Name = plug.hex, kindPlugin, "Music"
	l.propose(add, true, owner)
	add = opOf(owner, opAdd)
	add.Key, add.Kind, add.Boards = bot.hex, kindBot, []string{"music"}
	l.propose(add, true, owner)
	// "Everyone's" channels don't include bots or plugins.
	if l.r.CanRead(plug.hex, "general") || l.r.CanRead(bot.hex, "general") || !l.r.CanRead(bot.hex, "music") || l.r.CanRead(plug.hex, "music") {
		t.Fatal("bots and plugins read only what they're given")
	}
	if got := l.r.Readers("general"); len(got) != 1 || got[0] != owner.hex {
		t.Fatalf("readers %v", got)
	}
	// A plugin can't be a manager.
	m := opOf(owner, opManager)
	m.Key, m.On = plug.hex, tru()
	l.propose(m, false, owner)
}

func TestRosterTampering(t *testing.T) {
	owner, a, eve := newRK(), newRK(), newRK()
	l := startLog(t, owner, RosterPolicy{Approvers: "owner", Need: "1"}, []RosterMember{member(a)}, nil,
		[]RosterAccess{{Board: "general", All: true}})
	add := opOf(owner, opAdd)
	add.Key, add.Kind, add.Roster = a.hex+"", kindMember, l.r.ID
	good, _ := makeRosterEntry(add, owner.priv)
	// The body changed after signing (eve swapped in): the signature fails.
	bad := good
	bad.Body = strings.Replace(good.Body, a.hex, eve.hex, 1)
	l.add(bad, false)
	// Signed for another server's list.
	other := opOf(owner, opAdd)
	other.Key, other.Kind, other.Roster = eve.hex, kindMember, strings.Repeat("0", 32)
	e, _ := makeRosterEntry(other, owner.priv)
	l.add(e, false)
	// Fields the format doesn't have.
	e = RosterEntry{Body: `{"v":1,"roster":"` + l.r.ID + `","id":"` + strings.Repeat("1", 32) + `","at":1,"by":"` + owner.hex + `","op":"add","key":"` + eve.hex + `","kind":"member","admin":true}`}
	e.sign(owner.priv)
	l.add(e, false)
	// Junk among good entries doesn't stop the rest.
	ent := append([]RosterEntry{}, l.entries...)
	ent = append(ent, RosterEntry{Body: "garbage"})
	add = opOf(owner, opAdd)
	add.Key, add.Kind, add.Roster = eve.hex, kindMember, l.r.ID
	e, _ = makeRosterEntry(add, owner.priv)
	ent = append(ent, e)
	r, err := BuildRoster(ent)
	if err != nil || !r.CanRead(eve.hex, "general") || len(r.Rejected) < 4 {
		t.Fatalf("rebuild: %v, rejected %d", err, len(r.Rejected))
	}
	// A start not signed by its owner.
	op := newRosterOp("", owner.hex, opStart)
	op.Policy = &RosterPolicy{Approvers: "owner", Need: "1"}
	s, _ := makeRosterEntry(op, eve.priv)
	if _, err := BuildRoster([]RosterEntry{s}); err == nil {
		t.Fatal("a start signed by someone else")
	}
	// The order matters to the head (a server reordering is noticed).
	l2 := startLog(t, owner, RosterPolicy{Approvers: "owner", Need: "1"}, nil, nil, nil)
	for _, k := range []rk{a, eve} {
		o := opOf(owner, opAdd)
		o.Key, o.Kind = k.hex, kindMember
		l2.propose(o, true, owner)
	}
	r2, _ := BuildRoster([]RosterEntry{l2.entries[0], l2.entries[2], l2.entries[1]})
	if r2.Head == l2.r.Head || len(r2.Members) != len(l2.r.Members) {
		t.Fatal("reordering should change the head, and nothing else")
	}
}

func TestRosterConsider(t *testing.T) {
	owner, a, b, eve := newRK(), newRK(), newRK(), newRK()
	l := startLog(t, owner, RosterPolicy{Approvers: "managers", Need: "majority"},
		[]RosterMember{member(owner), member(a), member(b)}, []string{a.hex, b.hex}, []RosterAccess{{Board: "general", All: true}})
	add := opOf(a, opAdd)
	add.Key, add.Kind, add.Roster = eve.hex, kindMember, l.r.ID
	e, _ := makeRosterEntry(add, a.priv)
	c := l.r.Consider(e)
	if c.Ready || c.Approvals != 1 || c.Needed != 2 {
		t.Fatalf("%+v", c)
	}
	e.sign(b.priv)
	if c = l.r.Consider(e); !c.Ready || c.Approvals != 2 {
		t.Fatalf("%+v", c)
	}
	// Considering doesn't change the list.
	if _, in := l.r.Members[eve.hex]; in {
		t.Fatal("considering applied it")
	}
	l.add(e, true)
}

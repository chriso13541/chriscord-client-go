package main

// roster_app_test.go — what the app remembers of a server's signed member
// list (written whole or not at all; a rolled-back or altered list isn't
// trusted), and when a message's list head counts as "a different list".

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func isolateConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AppData", t.TempDir())
}

// twoEntries: a list started by owner, then alice added.
func twoEntries(t *testing.T) (owner, alice rk, entries []RosterEntry) {
	owner, alice = newRK(), newRK()
	l := startLog(t, owner, RosterPolicy{Approvers: "owner", Need: "1"}, nil, nil, []RosterAccess{{Board: "general", All: true}})
	add := opOf(owner, opAdd)
	add.Key, add.Kind = alice.hex, kindMember
	l.propose(add, true, owner)
	return owner, alice, l.entries
}

func freshClient(scope string) *rosterClient {
	a := &App{}
	rc := newRosterClient(a)
	a.roster = rc
	rc.scope, rc.state = scope, rosterLoading
	rc.heads, rc.forks = map[string]bool{}, map[string]string{}
	rc.pin = loadRosterPin(a.rosterPinPath(), scope)
	return rc
}

func TestRosterPinRollbackAndAlteration(t *testing.T) {
	isolateConfig(t)
	owner, alice, entries := twoEntries(t)
	rc := freshClient("test|me")
	rc.mu.Lock()
	rc.integrateLocked(entries, 2)
	rc.mu.Unlock()
	if rc.state != rosterActive || rc.pin.Head == "" {
		t.Fatalf("state %s, pin %+v", rc.state, rc.pin)
	}

	// Restarted; the server now has only the first entry.
	rc = freshClient("test|me")
	rc.mu.Lock()
	rc.integrateLocked(entries[:1], 1)
	rc.mu.Unlock()
	if rc.state != rosterReplaced || !strings.Contains(rc.problem, "missing changes") {
		t.Fatalf("rolled back list taken as %s (%s)", rc.state, rc.problem)
	}

	// Or as many entries, but a different second one.
	other := newRosterOp(entries[0].mustOp(t).ID, owner.hex, opAdd)
	other.Key, other.Kind = newRK().hex, kindMember
	e1, _ := makeRosterEntry(other, owner.priv)
	rc = freshClient("test|me")
	rc.mu.Lock()
	rc.integrateLocked([]RosterEntry{entries[0], e1}, 2)
	rc.mu.Unlock()
	if rc.state != rosterReplaced || !strings.Contains(rc.problem, "altered") {
		t.Fatalf("altered list taken as %s (%s)", rc.state, rc.problem)
	}

	// The real one again: fine.
	rc = freshClient("test|me")
	rc.mu.Lock()
	rc.integrateLocked(entries, 2)
	rc.mu.Unlock()
	if rc.state != rosterActive || !rc.r.CanRead(alice.hex, "general") {
		t.Fatalf("state %s (%s)", rc.state, rc.problem)
	}
}

func (e RosterEntry) mustOp(t *testing.T) RosterOp {
	op, err := e.op()
	if err != nil {
		t.Fatal(err)
	}
	return op
}

// A damaged pins file is moved aside, not read as "no servers" and then
// written over (which forgot every server's list), and saving leaves no
// temporary files.
func TestRosterPinsFileDamaged(t *testing.T) {
	isolateConfig(t)
	path := (&App{}).rosterPinPath()
	saveRosterPin(path, "a|1", &rosterPin{ID: "x", Applied: 3})
	if p := loadRosterPin(path, "a|1"); p == nil || p.Applied != 3 {
		t.Fatalf("round trip: %+v", p)
	}
	os.WriteFile(path, []byte(`{"a|1": {"id": "x", "appl`), 0600)
	if p := loadRosterPin(path, "a|1"); p != nil {
		t.Fatalf("read a damaged file as %+v", p)
	}
	saveRosterPin(path, "b|2", &rosterPin{ID: "y", Applied: 1})
	names, _ := os.ReadDir(filepath.Dir(path))
	var corrupt, tmp int
	for _, n := range names {
		switch {
		case strings.Contains(n.Name(), ".corrupt-"):
			corrupt++
		case strings.Contains(n.Name(), ".tmp-"):
			tmp++
		}
	}
	data, _ := os.ReadFile(path)
	var all map[string]*rosterPin
	if corrupt != 1 || tmp != 0 || json.Unmarshal(data, &all) != nil || all["b|2"] == nil {
		t.Fatalf("corrupt %d, tmp %d, file %s", corrupt, tmp, data)
	}
}

// A head this app didn't have yet stops counting as a fork once the list
// catches up; one from before the list started never counts.
func TestRosterForksSettle(t *testing.T) {
	isolateConfig(t)
	owner, _, entries := twoEntries(t)
	rc := freshClient("test|me")
	rc.mu.Lock()
	rc.integrateLocked(entries[:1], 1)
	rc.mu.Unlock()

	// A message from before the list started, with an old list's head.
	old := time.Unix(rc.r.StartedAt-3600, 0).UTC().Format(time.RFC3339)
	if sentDuring(rc.r, old) {
		t.Fatal("a message from an hour before the list started counted")
	}
	now := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	if !sentDuring(rc.r, now) {
		t.Fatal("a message sent while the list was running didn't count")
	}

	// Someone sent a message right after the second entry; this app saw
	// the message before the change.
	full, _ := BuildRoster(entries)
	h := headPrefix(full.Head)
	rc.mu.Lock()
	rc.forks["owner"] = h
	rc.integrateLocked(entries[1:], 2)
	_, still := rc.forks["owner"]
	rc.mu.Unlock()
	if still {
		t.Fatal("owner still marked as on a different list after catching up")
	}
	_ = owner
}

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeKeyServer does what channel_keys.rs does, in memory, so the whole
// exchange between several apps can be tested here.
type fakeKeyServer struct {
	mu      sync.Mutex
	users   map[string]*App                       // username → their app
	enc     map[string][2]string                  // identity key → (enc key, sig)
	access  map[string]map[string]bool            // board → who can see it (missing board: everyone)
	gens    map[string]int                        // board → latest generation
	boxes   map[string]map[int]map[string]fakeBox // board → gen → recipient → box
	failNew int                                   // make the next N new-generation requests conflict
}

type fakeBox struct{ sender, box string }

func newFakeKeyServer() *fakeKeyServer {
	return &fakeKeyServer{users: map[string]*App{}, enc: map[string][2]string{},
		access: map[string]map[string]bool{}, gens: map[string]int{}, boxes: map[string]map[int]map[string]fakeBox{}}
}

func (s *fakeKeyServer) add(t *testing.T, name string) *App {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	a := &App{username: name, domain: "test", account: &Account{PublicKey: pub, PrivateKey: priv}}
	a.text = newTextCrypto(a)
	a.text.call = func(method, path string, body, out interface{}) error {
		return s.handle(name, method, path, body, out)
	}
	s.mu.Lock()
	s.users[name] = a
	s.mu.Unlock()
	return a
}

func (s *fakeKeyServer) idOf(name string) string {
	return hex.EncodeToString(s.users[name].account.PublicKey)
}

func (s *fakeKeyServer) sees(name, board string) bool {
	set, ok := s.access[board]
	return !ok || set[name]
}

func (s *fakeKeyServer) member(name string) map[string]interface{} {
	m := map[string]interface{}{"username": name, "public_key": s.idOf(name), "enc_key": nil, "sig": nil}
	if e, ok := s.enc[s.idOf(name)]; ok {
		m["enc_key"], m["sig"] = e[0], e[1]
	}
	return m
}

func (s *fakeKeyServer) handle(user, method, path string, body, out interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var in map[string]interface{}
	if body != nil {
		raw, _ := json.Marshal(body)
		json.Unmarshal(raw, &in)
	}
	me := s.idOf(user)
	var resp interface{} = map[string]interface{}{"ok": true}
	switch {
	case method == "PUT" && path == "/api/e2ee/enc-key":
		encB, _ := hex.DecodeString(in["enc_key"].(string))
		sigB, _ := hex.DecodeString(in["sig"].(string))
		if !ed25519.Verify(s.users[user].account.PublicKey, encKeyMessage(encB), sigB) {
			return &httpError{400, "bad signature"}
		}
		s.enc[me] = [2]string{in["enc_key"].(string), in["sig"].(string)}

	case strings.HasPrefix(path, "/api/boards/") && strings.HasSuffix(path, "/keys"):
		board := strings.TrimSuffix(strings.TrimPrefix(path, "/api/boards/"), "/keys")
		if !s.sees(user, board) {
			return &httpError{404, "Channel not found"}
		}
		if method == "GET" {
			latest := s.gens[board]
			members := []interface{}{}
			memberKeys := map[string]bool{}
			for name := range s.users {
				if s.sees(name, board) {
					members = append(members, s.member(name))
					memberKeys[s.idOf(name)] = true
				}
			}
			rotate := false
			for r := range s.boxes[board][latest] {
				if !memberKeys[r] {
					rotate = true
				}
			}
			boxes := []interface{}{}
			for g := 1; g <= latest; g++ {
				if b, ok := s.boxes[board][g][me]; ok {
					sender := map[string]interface{}{"username": "", "public_key": b.sender, "enc_key": s.enc[b.sender][0], "sig": s.enc[b.sender][1]}
					boxes = append(boxes, map[string]interface{}{"generation": g, "box": b.box, "sender": sender})
				}
			}
			resp = map[string]interface{}{"latest": latest, "rotate": rotate, "boxes": boxes, "members": members}
		} else {
			gen := int(in["generation"].(float64))
			if s.failNew > 0 {
				s.failNew--
				return &httpError{http.StatusConflict, "conflict"}
			}
			if gen != s.gens[board]+1 {
				return &httpError{http.StatusConflict, "conflict"}
			}
			s.gens[board] = gen
			if s.boxes[board] == nil {
				s.boxes[board] = map[int]map[string]fakeBox{}
			}
			s.boxes[board][gen] = map[string]fakeBox{}
			for _, x := range in["boxes"].([]interface{}) {
				bx := x.(map[string]interface{})
				r := bx["recipient"].(string)
				for name := range s.users {
					if s.idOf(name) == r && s.sees(name, board) {
						if _, ok := s.enc[r]; ok {
							s.boxes[board][gen][r] = fakeBox{me, bx["box"].(string)}
						}
					}
				}
			}
		}

	case method == "GET" && path == "/api/e2ee/pending":
		pending := []interface{}{}
		for board, gens := range s.boxes {
			if !s.sees(user, board) {
				continue
			}
			for g, rs := range gens {
				if _, mine := rs[me]; !mine {
					continue
				}
				for name := range s.users {
					id := s.idOf(name)
					if _, has := rs[id]; has || !s.sees(name, board) {
						continue
					}
					if _, ok := s.enc[id]; !ok {
						continue
					}
					pending = append(pending, map[string]interface{}{"board_id": board, "generation": g, "recipient": s.member(name)})
				}
			}
		}
		resp = map[string]interface{}{"pending": pending}

	case method == "POST" && path == "/api/e2ee/boxes":
		for _, x := range in["boxes"].([]interface{}) {
			bx := x.(map[string]interface{})
			board, g, r := bx["board_id"].(string), int(bx["generation"].(float64)), bx["recipient"].(string)
			if _, mine := s.boxes[board][g][me]; !mine {
				continue
			}
			if _, has := s.boxes[board][g][r]; !has {
				s.boxes[board][g][r] = fakeBox{me, bx["box"].(string)}
			}
		}

	default:
		return fmt.Errorf("fake server: no %s %s", method, path)
	}
	if out != nil {
		raw, _ := json.Marshal(resp)
		return json.Unmarshal(raw, out)
	}
	return nil
}

// deliver is the server storing a message and someone's app receiving it.
func deliver(to *App, board, from, content string) ChatMessage {
	m := ChatMessage{ID: fmt.Sprintf("m%d", time.Now().UnixNano()), BoardID: board, Username: from, Content: content}
	to.text.open(&m)
	return m
}

func textTestEnv(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AppData", t.TempDir())
}

func mustSeal(t *testing.T, a *App, board, text string) string {
	t.Helper()
	c, err := a.text.seal(board, text)
	if err != nil {
		t.Fatalf("%s couldn't send: %v", a.username, err)
	}
	if !strings.HasPrefix(c, textPrefix) || strings.Contains(c, text) {
		t.Fatalf("not encrypted: %q", c)
	}
	return c
}

func expectText(t *testing.T, m ChatMessage, want, state string) {
	t.Helper()
	if m.Content != want || m.E2EE != state {
		t.Fatalf("got %q (%s), want %q (%s)", m.Content, m.E2EE, want, state)
	}
}

func TestTextHistoryRotationAndForgery(t *testing.T) {
	textTestEnv(t)
	s := newFakeKeyServer()
	alice, bob := s.add(t, "alice"), s.add(t, "bob")
	s.access["b1"] = map[string]bool{"alice": true, "bob": true}

	// Alice is first: the channel gets its first key. Bob's app hasn't
	// published a key yet (say he hasn't updated), so only Alice has it.
	first := mustSeal(t, alice, "b1", "hello from before bob updated")
	expectText(t, deliver(bob, "b1", "alice", first), textLockedMsg, "locked")

	// Bob updates (publishes his key); Alice's app shares the key with him.
	bob.text.ensurePublished()
	alice.text.runPending()
	bob.text.mu.Lock()
	bob.text.triedAt = map[string]time.Time{}
	bob.text.mu.Unlock()
	expectText(t, deliver(bob, "b1", "alice", first), "hello from before bob updated", "ok")

	// Carol joins the channel later and gets the whole history.
	carol := s.add(t, "carol")
	carol.text.ensurePublished()
	s.access["b1"]["carol"] = true
	bob.text.runPending()
	expectText(t, deliver(carol, "b1", "alice", first), "hello from before bob updated", "ok")
	reply := mustSeal(t, carol, "b1", "hi both")
	expectText(t, deliver(alice, "b1", "carol", reply), "hi both", "ok")

	// Carol loses access: the next message is under a new key she never gets.
	carolHad := map[int][]byte{}
	carol.text.mu.Lock()
	for g, k := range carol.text.boards["b1"].keys {
		carolHad[g] = k
	}
	carol.text.mu.Unlock()
	delete(s.access["b1"], "carol")
	after := mustSeal(t, bob, "b1", "carol can't read this")
	gen, blob, _ := parseText(after)
	if gen != 2 {
		t.Fatalf("expected a new generation after carol left, got %d", gen)
	}
	for g, k := range carolHad {
		if _, err := openText(k, "b1", g, blob); err == nil {
			t.Fatal("someone who lost access could read a new message")
		}
	}
	expectText(t, deliver(alice, "b1", "bob", after), "carol can't read this", "ok")
	// Old messages still open for those who stayed.
	expectText(t, deliver(alice, "b1", "alice", first), "hello from before bob updated", "ok")

	// The server can't pass one person's message off as another's.
	if m := deliver(alice, "b1", "carol", after); m.E2EE != "forged" {
		t.Fatalf("misattributed message not caught: %+v", m)
	}

	// Edits: encrypted with the newest key, and checked against the author.
	m := deliver(alice, "b1", "bob", after)
	edited := mustSeal(t, bob, "b1", "edited")
	if c, st := alice.text.openEdit(m.ID, "b1", edited); c != "edited" || st != "ok" {
		t.Fatalf("edit: %q %s", c, st)
	}

	// Plain (old, unencrypted) messages pass through untouched.
	expectText(t, deliver(alice, "b1", "bob", "plain old message"), "plain old message", "")
}

func TestTextNewGenerationRace(t *testing.T) {
	textTestEnv(t)
	s := newFakeKeyServer()
	alice, bob := s.add(t, "alice"), s.add(t, "bob")
	alice.text.ensurePublished()
	bob.text.ensurePublished()
	// Alice starts the channel's key; Bob uses the same one.
	mustSeal(t, alice, "b2", "first")
	c := mustSeal(t, bob, "b2", "second")
	expectText(t, deliver(alice, "b2", "bob", c), "second", "ok")

	// A conflict (someone else just made the next generation) is retried.
	s.access["b2"] = map[string]bool{"alice": true} // bob loses access → rotation needed
	s.failNew = 1
	c2 := mustSeal(t, alice, "b2", "after conflict")
	if g, _, _ := parseText(c2); g != 2 {
		t.Fatalf("generation %d", g)
	}
}

func TestTextEnvelope(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	c, _ := sealText(key, "b", 3, "alice", "hi")
	g, blob, ok := parseText(c)
	if !ok || g != 3 {
		t.Fatal("parse")
	}
	if _, err := openText(key, "other-board", 3, blob); err == nil {
		t.Fatal("opened under another channel")
	}
	if _, err := openText(key, "b", 4, blob); err == nil {
		t.Fatal("opened under another generation")
	}
	for _, plain := range []string{"hello", "e2ee:1:", "e2ee:1:x:abc", "e2ee:1:0:AAAA"} {
		if _, _, ok := parseText(plain); ok {
			t.Fatalf("%q parsed as encrypted", plain)
		}
	}
	// The same account gets the same encryption key on every device.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	a1, p1 := encKeyFromSeed(priv.Seed())
	a2, p2 := encKeyFromSeed(append([]byte(nil), priv.Seed()...))
	if a1 != a2 || p1 != p2 {
		t.Fatal("encryption key isn't deterministic")
	}
}

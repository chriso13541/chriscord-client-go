package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// histServer stands in for a server with /api/search/scan (all of it:
// cursor, since, ids, count), /api/search/changes and /api/search/authors.
type histServer struct {
	mu      sync.Mutex
	msgs    map[string]SearchResult
	changes []map[string]interface{} // change n is changes[n-1]
	hidden  map[string]bool          // channels you can't read
	scans   int
	paged   int // scans going further back (cursor), not counting by-id ones
	srv     *httptest.Server
}

func newHistServer(t *testing.T) *histServer {
	s := &histServer{msgs: map[string]SearchResult{}, hidden: map[string]bool{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

func histMsg(n int, board, user, text string) SearchResult {
	at := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Hour)
	return SearchResult{ChatMessage: ChatMessage{ID: fmt.Sprintf("m%07d", n), BoardID: board, Username: user,
		Content: text, CreatedAt: at.Format(time.RFC3339)}, BoardName: board}
}

func (s *histServer) add(ms ...SearchResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range ms {
		s.msgs[m.ID] = m
	}
}

func (s *histServer) note(c map[string]interface{}) {
	c["seq"] = len(s.changes) + 1
	s.changes = append(s.changes, c)
}

func (s *histServer) boards() []string {
	set := map[string]bool{}
	for _, m := range s.msgs {
		if !s.hidden[m.BoardID] {
			set[m.BoardID] = true
		}
	}
	var out []string
	for b := range set {
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

func (s *histServer) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := r.URL.Query()
	switch r.URL.Path {
	case "/api/search/scan":
		s.scans++
		if q.Get("cursor") != "" {
			s.paged++
		}
		var ids []string
		for id := range s.msgs {
			ids = append(ids, id)
		}
		since := q.Get("since")
		if since != "" {
			sort.Strings(ids)
		} else {
			sort.Sort(sort.Reverse(sort.StringSlice(ids)))
		}
		want := map[string]bool{}
		for _, id := range strings.Split(q.Get("ids"), ",") {
			if id != "" {
				want[id] = true
			}
		}
		after, before := parseMillis(q.Get("after")), parseMillis(q.Get("before"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		var out []SearchResult
		total := 0
		for _, id := range ids {
			m := s.msgs[id]
			t := parseMillis(m.CreatedAt)
			if s.hidden[m.BoardID] || (len(want) > 0 && !want[id]) ||
				(q.Get("from") != "" && !strings.EqualFold(m.Username, q.Get("from"))) ||
				(q.Get("in") != "" && m.BoardID != q.Get("in")) ||
				(q.Get("has") != "" && q.Get("has") != "link" && len(m.Attachments) == 0) ||
				(after != 0 && t < after) || (before != 0 && t >= before) {
				continue
			}
			total++
			if (since != "" && id <= since) || (q.Get("cursor") != "" && id >= q.Get("cursor")) {
				continue
			}
			if len(out) < limit {
				out = append(out, m)
			}
		}
		resp := map[string]interface{}{"messages": out, "next": nil}
		if len(out) == limit {
			resp["next"] = out[len(out)-1].ID
		}
		if q.Get("count") == "1" {
			resp["total"] = total
		}
		json.NewEncoder(w).Encode(resp)
	case "/api/search/changes":
		resp := map[string]interface{}{"changes": []interface{}{}, "last": len(s.changes), "more": false, "reset": false, "boards": s.boards()}
		if a := q.Get("after"); a != "" {
			n, _ := strconv.Atoi(a)
			var out []interface{}
			for _, c := range s.changes[n:] {
				c2 := map[string]interface{}{}
				for k, v := range c {
					c2[k] = v
				}
				if c["kind"] == "edit" {
					if m, ok := s.msgs[c["id"].(string)]; ok {
						c2["message"] = m
					}
				}
				out = append(out, c2)
			}
			if out != nil {
				resp["changes"] = out
			}
		}
		json.NewEncoder(w).Encode(resp)
	case "/api/search/authors":
		count := map[string]int{}
		for _, m := range s.msgs {
			count[m.Username]++
		}
		var out []SearchAuthor
		for u, n := range count {
			out = append(out, SearchAuthor{Username: u, Messages: n})
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"authors": out})
	default:
		http.NotFound(w, r)
	}
}

func histApp(t *testing.T, s *histServer, priv ed25519.PrivateKey) *App {
	a := &App{domain: s.srv.URL, token: "t", account: &Account{Slug: "acct", PublicKey: priv.Public().(ed25519.PublicKey), PrivateKey: priv}}
	a.text = newTextCrypto(a)
	// Each app starts without anything another left in memory.
	histMu.Lock()
	histCur = nil
	unsupported = map[string]bool{}
	histMu.Unlock()
	a.ClearSearch()
	return a
}

// searchAll runs a search to the end, as the page does.
func searchAll(t *testing.T, a *App, text, from, has, in, after, before string) ([]string, *SearchPage) {
	t.Helper()
	var ids []string
	cont := ""
	for i := 0; ; i++ {
		p, err := a.SearchMessages(text, from, has, in, after, before, cont, 25)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range p.Results {
			ids = append(ids, r.ID)
		}
		if p.Next == "" {
			return ids, p
		}
		cont = p.Next
		if i > 10000 {
			t.Fatal("never finished")
		}
	}
}

// brute is what a search should find: newest first.
func (s *histServer) brute(match func(m SearchResult) bool) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for id, m := range s.msgs {
		if !s.hidden[m.BoardID] && match(m) {
			out = append(out, id)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

func sameIDs(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s: got %d %v…, want %d %v…", what, len(got), head(got), len(want), head(want))
	}
}

func head(s []string) []string { return s[:min(len(s), 6)] }

func TestSearchCopy(t *testing.T) {
	textTestEnv(t)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s := newHistServer(t)
	users := []string{"ada", "bob", "cy"}
	for i := 0; i < 4500; i++ {
		m := histMsg(i, []string{"general", "random"}[i%2], users[i%3], fmt.Sprintf("message number %d", i))
		if i == 17 {
			m.Content = "the hidden needle is here"
		}
		if i%50 == 0 {
			m.Attachments = []Attachment{{URL: "/api/files/p.png", Name: fmt.Sprintf("photo%d.png", i), Mime: "image/png"}}
		}
		s.add(m)
	}
	a := histApp(t, s, priv)

	// Words only: going back grows the copy until it's found.
	ids, p := searchAll(t, a, "needle", "", "", "", "", "")
	sameIDs(t, "needle", ids, []string{"m0000017"})
	if !p.Copy || p.Searched != 4500 || p.Total != 4500 {
		t.Fatalf("progress: %+v", p)
	}
	info := a.SearchCopyInfo()
	if !info.Enabled || !info.Complete || info.Messages != 4500 {
		t.Fatalf("copy: %+v", info)
	}
	path, _, _ := a.copyPath()
	raw, err := os.ReadFile(path)
	if err != nil || bytes.Contains(raw, []byte("needle")) || bytes.Contains(raw, []byte("message")) {
		t.Fatalf("copy on disk isn't encrypted (or missing): %v", err)
	}

	// Another app with the same account: the copy is read, not fetched.
	a2 := histApp(t, s, priv)
	s.mu.Lock()
	s.scans = 0
	s.mu.Unlock()
	ids, _ = searchAll(t, a2, "needle", "", "", "", "", "")
	sameIDs(t, "needle again", ids, []string{"m0000017"})
	s.mu.Lock()
	scans := s.scans
	s.mu.Unlock()
	if scans > 3 { // new messages (none), the count, and the match itself
		t.Fatalf("%d scans with a complete copy", scans)
	}
	// Another account can't open it.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	h, _ := newLocalHistory(path, mustKey(t, other, s.srv.URL))
	h.mu.Lock()
	h.load()
	n := len(h.recs)
	h.mu.Unlock()
	if n != 0 {
		t.Fatal("another key opened the copy")
	}

	// Filters, against working it out by hand.
	ids, _ = searchAll(t, a2, "", "bob", "image", "", "", "")
	sameIDs(t, "from:bob has:image", ids, s.brute(func(m SearchResult) bool { return m.Username == "bob" && len(m.Attachments) > 0 }))
	ids, _ = searchAll(t, a2, "number", "", "", "random", "2020-02-01T00:00:00Z", "2020-03-01T00:00:00Z")
	sameIDs(t, "in:random dates", ids, s.brute(func(m SearchResult) bool {
		t := parseMillis(m.CreatedAt)
		return m.BoardID == "random" && t >= parseMillis("2020-02-01T00:00:00Z") && t < parseMillis("2020-03-01T00:00:00Z")
	}))

	// Changes while away: an edit, a deletion, new messages, history
	// imported into the past, an imported person claimed, a channel hidden.
	s.mu.Lock()
	m := s.msgs["m0000100"]
	m.Content = "edited to say walrus"
	s.msgs[m.ID] = m
	s.note(map[string]interface{}{"kind": "edit", "id": m.ID, "board_id": m.BoardID})
	delete(s.msgs, "m0000017")
	s.note(map[string]interface{}{"kind": "delete", "id": "m0000017", "board_id": "general"})
	s.mu.Unlock()
	s.add(histMsg(5000, "general", "dee", "a new walrus"), histMsg(5001, "general", "ada", "another"))
	imp := histMsg(3, "general", "olddiscorduser", "imported walrus from long ago")
	imp.ID = "m0000003x" // between existing ones
	imp.ImportedFrom = "discord:1"
	s.add(imp)
	s.mu.Lock()
	s.note(map[string]interface{}{"kind": "board", "board_id": "general"})
	imp.Username = "claimer"
	s.msgs[imp.ID] = imp
	s.note(map[string]interface{}{"kind": "author", "imported_from": "discord:1", "username": "claimer"})
	s.mu.Unlock()

	a3 := histApp(t, s, priv)
	ids, _ = searchAll(t, a3, "walrus", "", "", "", "", "")
	sameIDs(t, "after changes", ids, []string{"m0005000", "m0000100", "m0000003x"})
	ids, _ = searchAll(t, a3, "needle", "", "", "", "", "")
	sameIDs(t, "deleted", ids, nil)
	ids, _ = searchAll(t, a3, "", "claimer", "", "", "", "")
	sameIDs(t, "claimed", ids, []string{"m0000003x"})

	s.mu.Lock()
	s.hidden["random"] = true
	s.mu.Unlock()
	ids, _ = searchAll(t, a3, "number", "", "", "", "", "")
	sameIDs(t, "hidden channel", ids, s.brute(func(m SearchResult) bool { return strings.Contains(m.Content, "number") }))

	// Who's ever written, for from:.
	authors, err := a3.SearchAuthors()
	if err != nil || len(authors) != 5 {
		t.Fatalf("authors: %v %+v", err, authors)
	}

	// Cleared: gone from disk, and searching starts a new one.
	if err := a3.ClearSearchCopy(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("copy still there after clearing")
	}
	if info := a3.SearchCopyInfo(); info.Messages != 0 {
		t.Fatalf("after clearing: %+v", info)
	}
	// Switched off: no copy made.
	if err := a3.SetSearchCopy(false); err != nil {
		t.Fatal(err)
	}
	ids, p = searchAll(t, a3, "walrus", "", "", "", "", "")
	sameIDs(t, "without a copy", ids, []string{"m0005000", "m0000100", "m0000003x"})
	if p.Copy {
		t.Fatal("used a copy while switched off")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("made a copy while switched off")
	}
	a3.SetSearchCopy(true)
}

func mustKey(t *testing.T, priv ed25519.PrivateKey, domain string) []byte {
	k, err := historyKey(priv.Seed(), domain)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// Filtered searches past the end of a partial copy go to the server, and
// together find exactly what they should.
func TestSearchPartialCopy(t *testing.T) {
	textTestEnv(t)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s := newHistServer(t)
	for i := 0; i < 3200; i++ {
		s.add(histMsg(i, "general", []string{"ada", "bob"}[i%2], fmt.Sprintf("message %d", i)))
	}
	a := histApp(t, s, priv)
	// A first search copies just the newest page.
	ids, p := searchAll(t, a, "", "ada", "", "", "", "")
	sameIDs(t, "from:ada", ids, s.brute(func(m SearchResult) bool { return m.Username == "ada" }))
	if info := a.SearchCopyInfo(); info.Complete || info.Messages != histPage {
		t.Fatalf("copy: %+v", info)
	}
	if p.Searched != 1600 || p.Total != 1600 {
		t.Fatalf("progress: %+v", p)
	}
	// Searching again with the same filters: nothing fetched twice.
	s.mu.Lock()
	s.paged = 0
	s.mu.Unlock()
	ids, _ = searchAll(t, a, "message", "ada", "", "", "", "")
	sameIDs(t, "refined", ids, s.brute(func(m SearchResult) bool { return m.Username == "ada" }))
	s.mu.Lock()
	paged := s.paged
	s.mu.Unlock()
	if paged != 0 {
		t.Fatalf("%d pages fetched again when refining", paged)
	}

	// Downloading all of it.
	if err := a.DownloadSearchCopy(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for a.SearchCopyInfo().Downloading {
		if time.Now().After(deadline) {
			t.Fatal("download never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if info := a.SearchCopyInfo(); !info.Complete || info.Messages != 3200 {
		t.Fatalf("after download: %+v", info)
	}
}

// A copy whose end was cut off (the app closed mid-write) keeps the rest.
func TestSearchCopyDamaged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.copy")
	key := make([]byte, 32)
	h, _ := newLocalHistory(path, key)
	h.mu.Lock()
	h.load()
	for i := 0; i < 3; i++ {
		var put []histRec
		for j := 0; j < 10; j++ {
			n := 1000 - i*10 - j
			put = append(put, recFrom(fmt.Sprintf("m%05d", n), "b", "u", "", fmt.Sprintf("word%d", n), nil))
		}
		st := histState{Head: "m01000", Tail: put[len(put)-1].ID, Boards: []string{"b"}}
		if err := h.commit(&histOp{Put: put, State: &st}); err != nil {
			t.Fatal(err)
		}
	}
	h.unload()
	h.mu.Unlock()
	fi, _ := os.Stat(path)
	os.Truncate(path, fi.Size()-5)

	h, _ = newLocalHistory(path, key)
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.load(); err != nil {
		t.Fatal(err)
	}
	if len(h.recs) != 20 || h.st.Tail != "m00981" || h.recs[0].ID != "m01000" {
		t.Fatalf("%d messages, tail %s", len(h.recs), h.st.Tail)
	}
	// And carries on from there.
	st := h.st
	if err := h.commit(&histOp{Put: []histRec{recFrom("m00500", "b", "u", "", "later", nil)}, State: &st}); err != nil {
		t.Fatal(err)
	}
	if err := h.compact(); err != nil {
		t.Fatal(err)
	}
	h.unload()
	if err := h.load(); err != nil || len(h.recs) != 21 || h.recs[20].ID != "m00500" {
		t.Fatalf("after compacting: %v, %d", err, len(h.recs))
	}
}

// Messages added out of order land in order, replacing ones with the same id.
func TestSearchCopyPut(t *testing.T) {
	h := &localHistory{intern: map[string]string{}}
	r := func(id string) histRec { return histRec{ID: id, B: "b", X: " " + id + " "} }
	h.put([]histRec{r("m5"), r("m4")})
	h.put([]histRec{r("m2"), r("m3")}) // older
	h.put([]histRec{r("m9"), r("m8")}) // newer
	h.put([]histRec{r("m6"), r("m4"), r("m1")})
	var got []string
	for _, x := range h.recs {
		got = append(got, x.ID)
	}
	if strings.Join(got, " ") != "m9 m8 m6 m5 m4 m3 m2 m1" {
		t.Fatal(got)
	}
	if h.after("m6") != 3 || h.after("") != 0 || h.after("m0") != 8 {
		t.Fatal("after")
	}
}

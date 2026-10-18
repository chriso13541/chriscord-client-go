package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// scanServer answers /api/search/scan like the server: newest first, by
// pages, with the from:/has-attachments filters applied.
func scanServer(t *testing.T, msgs []SearchResult) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search/scan" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		cursor := q.Get("cursor")
		var out []SearchResult
		for i := len(msgs) - 1; i >= 0; i-- { // msgs are oldest first
			m := msgs[i]
			if cursor != "" && m.ID >= cursor {
				continue
			}
			if f := q.Get("from"); f != "" && m.Username != f {
				continue
			}
			if q.Get("has") != "" && q.Get("has") != "link" && len(m.Attachments) == 0 {
				continue
			}
			out = append(out, m)
			if len(out) == limit {
				break
			}
		}
		resp := map[string]interface{}{"messages": out, "next": nil}
		if len(out) == limit {
			resp["next"] = out[len(out)-1].ID
		}
		json.NewEncoder(w).Encode(resp)
	}))
}

func TestSearchNewestFirstAndPaging(t *testing.T) {
	var msgs []SearchResult
	for i := 0; i < 1300; i++ {
		m := SearchResult{ChatMessage: ChatMessage{ID: fmt.Sprintf("id%05d", i), BoardID: "b", Username: "cascade",
			Content: fmt.Sprintf("message %d", i), CreatedAt: fmt.Sprintf("2026-01-01T00:%02d:00Z", i%60)}}
		if i%10 == 0 {
			m.Attachments = []Attachment{{URL: "/api/files/x.png", Name: fmt.Sprintf("pic%d.png", i), Mime: "image/png"}}
		}
		if i%3 == 0 {
			m.Username = "ubuntu"
		}
		msgs = append(msgs, m)
	}
	srv := scanServer(t, msgs)
	defer srv.Close()
	a := &App{domain: srv.URL}
	a.text = newTextCrypto(a)
	a.ClearSearch()

	// from:cascade has:image — newest first, 25 at a time.
	p, err := a.SearchMessages("", "cascade", "image", "", "", "", "", 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Results) != 25 || p.Results[0].ID != "id01280" || p.Results[1].ID != "id01270" {
		t.Fatalf("first page: %d results, starts %s", len(p.Results), p.Results[0].ID)
	}
	for _, r := range p.Results {
		if r.Username != "cascade" || r.Attachments[0].Mime != "image/png" {
			t.Fatalf("wrong match %+v", r)
		}
	}
	seen := len(p.Results)
	last := p.Results[len(p.Results)-1].ID
	for p.Next != "" {
		p, err = a.SearchMessages("", "cascade", "image", "", "", "", p.Next, 25)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range p.Results {
			if r.ID >= last {
				t.Fatalf("out of order: %s after %s", r.ID, last)
			}
			last = r.ID
		}
		seen += len(p.Results)
	}
	// i%10==0 and i%3!=0, for i < 1300.
	want := 0
	for i := 0; i < 1300; i += 10 {
		if i%3 != 0 {
			want++
		}
	}
	if seen != want {
		t.Fatalf("found %d, expected %d", seen, want)
	}

	// Words: whole words, and file names count.
	p, _ = a.SearchMessages("pic120", "", "", "", "", "", "", 25)
	if len(p.Results) != 1 || p.Results[0].ID != "id00120" {
		t.Fatalf("file name search: %+v", p.Results)
	}
	p, _ = a.SearchMessages("message 7", "", "", "", "", "", "", 25)
	if len(p.Results) != 1 || p.Results[0].ID != "id00007" {
		t.Fatalf("word search: %d results", len(p.Results))
	}
}

func TestSearchMatching(t *testing.T) {
	m := &SearchResult{ChatMessage: ChatMessage{Content: "Café crème at https://example.com — don't forget",
		Attachments: []Attachment{{Name: "Holiday Clip.MP4", Mime: "video/mp4"}}}}
	for _, c := range []struct {
		q    string
		has  []string
		want bool
	}{
		{"cafe", nil, true}, {"CRÈME", nil, true}, {"caf", nil, false}, {`"cafe creme"`, nil, true},
		{`"creme cafe"`, nil, false}, {"holiday mp4", nil, true}, {"don", nil, true}, {"", []string{"link"}, true},
		{"", []string{"video"}, true}, {"", []string{"image"}, false}, {"forget", []string{"file", "link"}, true},
	} {
		if got := matchesSearch(m, parseQuery(c.q), c.has); got != c.want {
			t.Errorf("%q %v: %v", c.q, c.has, got)
		}
	}
	locked := &SearchResult{ChatMessage: ChatMessage{Content: textLockedMsg, E2EE: "locked"}}
	if matchesSearch(locked, parseQuery("encrypted"), nil) {
		t.Error("matched a message that couldn't be read")
	}
}

// Encrypted messages are decrypted, then matched.
func TestSearchEncrypted(t *testing.T) {
	textTestEnv(t)
	s := newFakeKeyServer()
	alice := s.add(t, "alice")
	alice.text.ensurePublished()
	c1 := mustSeal(t, alice, "b1", "the secret meeting is tuesday")
	c2 := mustSeal(t, alice, "b1", "nothing to see")
	msgs := []SearchResult{
		{ChatMessage: ChatMessage{ID: "a1", BoardID: "b1", Username: "alice", Content: c1}},
		{ChatMessage: ChatMessage{ID: "a2", BoardID: "b1", Username: "alice", Content: c2}},
	}
	srv := scanServer(t, msgs)
	defer srv.Close()
	alice.domain = srv.URL
	alice.ClearSearch()
	p, err := alice.SearchMessages("meeting", "", "", "", "", "", "", 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Results) != 1 || p.Results[0].Content != "the secret meeting is tuesday" || p.Next != "" {
		t.Fatalf("%+v", p)
	}
}

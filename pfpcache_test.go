package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// pfpTestServer answers /api/pfps and counts picture downloads.
type pfpTestServer struct {
	mu        sync.Mutex
	versions  map[string]int64 // username → picture version
	pics      map[string][]byte
	downloads int
	indexHits int
	noIndex   bool // an older server
}

func (s *pfpTestServer) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.URL.Path == "/api/pfps":
		if s.noIndex {
			http.NotFound(w, r)
			return
		}
		s.indexHits++
		users := []map[string]interface{}{}
		for u, v := range s.versions {
			users = append(users, map[string]interface{}{"username": u, "key": strings.Repeat("ab", 32)[:62] + u[:2], "pfp": v, "banner": 0})
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"users": users, "imported": []interface{}{}})
	case strings.HasPrefix(r.URL.Path, "/api/pfp/"):
		u := strings.TrimPrefix(r.URL.Path, "/api/pfp/")
		pic, ok := s.pics[u]
		if !ok {
			http.NotFound(w, r)
			return
		}
		s.downloads++
		w.Write(pic)
	default:
		http.NotFound(w, r)
	}
}

func pfpTestApp(t *testing.T, srv *httptest.Server) *App {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LocalAppData", t.TempDir())
	pfpIdx = &pfpIndexState{} // a fresh start
	return &App{domain: srv.URL, token: "tok"}
}

// what fetchUserPfpBytes does, without the parts of app.go this test
// can't reach.
func pfpTestFetch(a *App, s *pfpTestServer, user string) []byte {
	e, known, supported := a.pfpLookup(user)
	net := func() ([]byte, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.downloads++
		return s.pics[user], nil
	}
	if !supported {
		b, _ := net()
		return b
	}
	if !known || e.pfp == 0 {
		return nil
	}
	b, _ := cachedImage("pfp", e.id, e.pfp, net)
	return b
}

func TestPfpCacheFetchesOnlyChanges(t *testing.T) {
	s := &pfpTestServer{versions: map[string]int64{"alice": 100, "bob": 0}, pics: map[string][]byte{"alice": []byte("alice-v1")}}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()
	a := pfpTestApp(t, srv)

	if got := pfpTestFetch(a, s, "alice"); !bytes.Equal(got, []byte("alice-v1")) {
		t.Fatalf("got %q", got)
	}
	if got := pfpTestFetch(a, s, "bob"); got != nil || s.downloads != 1 {
		t.Fatalf("bob has no picture: asked anyway (%d downloads)", s.downloads)
	}
	// Again, and after a restart (index reloaded, memory gone): from disk.
	pfpTestFetch(a, s, "alice")
	pfpIdx = &pfpIndexState{}
	pfpIndexStale()
	if got := pfpTestFetch(a, s, "alice"); !bytes.Equal(got, []byte("alice-v1")) || s.downloads != 1 {
		t.Fatalf("downloaded again (%d) / %q", s.downloads, got)
	}

	// Alice changes her picture: the broadcast moves the version on.
	s.mu.Lock()
	s.versions["alice"], s.pics["alice"] = 200, []byte("alice-v2")
	s.mu.Unlock()
	pfpIndexSaw(srv.URL, "alice", 200, false)
	if got := pfpTestFetch(a, s, "alice"); !bytes.Equal(got, []byte("alice-v2")) || s.downloads != 2 {
		t.Fatalf("new picture not fetched: %q (%d)", got, s.downloads)
	}
	// The old version is gone from disk.
	e, _, _ := a.pfpLookup("alice")
	if pfpCacheRead(pfpCacheName("pfp", e.id, 100)) != nil {
		t.Fatal("old version kept")
	}
}

func TestPfpCacheOlderServer(t *testing.T) {
	s := &pfpTestServer{noIndex: true, pics: map[string][]byte{"alice": []byte("x")}}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()
	a := pfpTestApp(t, srv)
	pfpTestFetch(a, s, "alice")
	pfpTestFetch(a, s, "alice")
	if s.downloads != 2 {
		t.Fatalf("an older server should be asked each time (as before): %d", s.downloads)
	}
}

// Cache ids are per server: one server can't tell whether you've seen a
// picture on another.
func TestPfpCacheIDPerServer(t *testing.T) {
	k := strings.Repeat("cd", 32)
	if pfpCacheID("a.example", "member", k) == pfpCacheID("b.example", "member", k) {
		t.Fatal("same cache id on two servers")
	}
	if pfpCacheID("a.example", "member", k) != pfpCacheID("A.example", "member", strings.ToUpper(k)) {
		t.Fatal("cache id depends on case")
	}
}

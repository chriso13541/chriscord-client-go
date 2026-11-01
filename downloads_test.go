package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func dlTestApp(t *testing.T, h http.Handler) (*App, string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AppData", t.TempDir())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	if err := updateDownloadPrefs(func(p *downloadPrefs) { p.Folder = dir }); err != nil {
		t.Fatal(err)
	}
	a := &App{domain: srv.URL, token: "tok"}
	return a, dir
}

func waitDownload(t *testing.T, a *App, id string, want ...string) Download {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, d := range a.GetDownloads() {
			if d.ID == id {
				for _, w := range want {
					if d.State == w {
						return d
					}
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("download %s never got to %v: %+v", id, want, a.GetDownloads())
	return Download{}
}

func names(dir string) []string {
	es, _ := os.ReadDir(dir)
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// A server file: fetched with a fresh link into the downloads folder; the
// same name twice gets "(2)"; nothing half-written is left.
func TestDownloadServerFile(t *testing.T) {
	body := bytes.Repeat([]byte("chriscord"), 50000)
	var mu sync.Mutex
	var asked []string
	a, dir := dlTestApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.String())
		mu.Unlock()
		switch r.URL.Path {
		case "/api/files/abc.png/link":
			if r.Header.Get("X-Session-Token") != "tok" {
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"url": "/raw/abc.png?t=xyz"})
		case "/raw/abc.png":
			if r.URL.Query().Get("download") != "1" {
				w.WriteHeader(400)
				return
			}
			w.Write(body)
		default:
			w.WriteHeader(404)
		}
	}))
	d1, err := a.StartDownload(DownloadSource{Kind: "file", URL: "/api/files/abc.png", Name: "../../photo.png"})
	if err != nil {
		t.Fatal(err)
	}
	d2, err := a.StartDownload(DownloadSource{Kind: "file", URL: "/api/files/abc.png", Name: "photo.png"})
	if err != nil {
		t.Fatal(err)
	}
	r1 := waitDownload(t, a, d1.ID, dlDone, dlFailed)
	r2 := waitDownload(t, a, d2.ID, dlDone, dlFailed)
	if r1.State != dlDone || r2.State != dlDone {
		t.Fatalf("%+v %+v", r1, r2)
	}
	if r1.Path != filepath.Join(dir, "photo.png") || r2.Path != filepath.Join(dir, "photo (2).png") {
		t.Fatalf("paths %s %s", r1.Path, r2.Path)
	}
	if r1.Done != int64(len(body)) || r1.Total != int64(len(body)) {
		t.Fatalf("progress %d/%d", r1.Done, r1.Total)
	}
	got, _ := os.ReadFile(r1.Path)
	if !bytes.Equal(got, body) {
		t.Fatal("the file's contents differ")
	}
	if n := names(dir); strings.Join(n, ",") != "photo (2).png,photo.png" {
		t.Fatalf("folder has %v", n)
	}
	// Something that isn't a file on the server isn't fetched at all.
	if _, err := a.StartDownload(DownloadSource{Kind: "file", URL: "https://elsewhere.example/x", Name: "x"}); err == nil {
		t.Fatal("downloaded from somewhere other than the server")
	}
}

// Failing, retrying, cancelling, and the server's zip of a message.
func TestDownloadFailRetryCancel(t *testing.T) {
	var mu sync.Mutex
	fail := true
	release := make(chan struct{})
	a, dir := dlTestApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/files/f.txt/link":
			json.NewEncoder(w).Encode(map[string]string{"url": "/raw/f.txt"})
		case "/raw/f.txt":
			mu.Lock()
			f := fail
			mu.Unlock()
			if f {
				w.WriteHeader(410)
				return
			}
			w.Write([]byte("hello"))
		case "/api/files/slow.bin/link":
			json.NewEncoder(w).Encode(map[string]string{"url": "/raw/slow.bin"})
		case "/raw/slow.bin":
			w.Header().Set("Content-Length", "2000000")
			w.Write(make([]byte, 1000))
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
			}
		case "/api/messages/m1/zip/link":
			json.NewEncoder(w).Encode(map[string]interface{}{"url": "/raw/zip", "name": "m1.zip", "size": 4})
		case "/raw/zip":
			w.Write([]byte("PK\x05\x06"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer close(release)

	d, _ := a.StartDownload(DownloadSource{Kind: "file", URL: "/api/files/f.txt", Name: "f.txt"})
	r := waitDownload(t, a, d.ID, dlDone, dlFailed)
	if r.State != dlFailed || !strings.Contains(r.Error, "410") {
		t.Fatalf("%+v", r)
	}
	if n := names(dir); len(n) != 0 {
		t.Fatalf("a failed download left %v", n)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	a.RetryDownload(d.ID)
	if r := waitDownload(t, a, d.ID, dlDone, dlFailed); r.State != dlDone {
		t.Fatalf("retry: %+v", r)
	}

	s, _ := a.StartDownload(DownloadSource{Kind: "file", URL: "/api/files/slow.bin", Name: "slow.bin"})
	waitDownload(t, a, s.ID, dlDownloading)
	time.Sleep(50 * time.Millisecond)
	a.CancelDownload(s.ID)
	if r := waitDownload(t, a, s.ID, dlCancelled, dlDone, dlFailed); r.State != dlCancelled {
		t.Fatalf("cancel: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "slow.bin.part")); err == nil {
		t.Fatal("a cancelled download left its .part file")
	}

	z, _ := a.StartDownload(DownloadSource{Kind: "zip", MessageID: "m1", Name: "attachments.zip"})
	if r := waitDownload(t, a, z.ID, dlDone, dlFailed); r.State != dlDone {
		t.Fatalf("zip: %+v", r)
	}

	a.ClearDownloads()
	if l := a.GetDownloads(); len(l) != 0 {
		t.Fatalf("clear left %+v", l)
	}
	if n := names(dir); strings.Join(n, ",") != "attachments.zip,f.txt" {
		t.Fatalf("folder has %v (clearing the list mustn't delete files)", n)
	}
}

func TestRiskyFile(t *testing.T) {
	for _, n := range []string{"setup.EXE", "a.bat", "x.ps1", "y.lnk"} {
		if !riskyFile(n) {
			t.Errorf("%s should be risky", n)
		}
	}
	for _, n := range []string{"photo.jpg", "song.mp3", "notes.txt", "a.zip"} {
		if riskyFile(n) {
			t.Errorf("%s isn't risky", n)
		}
	}
}

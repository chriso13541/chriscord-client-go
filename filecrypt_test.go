package main

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	mrand "math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func encryptedBlob(t *testing.T, plain []byte) ([]byte, *fileEntry) {
	t.Helper()
	_, aead, err := newFileKey()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	n, err := encryptFile(&buf, bytes.NewReader(plain), aead)
	if err != nil || n != int64(len(plain)) {
		t.Fatalf("encrypt: %v (%d of %d)", err, n, len(plain))
	}
	if int64(buf.Len()) != fileCipherSize(int64(len(plain))) {
		t.Fatalf("size %d, expected %d", buf.Len(), fileCipherSize(int64(len(plain))))
	}
	return buf.Bytes(), &fileEntry{meta: fileMeta{URL: "/api/files/x.e2ee", Size: int64(len(plain)), Mime: "video/mp4"}, aead: aead}
}

func TestFileRoundTripSizes(t *testing.T) {
	for _, n := range []int{0, 1, 100, fileChunk - 1, fileChunk, fileChunk + 1, 3 * fileChunk, 3*fileChunk + 17} {
		plain := make([]byte, n)
		rand.Read(plain)
		blob, e := encryptedBlob(t, plain)
		if bytes.Contains(blob, plain[:min(n, 64)]) && n >= 16 {
			t.Fatal("plaintext visible in the blob")
		}
		var out bytes.Buffer
		err := decryptChunks(&out, bytes.NewReader(blob[fileHeaderLen:]), e.aead, int64(n), 0, fileChunks(int64(n))-1, 0, int64(n)-1)
		if err != nil || !bytes.Equal(out.Bytes(), plain) {
			t.Fatalf("size %d: %v", n, err)
		}
	}
}

func TestFileTamperAndTruncation(t *testing.T) {
	plain := make([]byte, 3*fileChunk)
	rand.Read(plain)
	blob, e := encryptedBlob(t, plain)
	size := int64(len(plain))
	bad := append([]byte(nil), blob...)
	bad[fileHeaderLen+10] ^= 1
	if decryptChunks(&bytes.Buffer{}, bytes.NewReader(bad[fileHeaderLen:]), e.aead, size, 0, 2, 0, size-1) == nil {
		t.Fatal("tampered file decrypted")
	}
	// Cut off after two chunks and claim that's the whole file: the second
	// chunk isn't marked as the last, so it fails.
	cut := blob[:fileHeaderLen+2*(fileChunk+16)]
	if decryptChunks(&bytes.Buffer{}, bytes.NewReader(cut[fileHeaderLen:]), e.aead, 2*fileChunk, 0, 1, 0, 2*fileChunk-1) == nil {
		t.Fatal("truncated file accepted")
	}
	// Chunks swapped around don't decrypt either.
	sw := append([]byte(nil), blob...)
	c := fileChunk + 16
	copy(sw[fileHeaderLen:], blob[fileHeaderLen+c:fileHeaderLen+2*c])
	copy(sw[fileHeaderLen+c:], blob[fileHeaderLen:fileHeaderLen+c])
	if decryptChunks(&bytes.Buffer{}, bytes.NewReader(sw[fileHeaderLen:]), e.aead, size, 0, 2, 0, size-1) == nil {
		t.Fatal("reordered chunks accepted")
	}
}

// The page's requests, through serveE2EEFile, against a server that
// answers Range requests like the real one (tower-http's ServeFile).
func TestServeRanges(t *testing.T) {
	plain := make([]byte, 5*fileChunk+1234)
	rand.Read(plain)
	blob, e := encryptedBlob(t, plain)
	fetches := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Session-Token") != "tok" {
			http.Error(w, "no", 403)
			return
		}
		fetches++
		http.ServeContent(w, r, "x.e2ee", time.Time{}, bytes.NewReader(blob))
	}))
	defer srv.Close()

	a := &App{domain: srv.URL, token: "tok"}
	a.text = newTextCrypto(a)
	a.text.files["x.e2ee"] = e
	size := int64(len(plain))

	get := func(rng string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", fileLocalPrefix+"x.e2ee", nil)
		if rng != "" {
			r.Header.Set("Range", rng)
		}
		w := httptest.NewRecorder()
		a.serveE2EEFile(w, r)
		return w
	}
	// Whole file.
	if w := get(""); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), plain) {
		t.Fatalf("whole file: %d, %d bytes", w.Code, w.Body.Len())
	}
	// Random ranges, including ones straddling chunk edges.
	rng := mrand.New(mrand.NewSource(1))
	for i := 0; i < 60; i++ {
		from := rng.Int63n(size)
		to := from + rng.Int63n(size-from)
		w := get(fmt.Sprintf("bytes=%d-%d", from, to))
		if w.Code != 206 || !bytes.Equal(w.Body.Bytes(), plain[from:to+1]) {
			t.Fatalf("range %d-%d: %d, got %d bytes", from, to, w.Code, w.Body.Len())
		}
		if cr := w.Header().Get("Content-Range"); cr != fmt.Sprintf("bytes %d-%d/%d", from, to, size) {
			t.Fatalf("Content-Range %q", cr)
		}
	}
	// Open-ended and suffix ranges.
	if w := get("bytes=100-"); w.Code != 206 || !bytes.Equal(w.Body.Bytes(), plain[100:]) {
		t.Fatal("open-ended range")
	}
	if w := get("bytes=-50"); w.Code != 206 || !bytes.Equal(w.Body.Bytes(), plain[size-50:]) {
		t.Fatal("suffix range")
	}
	// Unknown file.
	r := httptest.NewRequest("GET", fileLocalPrefix+"nope.e2ee", nil)
	w := httptest.NewRecorder()
	a.serveE2EEFile(w, r)
	if w.Code != 404 {
		t.Fatalf("unknown file: %d", w.Code)
	}
	// Saving decrypts the whole thing.
	var out bytes.Buffer
	if err := a.decryptTo(&out, e); err != nil || !bytes.Equal(out.Bytes(), plain) {
		t.Fatalf("decryptTo: %v", err)
	}
}

func TestFileRangeCap(t *testing.T) {
	plain := make([]byte, fileRangeCap+3*fileChunk)
	rand.Read(plain)
	blob, e := encryptedBlob(t, plain)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "x", time.Time{}, bytes.NewReader(blob))
	}))
	defer srv.Close()
	a := &App{domain: srv.URL}
	a.text = newTextCrypto(a)
	a.text.files["x.e2ee"] = e
	r := httptest.NewRequest("GET", fileLocalPrefix+"x.e2ee", nil)
	r.Header.Set("Range", "bytes=0-")
	w := httptest.NewRecorder()
	a.serveE2EEFile(w, r)
	if w.Code != 206 || w.Body.Len() != fileRangeCap || !bytes.Equal(w.Body.Bytes(), plain[:fileRangeCap]) {
		t.Fatalf("capped range: %d, %d bytes", w.Code, w.Body.Len())
	}
}

// Attachments travel inside the encrypted message; the server's list only
// has bare blob addresses; edits keep them.
func TestAttachmentsInMessages(t *testing.T) {
	textTestEnv(t)
	s := newFakeKeyServer()
	alice, bob := s.add(t, "alice"), s.add(t, "bob")
	alice.text.ensurePublished()
	bob.text.ensurePublished()

	// What uploadEncrypted leaves behind for a file it uploaded.
	key, _, _ := newFileKey()
	meta := fileMeta{URL: "/api/files/abc.e2ee", Name: "holiday.mp4", Mime: "video/mp4", Size: 1234,
		Key: b64(key)}
	local, err := alice.text.remember(meta)
	if err != nil {
		t.Fatal(err)
	}
	content, sent, err := alice.text.sealMessage("b1", "look", []Attachment{
		{URL: local, Name: "holiday.mp4", Mime: "video/mp4", Spoiler: true},
		{URL: "/api/files/old.png", Name: "old.png", Mime: "image/png"}, // from before encryption
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content, "holiday") {
		t.Fatal("file name visible in the message")
	}
	if len(sent) != 2 || sent[0].URL != "/api/files/abc.e2ee" || sent[0].Name != fileOpaqueName || sent[0].Mime != fileOpaqueMime || sent[0].Spoiler {
		t.Fatalf("what the server gets: %+v", sent)
	}

	m := ChatMessage{ID: "m1", BoardID: "b1", Username: "alice", Content: content, Attachments: sent}
	bob.text.open(&m)
	if m.Content != "look" || len(m.Attachments) != 2 {
		t.Fatalf("bob sees %+v", m)
	}
	var enc Attachment
	for _, x := range m.Attachments {
		if strings.HasPrefix(x.URL, fileLocalPrefix) {
			enc = x
		}
	}
	if enc.Name != "holiday.mp4" || enc.Mime != "video/mp4" || !enc.Spoiler {
		t.Fatalf("decrypted attachment: %+v", enc)
	}
	if e := bob.text.fileFor(enc.URL); e == nil || e.meta.Size != 1234 {
		t.Fatal("bob's app can't serve it")
	}

	// Alice's edit keeps the attachment.
	alice.text.open(&ChatMessage{ID: "m1", BoardID: "b1", Username: "alice", Content: content, Attachments: sent})
	edited, err := alice.text.sealEdit("m1", "look again")
	if err != nil {
		t.Fatal(err)
	}
	m2 := ChatMessage{ID: "m1", BoardID: "b1", Username: "alice", Content: edited, Attachments: sent}
	bob.text.open(&m2)
	if m2.Content != "look again" || len(m2.Attachments) != 2 {
		t.Fatalf("after edit: %+v", m2)
	}

	// A message that can't be opened shows no blob entries.
	carol := s.add(t, "carol")
	carol.text.ensurePublished()
	s.access["b1"] = map[string]bool{"alice": true, "bob": true, "carol": true}
	m3 := ChatMessage{ID: "m1", BoardID: "b1", Username: "alice", Content: content, Attachments: sent}
	carol.text.open(&m3)
	if m3.E2EE != "locked" || len(m3.Attachments) != 1 || m3.Attachments[0].Name != "old.png" {
		t.Fatalf("locked message: %+v", m3)
	}

	// An upload this app no longer knows about can't be sent.
	if _, _, err := alice.text.sealMessage("b1", "", []Attachment{{URL: fileLocalPrefix + "gone.e2ee"}}); err == nil {
		t.Fatal("sent an attachment with no key")
	}
}

func TestParseRange(t *testing.T) {
	for _, c := range []struct {
		h        string
		from, to int64
		ok       bool
	}{
		{"bytes=0-9", 0, 9, true}, {"bytes=5-", 5, 99, true}, {"bytes=-10", 90, 99, true},
		{"bytes=90-200", 90, 99, true}, {"bytes=100-", 0, 0, false}, {"bytes=5-2", 0, 0, false},
		{"bytes=0-1,5-6", 0, 0, false}, {"", 0, 0, false}, {"items=0-1", 0, 0, false},
	} {
		f, to, ok := parseRange(c.h, 100)
		if ok != c.ok || (ok && (f != c.from || to != c.to)) {
			t.Fatalf("%q: %d-%d %v", c.h, f, to, ok)
		}
	}
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// Uploading: what reaches the server is a nameless encrypted blob, and the
// page gets a local address that serves the original.
func TestUploadEncrypted(t *testing.T) {
	plain := bytes.Repeat([]byte("secret holiday video frame "), 20000)
	var stored []byte
	var gotName string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/upload":
			f, h, err := r.FormFile("file")
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			gotName = h.Filename
			var b bytes.Buffer
			b.ReadFrom(f)
			stored = b.Bytes()
			w.Write([]byte(`{"url":"/api/files/u1.e2ee","filename":"attachment.e2ee","mime":"application/octet-stream"}`))
		case r.URL.Path == "/api/files/u1.e2ee":
			http.ServeContent(w, r, "u1.e2ee", time.Time{}, bytes.NewReader(stored))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	a := &App{domain: srv.URL, token: "tok"}
	a.text = newTextCrypto(a)
	res, err := a.uploadEncrypted(srv.URL, "tok", "holiday.mp4", bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	if gotName != fileBlobName || bytes.Contains(stored, []byte("secret holiday")) {
		t.Fatalf("the server learned something: name %q", gotName)
	}
	if res.URL != fileLocalPrefix+"u1.e2ee" || res.Filename != "holiday.mp4" || res.Mime != "video/mp4" {
		t.Fatalf("result %+v", res)
	}
	w := httptest.NewRecorder()
	a.serveE2EEFile(w, httptest.NewRequest("GET", res.URL, nil))
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), plain) || w.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("served back: %d, %d bytes, %s", w.Code, w.Body.Len(), w.Header().Get("Content-Type"))
	}
}

// Downloads: the browser gets a link to this app, which streams the file
// (or a zip of a message's files) decrypted.
func TestBrowserDownloadLinks(t *testing.T) {
	plain := make([]byte, 3*fileChunk+99)
	rand.Read(plain)
	blob, e := encryptedBlob(t, plain)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "x", time.Time{}, bytes.NewReader(blob))
	}))
	defer srv.Close()
	a := &App{domain: srv.URL}
	a.text = newTextCrypto(a)
	e.meta.Name, e.meta.Mime = "holiday clip.mp4", "video/mp4"
	a.text.files["x.e2ee"] = e
	a.text.msgFiles["m1"] = []fileMeta{e.meta, e.meta}

	link, err := a.AttachmentDownloadLink(fileLocalPrefix+"x.e2ee", "holiday clip.mp4")
	if err != nil || !strings.HasPrefix(link, "http://127.0.0.1:") {
		t.Fatalf("link %q: %v", link, err)
	}
	resp, err := http.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	got.ReadFrom(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(got.Bytes(), plain) ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("download: %d, %d bytes, %q", resp.StatusCode, got.Len(), resp.Header.Get("Content-Disposition"))
	}

	zlink, err := a.AttachmentsZipLink("m1")
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.Get(zlink)
	if err != nil {
		t.Fatal(err)
	}
	got.Reset()
	got.ReadFrom(resp.Body)
	resp.Body.Close()
	zr, err := zip.NewReader(bytes.NewReader(got.Bytes()), int64(got.Len()))
	if err != nil || len(zr.File) != 2 || zr.File[1].Name != "holiday clip (2).mp4" {
		t.Fatalf("zip: %v %d", err, len(zr.File))
	}
	f, _ := zr.File[0].Open()
	var one bytes.Buffer
	one.ReadFrom(f)
	if !bytes.Equal(one.Bytes(), plain) {
		t.Fatal("zipped file differs")
	}

	// A made-up or expired token doesn't work.
	bad := zlink[:strings.Index(zlink, "/dl/")+4] + "00000000000000000000000000000000/x"
	if r, _ := http.Get(bad); r == nil || r.StatusCode != http.StatusGone {
		t.Fatal("unknown token accepted")
	}
}

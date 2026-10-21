package main

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// rewrite sends every request to srv, keeping its path (as if it were the
// real site).
type rewrite struct{ srv *httptest.Server }

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	u, _ := url.Parse(r.srv.URL)
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = u.Scheme, u.Host
	return http.DefaultTransport.RoundTrip(req)
}

func pngOf(w, h int) []byte {
	var b bytes.Buffer
	png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h)))
	return b.Bytes()
}

func TestWikipediaPreview(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		switch {
		case r.URL.Path == "/api/rest_v1/page/summary/Linux":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"type":"standard","title":"Linux","displaytitle":"Linux","extract":"Linux is a family of free and open-source software Unix-like operating systems based on the Linux kernel, which was first released on 17 September 1991 by Linus Torvalds.",
				"thumbnail":{"source":"https://upload.wikimedia.org/wikipedia/commons/thumb/3/35/Tux.svg/320px-Tux.svg.png","width":320,"height":378},
				"originalimage":{"source":"https://upload.wikimedia.org/wikipedia/commons/3/35/Tux.svg","width":265,"height":314}}`))
		case strings.HasSuffix(r.URL.Path, "/480px-Tux.svg.png"):
			w.Write(pngOf(406, 480)) // a tall picture
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := &http.Client{Transport: rewrite{srv}}
	p, err := makeLinkPreview(context.Background(), client, "https://en.wikipedia.org/wiki/Linux")
	if err != nil || p == nil {
		t.Fatal(err)
	}
	if p.T != "Linux" || p.S != "Wikipedia" || !strings.HasPrefix(p.D, "Linux is a family of free") {
		t.Fatalf("%+v", p)
	}
	if !strings.HasPrefix(p.I, "data:image/jpeg;base64,") {
		t.Fatalf("no picture (asked for %v)", asked)
	}
	// Mobile links and other languages are articles too; the main page isn't one.
	for link, want := range map[string]bool{"https://en.m.wikipedia.org/wiki/Linux": true, "https://de.wikipedia.org/wiki/Linux": true,
		"https://en.wikipedia.org/w/index.php?title=Linux": false, "https://example.org/wiki/Linux": false} {
		u, _ := url.Parse(link)
		if _, _, ok := wikipediaArticle(u); ok != want {
			t.Errorf("%s: %v", link, ok)
		}
	}
}

func TestFirstParagraph(t *testing.T) {
	page := `<html><head><title>x</title></head><body>
		<nav><p>Home. About. Contact us today for more information about everything here.</p></nav>
		<p>Short.</p>
		<p>We use cookies to improve your experience on this website, see our policy for details.</p>
		<p><b>Chriscord</b> is a self-hostable chat app for voice, video and text, with end-to-end encryption<sup>[1]</sup> and   servers anyone can run.</p>
		<p>A later paragraph that shouldn't be chosen because an earlier one already was.</p></body></html>`
	got := firstParagraph(page)
	if got != "Chriscord is a self-hostable chat app for voice, video and text, with end-to-end encryption and servers anyone can run." {
		t.Fatalf("got %q", got)
	}
	if firstParagraph("<body><p>menu</p></body>") != "" {
		t.Fatal("found a paragraph in a menu")
	}
}

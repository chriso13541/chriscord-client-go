package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Stand-ins for sites that turn away programs, or don't say what their
// picture is the usual way.
func TestPreviewHardSites(t *testing.T) {
	jpg := pngOf(300, 200) // (any picture Go reads will do)
	var imgAsked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua := r.Header.Get("User-Agent")
		switch {
		// A shop that only gives its preview to link-preview fetchers, and
		// shows browsers from elsewhere a robot check.
		case r.URL.Path == "/shop":
			if strings.Contains(ua, "facebookexternalhit") {
				w.Write([]byte(`<html><head><meta property="og:title" content="Fancy Lamp"><meta property="og:image" content="/img/lamp.jpg"></head></html>`))
			} else {
				w.Write([]byte(`<html><head><title>Robot Check</title></head><body>Type the characters you see.</body></html>`))
			}
		// A site that answers browsers 403 outright.
		case r.URL.Path == "/forum":
			if strings.Contains(ua, "Chrome") {
				w.WriteHeader(403)
				return
			}
			w.Write([]byte(`<html><head><meta property="og:title" content="A post"><meta property="og:image" content="/img/post.jpg"></head></html>`))
		// Amazon: no og:image, the picture in its own attributes.
		case r.URL.Path == "/dp/B0TEST":
			w.Write([]byte(`<html><head><title>Amazon.com: Widget</title></head><body>
				<span id="productTitle" class="a-size-large">   Super Widget, Blue   </span>
				<img id="landingImage" src="data:image/gif;base64,R0lGOD" data-old-hires="https://m.media-amazon.com/img/widget.jpg"></body></html>`))
		// eBay: the product in JSON-LD, its picture named .webp.
		case r.URL.Path == "/itm/1":
			w.Write([]byte(`<html><head><title>x | eBay</title>
				<script type="application/ld+json">{"@context":"https://schema.org","@type":"Product","name":"Vintage Camera","description":"Works great.","image":["https://i.ebayimg.com/images/g/abc/s-l1600.webp"]}</script></head></html>`))
		case strings.HasPrefix(r.URL.Path, "/img/"), strings.HasPrefix(r.URL.Path, "/images/"):
			imgAsked = append(imgAsked, r.URL.Path+" accept="+r.Header.Get("Accept"))
			if strings.HasSuffix(r.URL.Path, ".webp") {
				w.Header().Set("Content-Type", "image/webp")
				w.Write([]byte("RIFF....WEBPVP8 ")) // Go can't read these
				return
			}
			w.Write(jpg)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := &http.Client{Transport: rewrite{srv}}

	check := func(link, title string) *textLinkPreview {
		t.Helper()
		p, err := makeLinkPreview(context.Background(), client, link)
		if err != nil || p == nil {
			t.Fatalf("%s: %v", link, err)
		}
		if p.T != title || !strings.HasPrefix(p.I, "data:image/jpeg;base64,") {
			t.Fatalf("%s: title %q, picture %v (pictures asked for: %v)", link, p.T, p.I != "", imgAsked)
		}
		return p
	}
	check("https://shop.example/shop", "Fancy Lamp")
	check("https://forum.example/forum", "A post")
	// The page names the picture with the real site's address; the test
	// server stands in for every host.
	p := check("https://www.amazon.com/dp/B0TEST", "Super Widget, Blue")
	if p.S != "amazon.com" {
		t.Fatalf("site %q", p.S)
	}
	p = check("https://www.ebay.com/itm/1", "Vintage Camera")
	if p.D != "Works great." {
		t.Fatalf("description %q", p.D)
	}
	webp := false
	for _, a := range imgAsked {
		if strings.Contains(a, ".webp") || strings.Contains(a, "image/webp") {
			webp = true
		}
	}
	if webp {
		t.Fatalf("asked for WebP: %v", imgAsked)
	}
}

func TestFirstLinkTail(t *testing.T) {
	for in, want := range map[string]string{
		"see https://en.wikipedia.org/wiki/Linux_(kernel).":              "https://en.wikipedia.org/wiki/Linux_(kernel)",
		"(https://example.com/a)":                                        "https://example.com/a",
		"https://www.amazon.com/dp/B0?ref_=x&th=1&utm_source=chat, look": "https://www.amazon.com/dp/B0?ref_=x&th=1&utm_source=chat",
		"\"https://a.com/x\"":                                            "https://a.com/x",
		"https://":                                                       "",
	} {
		if got := firstLink(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

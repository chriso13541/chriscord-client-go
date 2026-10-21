package main

// linkpreview.go — link previews, made by the app that sends the link.
//
// They used to be fetched by the server, for everyone who saw the message
// — so the server learned every link anyone posted. Now the sender's app
// fetches the page itself, when the message is sent, and puts the preview
// inside the encrypted message: its title, description, site and a small
// copy of its picture. Nobody else fetches anything (the server never sees
// the link; the site only sees the sender, who posted it).
//
// A link straight to an image or a video is still shown as that image or
// video, loaded from where it's hosted when someone looks at it (as before;
// not through the server).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// textLinkPreview is a link's preview inside a message (textPlain.L).
type textLinkPreview struct {
	U string `json:"u"`           // the link
	T string `json:"t,omitempty"` // title
	D string `json:"d,omitempty"` // description
	S string `json:"s,omitempty"` // site name
	K string `json:"k,omitempty"` // "image" or "video": the link is that file itself
	V string `json:"v,omitempty"` // a video the page offers to play
	I string `json:"i,omitempty"` // its picture, small (a data: URL, JPEG)
}

const (
	previewTimeout   = 4 * time.Second
	previewMaxPage   = 1 << 20
	previewMaxImage  = 6 << 20
	previewThumbSide = 480
	previewMaxThumb  = 60 << 10
	previewUA        = "Mozilla/5.0 (compatible; Chriscord/1.0; link preview)"
	redditUA         = "desktop:chriscord-link-preview:1.0 (chat app)"
)

var (
	reURL      = regexp.MustCompile(`https?://[^\s<>"'` + "`" + `]+`)
	reMetaTag  = regexp.MustCompile(`(?is)<meta\b[^>]*>`)
	reAttr     = regexp.MustCompile(`(?is)([a-z:_-]+)\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`)
	reTitleTag = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
)

// firstLink: the first http(s) link in a message's text ("" if none), as
// the page finds them (trailing punctuation dropped).
func firstLink(text string) string {
	m := reURL.FindString(text)
	return strings.TrimRight(m, ".,;:!?)]}'\"")
}

func linkExt(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	path := strings.ToLower(p.Path)
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[i+1:]
	}
	return ""
}

// makeLinkPreview fetches link and makes its preview (nil if there's
// nothing to show).
func makeLinkPreview(ctx context.Context, client *http.Client, link string) (*textLinkPreview, error) {
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("not a web link")
	}
	switch linkExt(link) {
	case "png", "jpg", "jpeg", "gif", "webp", "avif", "bmp":
		return &textLinkPreview{U: link, K: "image"}, nil
	case "mp4", "webm", "mov", "m4v":
		return &textLinkPreview{U: link, K: "video"}, nil
	}
	if isRedditHost(u.Host) {
		if p := redditPreview(ctx, client, link); p != nil {
			return p, nil
		}
	}
	if lang, title, ok := wikipediaArticle(u); ok {
		if p := wikipediaPreview(ctx, client, link, lang, title); p != nil {
			return p, nil
		}
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", link, nil)
	req.Header.Set("User-Agent", previewUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("the site answered %d", resp.StatusCode)
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch {
	case strings.HasPrefix(ct, "image/"):
		return &textLinkPreview{U: link, K: "image"}, nil
	case strings.HasPrefix(ct, "video/"):
		return &textLinkPreview{U: link, K: "video"}, nil
	case ct != "" && ct != "text/html" && ct != "application/xhtml+xml":
		return nil, nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, previewMaxPage))
	page := string(body)
	base := resp.Request.URL
	meta := pageMeta(page)
	get := func(keys ...string) string {
		for _, k := range keys {
			if v := strings.TrimSpace(meta[k]); v != "" {
				return v
			}
		}
		return ""
	}
	p := &textLinkPreview{U: link,
		T: clip(get("og:title", "twitter:title"), 200),
		D: clip(get("og:description", "twitter:description", "description"), 400),
		S: clip(get("og:site_name"), 80)}
	if p.T == "" {
		if m := reTitleTag.FindStringSubmatch(page); m != nil {
			p.T = clip(strings.TrimSpace(html.UnescapeString(m[1])), 200)
		}
	}
	if p.D == "" {
		// No description given: its first real paragraph, as a search
		// engine shows (an article's opening, say).
		p.D = clip(firstParagraph(page), 300)
	}
	if v := get("og:video:secure_url", "og:video:url", "og:video"); v != "" {
		vt := strings.ToLower(get("og:video:type"))
		ext := linkExt(v)
		if vt == "video/mp4" || vt == "video/webm" || ext == "mp4" || ext == "webm" {
			p.V = resolveLink(base, v)
		}
	}
	if img := get("og:image:secure_url", "og:image", "twitter:image", "twitter:image:src"); img != "" {
		p.I = thumbnailFrom(ctx, client, resolveLink(base, img))
	}
	if p.T == "" && p.D == "" && p.I == "" && p.V == "" {
		return nil, nil
	}
	return p, nil
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func resolveLink(base *url.URL, ref string) string {
	r, err := url.Parse(strings.TrimSpace(ref))
	if err != nil || base == nil {
		return ref
	}
	out := base.ResolveReference(r)
	if out.Scheme != "http" && out.Scheme != "https" {
		return ""
	}
	return out.String()
}

// pageMeta: a page's <meta> tags, property/name → content (first wins).
func pageMeta(page string) map[string]string {
	if i := strings.Index(strings.ToLower(page), "</head>"); i > 0 {
		page = page[:i]
	}
	out := map[string]string{}
	for _, tag := range reMetaTag.FindAllString(page, 400) {
		attrs := map[string]string{}
		for _, m := range reAttr.FindAllStringSubmatch(tag, -1) {
			v := m[3] + m[4] + m[5]
			attrs[strings.ToLower(m[1])] = html.UnescapeString(v)
		}
		key := strings.ToLower(attrs["property"])
		if key == "" {
			key = strings.ToLower(attrs["name"])
		}
		if key != "" && attrs["content"] != "" {
			if _, seen := out[key]; !seen {
				out[key] = attrs["content"]
			}
		}
	}
	return out
}

// thumbnailFrom fetches a picture and makes a small JPEG of it, as a data:
// URL ("" if it can't: too big, or a kind Go can't read, like WebP).
func thumbnailFrom(ctx context.Context, client *http.Client, link string) string {
	if link == "" {
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, "GET", link, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", previewUA)
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return ""
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, previewMaxImage+1))
	if len(data) > previewMaxImage {
		return ""
	}
	return thumbnail(data)
}

// thumbnail: a picture made at most previewThumbSide across, as a JPEG
// data: URL no bigger than previewMaxThumb ("" if it can't be read).
func thumbnail(data []byte) string {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > 40_000_000 {
		return ""
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return ""
	}
	for side, q := previewThumbSide, 72; side >= 120; side, q = side*3/4, q-6 {
		var buf bytes.Buffer
		if jpeg.Encode(&buf, scaleDown(src, side), &jpeg.Options{Quality: q}) != nil {
			return ""
		}
		if buf.Len() <= previewMaxThumb {
			return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
		}
	}
	return ""
}

// scaleDown shrinks img so its longer side is at most side (averaging the
// pixels each new one covers).
func scaleDown(img image.Image, side int) *image.RGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	nw, nh := w, h
	if w >= h && w > side {
		nw, nh = side, h*side/w
	} else if h > w && h > side {
		nw, nh = w*side/h, side
	}
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	out := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		y0, y1 := b.Min.Y+y*h/nh, b.Min.Y+(y+1)*h/nh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < nw; x++ {
			x0, x1 := b.Min.X+x*w/nw, b.Min.X+(x+1)*w/nw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, n uint64
			// At most 4×4 samples per pixel: plenty for a small picture.
			sx, sy := (x1-x0+3)/4, (y1-y0+3)/4
			if sx < 1 {
				sx = 1
			}
			if sy < 1 {
				sy = 1
			}
			for yy := y0; yy < y1; yy += sy {
				for xx := x0; xx < x1; xx += sx {
					cr, cg, cb, _ := img.At(xx, yy).RGBA()
					r, g, bl, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), n+1
				}
			}
			i := out.PixOffset(x, y)
			out.Pix[i], out.Pix[i+1], out.Pix[i+2], out.Pix[i+3] = uint8(r/n>>8), uint8(g/n>>8), uint8(bl/n>>8), 255
		}
	}
	return out
}

var (
	reParagraph     = regexp.MustCompile(`(?is)<p\b[^>]*>(.*?)</p>`)
	reTags          = regexp.MustCompile(`(?s)<[^>]*>`)
	reFootnote      = regexp.MustCompile(`\[(\d+|[a-z]|citation needed|note \d+)\]`)
	reSpaces        = regexp.MustCompile(`\s+`)
	reWikiThumbSize = regexp.MustCompile(`/\d+px-`)
	reNoText        = regexp.MustCompile(`(?is)<(script|style|noscript|nav|header|footer|aside|form)\b.*?</\s*(script|style|noscript|nav|header|footer|aside|form)\s*>`)
)

// firstParagraph: the text of the first paragraph on page that reads like
// one (a sentence or two, not a menu, a caption or a cookie notice).
func firstParagraph(page string) string {
	body := page
	if i := strings.Index(strings.ToLower(body), "<body"); i >= 0 {
		body = body[i:]
	}
	body = reNoText.ReplaceAllString(body, " ")
	for i, m := range reParagraph.FindAllStringSubmatch(body, 60) {
		if i >= 60 {
			break
		}
		t := html.UnescapeString(reTags.ReplaceAllString(m[1], ""))
		t = reFootnote.ReplaceAllString(t, "")
		t = strings.TrimSpace(reSpaces.ReplaceAllString(t, " "))
		low := strings.ToLower(t)
		if len([]rune(t)) < 80 || !strings.ContainsAny(t, ".!?") ||
			strings.Contains(low, "cookie") || strings.Contains(low, "javascript") || strings.Contains(low, "subscribe") {
			continue
		}
		return t
	}
	return ""
}

// wikipediaArticle: whether u is a Wikipedia article (en.wikipedia.org/wiki/
// Linux, its mobile site too), and its language and title as in the link.
func wikipediaArticle(u *url.URL) (lang, title string, ok bool) {
	host := strings.ToLower(u.Hostname())
	if !strings.HasSuffix(host, ".wikipedia.org") || !strings.HasPrefix(u.EscapedPath(), "/wiki/") {
		return "", "", false
	}
	lang = strings.TrimSuffix(strings.TrimSuffix(host, ".wikipedia.org"), ".m")
	title = strings.TrimPrefix(u.EscapedPath(), "/wiki/")
	if lang == "" || title == "" || strings.ContainsAny(lang, "./") || strings.Contains(title, "/") && !strings.Contains(title, "%2F") {
		return "", "", false
	}
	return lang, title, true
}

// wikipediaPreview: an article's opening paragraph and picture, from
// Wikipedia's page summaries (its pages have no description of their own
// for a preview to show).
func wikipediaPreview(ctx context.Context, client *http.Client, link, lang, title string) *textLinkPreview {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://"+lang+".wikipedia.org/api/rest_v1/page/summary/"+title, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", previewUA)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	var d struct {
		Title     string `json:"title"`
		Extract   string `json:"extract"`
		Thumbnail struct {
			Source string `json:"source"`
		} `json:"thumbnail"`
		Original struct {
			Source string `json:"source"`
			Width  int    `json:"width"`
		} `json:"originalimage"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, previewMaxPage)).Decode(&d) != nil || (d.Title == "" && d.Extract == "") {
		return nil
	}
	p := &textLinkPreview{U: link, T: clip(d.Title, 200), D: clip(strings.TrimSpace(d.Extract), 300), S: "Wikipedia"}
	// Its picture: Wikipedia's thumbnail asked for at the size previews
	// use (it renders any size, SVGs included, as PNG or JPEG); else the
	// original, if Go can read it.
	if t := d.Thumbnail.Source; t != "" {
		p.I = thumbnailFrom(ctx, client, reWikiThumbSize.ReplaceAllString(t, fmt.Sprintf("/%dpx-", previewThumbSide)))
		if p.I == "" {
			p.I = thumbnailFrom(ctx, client, t)
		}
	}
	if p.I == "" && d.Original.Source != "" && d.Original.Width <= 2000 {
		p.I = thumbnailFrom(ctx, client, d.Original.Source)
	}
	return p
}

func isRedditHost(host string) bool {
	host = strings.ToLower(strings.TrimPrefix(host, "www."))
	return host == "reddit.com" || strings.HasSuffix(host, ".reddit.com") || host == "redd.it"
}

var reRedditPost = regexp.MustCompile(`/comments/([a-z0-9]+)`)

// redditPreview: a Reddit post, from Reddit's JSON for it (its pages turn
// away programs). Share links are followed to the post first.
func redditPreview(ctx context.Context, client *http.Client, link string) *textLinkPreview {
	final := link
	if !reRedditPost.MatchString(link) {
		req, _ := http.NewRequestWithContext(ctx, "GET", link, nil)
		req.Header.Set("User-Agent", redditUA)
		resp, err := client.Do(req)
		if err != nil {
			return nil
		}
		resp.Body.Close()
		final = resp.Request.URL.String()
	}
	m := reRedditPost.FindStringSubmatch(final)
	if m == nil {
		return nil
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://www.reddit.com/comments/"+m[1]+".json?raw_json=1&limit=1", nil)
	req.Header.Set("User-Agent", redditUA)
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var listing []struct {
		Data struct {
			Children []struct {
				Data struct {
					Title     string `json:"title"`
					Selftext  string `json:"selftext"`
					Subreddit string `json:"subreddit_name_prefixed"`
					Thumbnail string `json:"thumbnail"`
					URL       string `json:"url_overridden_by_dest"`
					Preview   struct {
						Images []struct {
							Source struct {
								URL string `json:"url"`
							} `json:"source"`
						} `json:"images"`
					} `json:"preview"`
				} `json:"data"`
			} `json:"children"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, previewMaxPage)).Decode(&listing) != nil || len(listing) == 0 || len(listing[0].Data.Children) == 0 {
		return nil
	}
	d := listing[0].Data.Children[0].Data
	p := &textLinkPreview{U: link, T: clip(d.Title, 200), D: clip(d.Selftext, 400), S: "Reddit · " + d.Subreddit}
	img := ""
	if len(d.Preview.Images) > 0 {
		img = d.Preview.Images[0].Source.URL
	} else if strings.HasPrefix(d.Thumbnail, "http") {
		img = d.Thumbnail
	}
	p.I = thumbnailFrom(ctx, client, img)
	return p
}

// previewFor makes the preview for a message being sent (nil if it has no
// link, or the page can't be fetched in time — it's sent without).
func previewFor(text string) *textLinkPreview {
	link := firstLink(text)
	if link == "" || !linkPreviewsOn() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), previewTimeout)
	defer cancel()
	client := &http.Client{Timeout: previewTimeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return nil
	}}
	p, err := makeLinkPreview(ctx, client, link)
	if err != nil {
		return nil
	}
	return p
}

// asLinkPreview turns a message's preview into what the page shows.
func (p *textLinkPreview) asLinkPreview() *LinkPreview {
	if p == nil || p.U == "" {
		return nil
	}
	str := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	img := ""
	if strings.HasPrefix(p.I, "data:image/jpeg;base64,") {
		img = p.I
	}
	return &LinkPreview{URL: p.U, Title: str(p.T), Description: str(p.D), Image: str(img), SiteName: str(p.S), Kind: p.K, Video: str(p.V)}
}

// Whether this app makes link previews (Settings; on unless turned off).
var linkPreviewsOff atomic.Bool

func linkPreviewsOn() bool { return !linkPreviewsOff.Load() }

// SetLinkPreviews turns making link previews on or off (when off, links
// are sent without one, and the app doesn't fetch the page).
func (a *App) SetLinkPreviews(on bool) { linkPreviewsOff.Store(!on) }

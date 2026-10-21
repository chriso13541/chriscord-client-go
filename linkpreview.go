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
	previewTimeout   = 5 * time.Second
	previewMaxPage   = 3 << 20 // shops' pages are big; what a preview needs is near the top
	previewMaxImage  = 6 << 20
	previewThumbSide = 480
	previewMaxThumb  = 60 << 10
	previewUA        = "Mozilla/5.0 (compatible; Chriscord/1.0; link preview)"
	// The pages themselves are asked for as a browser (as the sender's own
	// app, on their own connection), and failing that as a link-preview
	// fetcher, which is how sites recognise the ones chat apps make.
	browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"
	crawlerUA = "facebookexternalhit/1.1 (+http://www.facebook.com/externalhit_uatext.php)"
	redditUA  = "desktop:chriscord-link-preview:1.0 (chat app)"
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
	// What follows a link but isn't part of it (as the page's
	// splitLinkTail): punctuation, and a ")" or "]" it didn't open —
	// wikipedia.org/wiki/Linux_(kernel) keeps its own.
	for m != "" {
		c := m[len(m)-1]
		if strings.IndexByte(".,;:!?'\"*_~", c) >= 0 {
			m = m[:len(m)-1]
			continue
		}
		if (c == ')' && strings.Count(m, "(") < strings.Count(m, ")")) || (c == ']' && strings.Count(m, "[") < strings.Count(m, "]")) || c == '}' {
			m = m[:len(m)-1]
			continue
		}
		break
	}
	if strings.TrimLeft(strings.TrimPrefix(strings.TrimPrefix(m, "https://"), "http://"), "/") == "" {
		return ""
	}
	return m
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
	// As a browser first: many shops (Amazon, eBay) and Reddit turn away
	// anything that doesn't look like one. Turned away anyway (an error, a
	// robot check, a page with nothing to show), once more as a link-preview
	// fetcher, which sites that do keep their previews for (they're made for
	// chat apps' previews, and give their title and picture to those).
	var best *textLinkPreview
	for i, ua := range []string{browserUA, crawlerUA} {
		if i > 0 && ctx.Err() != nil {
			break
		}
		page, base, kind, err := fetchPage(ctx, client, link, ua)
		if kind != "" {
			return &textLinkPreview{U: link, K: kind}, nil
		}
		if err != nil {
			if i == 0 {
				continue
			}
			if best == nil {
				return nil, err
			}
			break
		}
		if page == "" {
			return nil, nil // not a web page (a PDF, say): no preview
		}
		p := pagePreview(ctx, client, link, page, base)
		if p != nil && (best == nil || p.score() > best.score()) {
			best = p
		}
		if best != nil && best.T != "" && best.I != "" {
			break // all a card shows: no need to ask again
		}
	}
	if best == nil || (best.T == "" && best.D == "" && best.I == "" && best.V == "") {
		return nil, nil
	}
	if best.S == "" {
		best.S = strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	}
	return best, nil
}

// How much a preview has to show (to keep the better of two tries).
func (p *textLinkPreview) score() int {
	n := 0
	for _, f := range []string{p.T, p.D, p.I, p.V} {
		if f != "" {
			n++
		}
	}
	if p.I != "" {
		n += 2 // the picture is most of a card
	}
	return n
}

// fetchPage gets a page as ua ("" page and no error: not a web page).
// kind is "image" or "video" when the link is that file itself.
func fetchPage(ctx context.Context, client *http.Client, link, ua string) (page string, base *url.URL, kind string, err error) {
	req, err := http.NewRequestWithContext(ctx, "GET", link, nil)
	if err != nil {
		return "", nil, "", err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", nil, "", fmt.Errorf("the site answered %d", resp.StatusCode)
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch {
	case strings.HasPrefix(ct, "image/"):
		return "", nil, "image", nil
	case strings.HasPrefix(ct, "video/"):
		return "", nil, "video", nil
	case ct != "" && ct != "text/html" && ct != "application/xhtml+xml":
		return "", nil, "", nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, previewMaxPage))
	page = string(body)
	if robotCheck(page) {
		return "", nil, "", errors.New("the site asked for a robot check")
	}
	return page, resp.Request.URL, "", nil
}

var reRobotTitle = regexp.MustCompile(`(?i)robot check|captcha|are you a (robot|human)|access denied|attention required|just a moment|pardon our interruption|security check|verify you are human|unusual traffic`)

// robotCheck: whether page is a site's "are you a robot?" wall instead of
// the page asked for.
func robotCheck(page string) bool {
	if m := reTitleTag.FindStringSubmatch(page); m != nil && reRobotTitle.MatchString(m[1]) {
		return true
	}
	return len(page) < 20000 && strings.Contains(strings.ToLower(page), "captcha")
}

// pagePreview makes a page's preview from what it says about itself: its
// Open Graph and Twitter tags, its structured data (shops' products), and
// failing those its title, first paragraph and main picture.
func pagePreview(ctx context.Context, client *http.Client, link, page string, base *url.URL) *textLinkPreview {
	meta := pageMeta(page)
	get := func(keys ...string) string {
		for _, k := range keys {
			if v := strings.TrimSpace(meta[k]); v != "" {
				return v
			}
		}
		return ""
	}
	ld := pageLD(page)
	p := &textLinkPreview{U: link,
		T: clip(firstOf(get("og:title", "twitter:title"), ld.name, amazonTitle(page)), 200),
		D: clip(firstOf(get("og:description", "twitter:description", "description"), ld.description), 400),
		S: clip(get("og:site_name"), 80)}
	if p.T == "" {
		if m := reTitleTag.FindStringSubmatch(page); m != nil {
			p.T = clip(strings.TrimSpace(reSpaces.ReplaceAllString(html.UnescapeString(m[1]), " ")), 200)
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
	// Its picture: the first of these that loads.
	candidates := []string{get("og:image:secure_url"), get("og:image"), get("og:image:url"), get("twitter:image"), get("twitter:image:src"),
		linkRel(page, "image_src"), ld.image, itempropImage(page), amazonImage(page)}
	seen := map[string]bool{}
	tried := 0
	for _, c := range candidates {
		c = resolveLink(base, html.UnescapeString(strings.TrimSpace(c)))
		if c == "" || seen[c] || strings.HasPrefix(c, "data:") {
			continue
		}
		seen[c] = true
		if tried++; tried > 3 || ctx.Err() != nil {
			break
		}
		if p.I = thumbnailFrom(ctx, client, c, link); p.I != "" {
			break
		}
	}
	if p.T == "" && p.D == "" && p.I == "" && p.V == "" {
		return nil
	}
	return p
}

func firstOf(vs ...string) string {
	for _, v := range vs {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

var (
	reLinkTag     = regexp.MustCompile(`(?is)<link\b[^>]*>`)
	reItemprop    = regexp.MustCompile(`(?is)<(?:meta|img|link)\b[^>]*\bitemprop\s*=\s*["']?image["']?[^>]*>`)
	reLDScript    = regexp.MustCompile(`(?is)<script[^>]*type\s*=\s*["']application/ld\+json["'][^>]*>(.*?)</script>`)
	reAmazonTitle = regexp.MustCompile(`(?is)<span[^>]*id\s*=\s*["']productTitle["'][^>]*>(.*?)</span>`)
	reAmazonImage = regexp.MustCompile(`(?i)(?:data-old-hires\s*=\s*"(https://[^"]+)"|"hiRes"\s*:\s*"(https://[^"]+)"|"large"\s*:\s*"(https://[^"]+)")`)
)

func tagAttrs(tag string) map[string]string {
	attrs := map[string]string{}
	for _, m := range reAttr.FindAllStringSubmatch(tag, -1) {
		attrs[strings.ToLower(m[1])] = html.UnescapeString(m[3] + m[4] + m[5])
	}
	return attrs
}

// linkRel: the href of <link rel="rel"> ("" if none).
func linkRel(page, rel string) string {
	for _, tag := range reLinkTag.FindAllString(page, 300) {
		a := tagAttrs(tag)
		if strings.EqualFold(a["rel"], rel) && a["href"] != "" {
			return a["href"]
		}
	}
	return ""
}

// itempropImage: a microdata picture (itemprop="image").
func itempropImage(page string) string {
	for _, tag := range reItemprop.FindAllString(page, 10) {
		a := tagAttrs(tag)
		if v := firstOf(a["content"], a["src"], a["href"]); v != "" {
			return v
		}
	}
	return ""
}

func amazonTitle(page string) string {
	if m := reAmazonTitle.FindStringSubmatch(page); m != nil {
		return strings.TrimSpace(reSpaces.ReplaceAllString(html.UnescapeString(reTags.ReplaceAllString(m[1], "")), " "))
	}
	return ""
}

// amazonImage: an Amazon product's main picture (its pages have no
// og:image).
func amazonImage(page string) string {
	if m := reAmazonImage.FindStringSubmatch(page); m != nil {
		return firstOf(m[1], m[2], m[3])
	}
	return ""
}

type ldInfo struct{ name, description, image string }

// pageLD: a page's structured data (JSON-LD) — a shop's product, an
// article: its name, description and picture, the first of each found.
func pageLD(page string) ldInfo {
	var out ldInfo
	var walk func(v interface{}, depth int)
	str := func(v interface{}) string {
		switch x := v.(type) {
		case string:
			return x
		case []interface{}:
			for _, e := range x {
				if s := imageOf(e); s != "" {
					return s
				}
			}
		case map[string]interface{}:
			return imageOf(x)
		}
		return ""
	}
	walk = func(v interface{}, depth int) {
		if depth > 6 {
			return
		}
		switch x := v.(type) {
		case []interface{}:
			for _, e := range x {
				walk(e, depth+1)
			}
		case map[string]interface{}:
			if g, ok := x["@graph"]; ok {
				walk(g, depth+1)
			}
			typ := strings.ToLower(fmt.Sprint(x["@type"]))
			if strings.Contains(typ, "breadcrumb") || strings.Contains(typ, "organization") || strings.Contains(typ, "website") {
				return // the site's own logo and name, not the page's
			}
			if s, ok := x["name"].(string); ok && out.name == "" {
				out.name = s
			} else if s, ok := x["headline"].(string); ok && out.name == "" {
				out.name = s
			}
			if s, ok := x["description"].(string); ok && out.description == "" {
				out.description = html.UnescapeString(reTags.ReplaceAllString(s, ""))
			}
			if out.image == "" {
				out.image = str(x["image"])
				if out.image == "" {
					out.image = str(x["thumbnailUrl"])
				}
			}
		}
	}
	for _, m := range reLDScript.FindAllStringSubmatch(page, 10) {
		var v interface{}
		if json.Unmarshal([]byte(strings.TrimSpace(m[1])), &v) == nil {
			walk(v, 0)
		}
	}
	return out
}

// imageOf: a picture's address, from JSON-LD's ways of giving one.
func imageOf(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case map[string]interface{}:
		for _, k := range []string{"url", "contentUrl", "@id"} {
			if s, ok := x[k].(string); ok && strings.HasPrefix(s, "http") {
				return s
			}
		}
	}
	return ""
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
func thumbnailFrom(ctx context.Context, client *http.Client, link string, page ...string) string {
	if link == "" {
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, "GET", jpegVersion(link), nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", browserUA)
	// Not WebP or AVIF, which Go can't read: sites that pick the format by
	// what's asked for (most picture servers) send JPEG or PNG instead.
	req.Header.Set("Accept", "image/jpeg,image/png,image/gif;q=0.9,image/*;q=0.5")
	if len(page) > 0 && page[0] != "" {
		req.Header.Set("Referer", page[0]) // some refuse pictures to anyone else
	}
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

var (
	reEbayImage = regexp.MustCompile(`(?i)^(https://i\.ebayimg\.com/.+/s-l\d+)\.(webp|avif)$`)
	reFormatArg = regexp.MustCompile(`(?i)([?&](?:format|fm))=(webp|avif)\b`)
)

// jpegVersion: the JPEG of a picture that's named as WebP or AVIF, where the
// site makes one (eBay, Reddit's previews, most image CDNs' format=).
func jpegVersion(link string) string {
	if m := reEbayImage.FindStringSubmatch(link); m != nil {
		return m[1] + ".jpg"
	}
	if strings.Contains(link, "redd.it/") {
		return reFormatArg.ReplaceAllString(link, "${1}=pjpg") // Reddit's name for it
	}
	return reFormatArg.ReplaceAllString(link, "${1}=jpg")
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

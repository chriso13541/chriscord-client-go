package main

// search.go — searching messages the server can't read.
//
// With end-to-end encryption the server can't match words (or file names
// and types), so this app does: it asks the server for messages newest
// first, a page at a time (/api/search/scan), already narrowed down by what
// the server can still see — who sent it, which channel, when, whether it
// has attachments — then decrypts each page here and matches the text and
// attachments itself. It stops once it has a screenful of matches (or has
// spent a few seconds), and carries on from there when asked ("Search
// older").
//
// Nothing is written to disk. While the search panel is open, the pages
// already decrypted are kept in memory, so refining the words ("photo" →
// "photo beach") with the same filters doesn't download them again; they're
// dropped when search closes (ClearSearch) or after searchCacheIdle unused.

import (
	"errors"
	"log"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

const (
	searchPage      = 500             // messages per page from the server
	searchWant      = 25              // matches per answer, unless asked for another number
	searchBudget    = 4 * time.Second // most time one answer spends fetching
	searchCacheIdle = 5 * time.Minute // forget decrypted pages after this long unused
	searchCacheMax  = 200_000         // most decrypted messages kept at once
)

// SearchPage is one answer: matches newest first, and where to carry on.
type SearchPage struct {
	Results []SearchResult `json:"results"`
	// Next is where "Search older" continues from; -1 when everything has
	// been searched.
	Next int `json:"next"`
	// Searched is how many messages have been looked at so far, and
	// OldestAt the date of the oldest of them — for "searched back to …".
	Searched int    `json:"searched"`
	OldestAt string `json:"oldest_at"`
}

type searchCache struct {
	sig    string         // the server-side filters these pages are for
	msgs   []SearchResult // decrypted, newest first
	cursor string         // the server's cursor for the next page
	done   bool           // nothing older on the server
	used   time.Time
}

var (
	searchMu sync.Mutex
	search   *searchCache
)

type scanResp struct {
	Messages []SearchResult `json:"messages"`
	Next     *string        `json:"next"`
}

// SearchMessages finds messages matching text and the filters (from: user,
// has: comma-separated link/image/video/audio/file, in: channel id,
// after/before: RFC 3339 instants), newest first, starting at offset (0 for
// a new search; a previous answer's Next to carry on). want is how many
// matches to stop at (0 for the default).
func (a *App) SearchMessages(text, from, has, inBoard, after, before string, offset, want int) (*SearchPage, error) {
	if want <= 0 {
		want = searchWant
	}
	q := parseQuery(text)
	hasList := splitHas(has)
	sig := strings.Join([]string{from, strings.Join(hasList, ","), inBoard, after, before}, "\x00")

	searchMu.Lock()
	defer searchMu.Unlock()
	if search == nil || search.sig != sig || time.Since(search.used) > searchCacheIdle {
		search = &searchCache{sig: sig}
		offset = 0
	}
	c := search
	c.used = time.Now()

	page := &SearchPage{Results: []SearchResult{}}
	start := time.Now()
	i := offset
	for len(page.Results) < want {
		if i >= len(c.msgs) {
			if c.done {
				break
			}
			if time.Since(start) > searchBudget || len(c.msgs) >= searchCacheMax {
				break // enough for now: "Search older" carries on
			}
			if err := a.searchFetch(c, from, has, inBoard, after, before); err != nil {
				if len(page.Results) == 0 && offset == 0 && isNotFound(err) {
					searchMu.Unlock()
					res, ferr := a.searchOld(text, from, has, inBoard, after, before)
					searchMu.Lock()
					return res, ferr
				}
				return nil, err
			}
			continue
		}
		m := c.msgs[i]
		i++
		if matchesSearch(&m, q, hasList) {
			page.Results = append(page.Results, m)
		}
	}
	page.Searched = i
	if i > 0 && i <= len(c.msgs) {
		page.OldestAt = c.msgs[i-1].CreatedAt
	}
	page.Next = i
	if c.done && i >= len(c.msgs) {
		page.Next = -1
	}
	return page, nil
}

// searchFetch gets the next page from the server and decrypts it.
func (a *App) searchFetch(c *searchCache, from, has, inBoard, after, before string) error {
	v := url.Values{}
	v.Set("limit", strconv.Itoa(searchPage))
	for k, val := range map[string]string{"from": from, "has": has, "in": inBoard, "after": after, "before": before, "cursor": c.cursor} {
		if val != "" {
			v.Set(k, val)
		}
	}
	var r scanResp
	if err := a.doJSON("GET", "/api/search/scan?"+v.Encode(), nil, &r); err != nil {
		return err
	}
	for i := range r.Messages {
		a.text.open(&r.Messages[i].ChatMessage)
	}
	c.msgs = append(c.msgs, r.Messages...)
	if r.Next == nil || *r.Next == "" || len(r.Messages) == 0 {
		c.done = true
	} else {
		c.cursor = *r.Next
	}
	return nil
}

// searchOld is for servers from before /api/search/scan: their own search
// (which can only find unencrypted messages), newest first.
func (a *App) searchOld(text, from, has, inBoard, after, before string) (*SearchPage, error) {
	res, err := a.SearchServerFiltered(text, from, has, inBoard, after, before)
	if err != nil {
		return nil, err
	}
	sortNewestFirst(res)
	return &SearchPage{Results: res, Next: -1, Searched: len(res)}, nil
}

func sortNewestFirst(rs []SearchResult) {
	// Ids are UUIDv7: they sort by time.
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j].ID > rs[j-1].ID; j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}

func isNotFound(err error) bool {
	var h *httpError
	return errors.As(err, &h) && h.code == 404
}

// ClearSearch forgets the decrypted pages (the search panel closed).
func (a *App) ClearSearch() {
	searchMu.Lock()
	search = nil
	searchMu.Unlock()
}

// ── Matching ──────────────────────────────────────────────────────────────────

// searchQuery: every word has to appear in a message, and every "quoted
// phrase" as those words in a row — whole words, ignoring case and accents
// (like the server's own search did).
type searchQuery struct {
	words   []string
	phrases [][]string
}

func (q searchQuery) empty() bool { return len(q.words) == 0 && len(q.phrases) == 0 }

func parseQuery(text string) searchQuery {
	var q searchQuery
	rest := text
	for {
		rest = strings.TrimLeft(rest, " \t\r\n")
		if rest == "" {
			return q
		}
		var piece string
		quoted := false
		if after, ok := strings.CutPrefix(rest, `"`); ok {
			quoted = true
			if end := strings.IndexByte(after, '"'); end >= 0 {
				piece, rest = after[:end], after[end+1:]
			} else {
				piece, rest = after, ""
			}
		} else {
			end := strings.IndexFunc(rest, unicode.IsSpace)
			if end < 0 {
				end = len(rest)
			}
			piece, rest = rest[:end], rest[end:]
		}
		toks := tokens(piece)
		switch {
		case len(toks) == 0:
		case quoted && len(toks) > 1:
			q.phrases = append(q.phrases, toks)
		default:
			q.words = append(q.words, toks...) // "don't" → don, t, as the old index did
		}
	}
}

// tokens splits text into lowercase, accent-free words.
func tokens(s string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	for _, r := range norm.NFD.String(s) {
		switch {
		case unicode.Is(unicode.Mn, r): // an accent: dropped
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
		default:
			flush()
		}
	}
	flush()
	return out
}

func matchesSearch(m *SearchResult, q searchQuery, has []string) bool {
	if m.E2EE == "locked" || m.E2EE == "error" {
		return false // nothing readable to match
	}
	for _, h := range has {
		if !hasKind(m, h) {
			return false
		}
	}
	if q.empty() {
		return true
	}
	var parts []string
	parts = append(parts, m.Content)
	for _, at := range m.Attachments {
		parts = append(parts, at.Name)
	}
	toks := tokens(strings.Join(parts, "\n"))
	set := make(map[string]bool, len(toks))
	for _, t := range toks {
		set[t] = true
	}
	for _, w := range q.words {
		if !set[w] {
			return false
		}
	}
	for _, p := range q.phrases {
		if !hasRun(toks, p) {
			return false
		}
	}
	return true
}

func hasRun(toks, phrase []string) bool {
	for i := 0; i+len(phrase) <= len(toks); i++ {
		ok := true
		for j, w := range phrase {
			if toks[i+j] != w {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func hasKind(m *SearchResult, kind string) bool {
	switch kind {
	case "link":
		c := strings.ToLower(m.Content)
		return strings.Contains(c, "http://") || strings.Contains(c, "https://")
	case "file", "attachment":
		return len(m.Attachments) > 0
	case "image", "video", "audio", "sound":
		prefix := kind
		if kind == "sound" {
			prefix = "audio"
		}
		for _, at := range m.Attachments {
			if strings.HasPrefix(at.Mime, prefix+"/") {
				return true
			}
		}
		return false
	}
	log.Printf("search: unknown has:%s", kind)
	return false
}

func splitHas(has string) []string {
	var out []string
	for _, h := range strings.Split(has, ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			out = append(out, h)
		}
	}
	return out
}

package main

// search.go — searching messages the server can't read.
//
// With end-to-end encryption the server can't match words (or file names
// and types), so this app does. It looks through this computer's copy of
// the server's history first (history.go) — brought up to date before each
// search — then, past the oldest message the copy has, asks the server for
// older messages newest first (/api/search/scan), already narrowed down by
// what the server can still see (who sent it, which channel, when, whether
// it has attachments), and decrypts and matches those here. A search for
// just words goes back by growing the copy, so the next one starts further
// back.
//
// Each call does a little (searchStep) and reports how far it got, so the
// search box can show progress while it carries on; the page keeps calling
// until it has a screenful of results, everything has been searched, or
// you stop it (StopSearch).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/text/unicode/norm"
)

const (
	searchPage      = 500                    // messages per page from the server, when it's narrowing them down
	histPage        = 1000                   // messages per page when copying history
	searchWant      = 25                     // matches per answer, unless asked for another number
	searchStep      = 700 * time.Millisecond // about how long one call works before reporting back
	searchCacheIdle = 5 * time.Minute        // forget a search (and pages fetched for it) after this long unused
	searchCacheMax  = 200_000                // most fetched messages kept for one set of filters
	histRetryEvery  = 10 * time.Minute       // how often messages that couldn't be read are tried again
	authorsFresh    = 2 * time.Minute
)

// SearchPage is one answer: matches newest first, and how far it got.
type SearchPage struct {
	Results []SearchResult `json:"results"`
	// Next continues this search (pass it back to SearchMessages); "" when
	// everything has been searched.
	Next string `json:"next"`
	// Searched is how many messages (of those the filters cover) have been
	// looked at so far, of Total (-1 if the server can't say); OldestAt is
	// when the oldest of them was sent — for "searched back to …".
	Searched int    `json:"searched"`
	Total    int    `json:"total"`
	OldestAt string `json:"oldest_at"`
	// Copy: this computer's copy of the history was used.
	Copy bool `json:"copy"`
}

// serverRef is the server a search or copy talks to, fixed when it starts,
// so switching servers halfway can't mix one's history into another's.
type serverRef struct{ domain, token string }

func (a *App) serverRef() serverRef {
	a.mu.Lock()
	defer a.mu.Unlock()
	return serverRef{a.domain, a.token}
}

// getJSON is doJSON's GET, to a given server.
func (r serverRef) getJSON(path string, out interface{}) error {
	req, err := http.NewRequest("GET", normaliseHTTP(r.domain)+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Session-Token", r.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("network error: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("network error: %w", err)
	}
	if resp.StatusCode >= 400 {
		var e map[string]string
		if json.Unmarshal(body, &e) == nil && e["error"] != "" {
			return &httpError{resp.StatusCode, e["error"]}
		}
		return &httpError{resp.StatusCode, fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	return json.NewDecoder(bytes.NewReader(body)).Decode(out)
}

type scanResp struct {
	Messages []SearchResult `json:"messages"`
	Next     *string        `json:"next"`
	Total    *int           `json:"total"`
}

func (r serverRef) scan(v url.Values) (*scanResp, error) {
	var out scanResp
	if err := r.getJSON("/api/search/scan?"+v.Encode(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func scanFilters(from, has, inBoard, after, before string) url.Values {
	v := url.Values{}
	for k, val := range map[string]string{"from": from, "has": has, "in": inBoard, "after": after, "before": before} {
		if val != "" {
			v.Set(k, val)
		}
	}
	return v
}

// ── Searching ────────────────────────────────────────────────────────────────

type searchRun struct {
	id                string
	ref               serverRef
	q                 searchQuery
	has               []string
	from              string
	inBoard           string
	afterMs, beforeMs int64
	needsFiles        bool // has: something other than link: only messages with attachments
	filters           url.Values

	h           *localHistory // nil: no copy
	localCursor string        // the last message in the copy looked at
	localDone   bool
	// extend: nothing narrows the search down on the server, so going past
	// the copy's oldest message is the same as copying more history — and
	// that's what's done, keeping it.
	extend bool

	remote    *remotePages
	remoteIdx int

	searched int
	total    int
	oldest   int64
	used     time.Time
}

// remotePages: messages fetched (and decrypted) for one set of filters,
// from where the copy ends, kept while searching so refining the words
// doesn't fetch them again.
type remotePages struct {
	sig    string
	msgs   []SearchResult // newest first
	cursor string
	done   bool
}

var (
	searchMu  sync.Mutex
	searchCur *searchRun
	remoteCur *remotePages
	runSeq    int

	stopMu     sync.Mutex
	stopSearch context.CancelFunc
	searchCtx  context.Context
)

// SearchMessages finds messages matching text and the filters (from: user,
// has: comma-separated link/image/video/audio/file, in: channel id,
// after/before: RFC 3339 instants), newest first. cont is "" for a new
// search, or a previous answer's Next to carry on; want is how many
// matches to stop at (0 for the default).
func (a *App) SearchMessages(text, from, has, inBoard, after, before, cont string, want int) (*SearchPage, error) {
	if want <= 0 {
		want = searchWant
	}
	var ctx context.Context
	stopMu.Lock()
	if cont == "" || searchCtx == nil || searchCtx.Err() != nil {
		if stopSearch != nil {
			stopSearch() // a new search replaces the one before
		}
		// (Carrying on after Stop: Stop was for the part that was running.)
		searchCtx, stopSearch = context.WithCancel(context.Background())
	}
	ctx = searchCtx
	stopMu.Unlock()

	searchMu.Lock()
	defer searchMu.Unlock()
	run := searchCur
	if cont != "" && (run == nil || run.id != cont || time.Since(run.used) > searchCacheIdle) {
		return nil, errors.New("that search has ended — search again")
	}
	if cont == "" {
		var err error
		run, err = a.newSearchRun(ctx, text, from, has, inBoard, after, before)
		if err != nil {
			if isNotFound(err) {
				return a.searchOld(text, from, has, inBoard, after, before)
			}
			return nil, err
		}
		searchCur = run
	}
	run.used = time.Now()
	return a.searchStep(ctx, run, want)
}

// StopSearch stops the search that's running (the page's Stop button).
func (a *App) StopSearch() {
	stopMu.Lock()
	if stopSearch != nil {
		stopSearch()
	}
	stopMu.Unlock()
}

// ClearSearch forgets the search and what was fetched for it (the search
// panel closed). This computer's copy stays.
func (a *App) ClearSearch() {
	a.StopSearch()
	searchMu.Lock()
	searchCur, remoteCur = nil, nil
	searchMu.Unlock()
}

func (a *App) newSearchRun(ctx context.Context, text, from, has, inBoard, after, before string) (*searchRun, error) {
	runSeq++
	run := &searchRun{
		id: strconv.Itoa(runSeq), ref: a.serverRef(), q: parseQuery(text), has: splitHas(has),
		from: from, inBoard: inBoard, afterMs: parseMillis(after), beforeMs: parseMillis(before),
		filters: scanFilters(from, has, inBoard, after, before), total: -1,
	}
	for _, k := range run.has {
		if k != "link" {
			run.needsFiles = true
		}
	}
	run.extend = from == "" && inBoard == "" && after == "" && before == "" && !run.needsFiles

	if h := a.searchCopy(); h != nil {
		err := a.copyUpdate(ctx, run.ref, h, func(stage string, n int) {
			a.emit("search:progress", map[string]interface{}{"stage": stage, "done": n})
		})
		switch {
		case err == nil:
			run.h = h
		case errors.Is(err, errCopyUnsupported):
		case ctx.Err() != nil:
			return nil, ctx.Err()
		default:
			// Searching without it still works; the copy catches up next time.
			log.Printf("search copy: couldn't bring it up to date: %v", err)
			run.h = h
		}
	}
	// How many messages the filters cover, for the progress bar.
	v := url.Values{}
	for k, val := range run.filters {
		v[k] = val
	}
	v.Set("count", "1")
	v.Set("limit", "1")
	r, err := run.ref.scan(v)
	if err != nil {
		if run.h == nil && isNotFound(err) {
			return nil, err // a server from before /api/search/scan
		}
	} else if r.Total != nil {
		run.total = *r.Total
	}
	return run, nil
}

// passes: whether a message in the copy is one the server would have
// handed over for these filters.
func (run *searchRun) passes(h *localHistory, r *histRec) bool {
	return h.boards[r.B] &&
		(run.from == "" || strings.EqualFold(r.U, run.from)) &&
		(run.inBoard == "" || r.B == run.inBoard) &&
		(run.afterMs == 0 || r.T >= run.afterMs) &&
		(run.beforeMs == 0 || r.T < run.beforeMs) &&
		(!run.needsFiles || r.K&hasFile != 0)
}

func (a *App) searchStep(ctx context.Context, run *searchRun, want int) (*SearchPage, error) {
	start := time.Now()
	var hits []string        // matches in the copy, shown by fetching them
	var found []SearchResult // matches fetched from the server
	done := false
	for !done && len(hits)+len(found) < want && time.Since(start) < searchStep {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if run.h != nil && !run.localDone {
			h := run.h
			h.mu.Lock()
			if err := h.load(); err != nil {
				h.mu.Unlock()
				return nil, err
			}
			i := h.after(run.localCursor)
			for ; i < len(h.recs) && len(hits)+len(found) < want; i++ {
				if i%4096 == 0 && time.Since(start) > searchStep {
					break
				}
				r := &h.recs[i]
				run.localCursor = r.ID
				if !run.passes(h, r) {
					continue
				}
				run.searched++
				run.oldest = r.T
				if r.matches(run.q, run.has) {
					hits = append(hits, r.ID)
				}
			}
			atEnd := i >= len(h.recs)
			complete, tail := h.st.Complete, h.st.Tail
			h.mu.Unlock()
			if !atEnd {
				continue
			}
			switch {
			case complete:
				run.localDone, done = true, true
			case run.extend:
				n, err := a.copyBackfill(ctx, run.ref, h)
				if err != nil {
					return nil, err
				}
				if n == 0 { // (it should have been complete)
					run.localDone, done = true, true
				}
			default:
				run.localDone = true
				run.remote = remoteFor(run, tail)
			}
			continue
		}
		// Past the copy (or without one): from the server.
		if run.remote == nil {
			run.remote = remoteFor(run, "")
		}
		rp := run.remote
		if run.remoteIdx >= len(rp.msgs) {
			if rp.done {
				done = true
				break
			}
			if len(rp.msgs) >= searchCacheMax {
				rp.msgs, run.remoteIdx = nil, 0 // let go of what's been searched
			}
			if err := run.fetchRemote(a, rp); err != nil {
				return nil, err
			}
			continue
		}
		m := rp.msgs[run.remoteIdx]
		run.remoteIdx++
		run.searched++
		run.oldest = parseMillis(m.CreatedAt)
		if matchesSearch(&m, run.q, run.has) {
			found = append(found, m)
		}
	}

	page := &SearchPage{Results: []SearchResult{}, Searched: run.searched, Total: run.total, Copy: run.h != nil, Next: run.id}
	if len(hits) > 0 {
		got, err := a.fetchMessages(run.ref, run.h, hits)
		if err != nil {
			return nil, err
		}
		page.Results = append(page.Results, got...)
	}
	page.Results = append(page.Results, found...)
	if run.oldest > 0 {
		page.OldestAt = time.UnixMilli(run.oldest).UTC().Format(time.RFC3339)
	}
	if done {
		page.Next = ""
		if run.total >= 0 && run.searched > run.total {
			page.Total = run.searched
		}
	}
	return page, nil
}

// remoteFor: the pages fetched for this search's filters, starting below
// from (the copy's oldest message, or "" for the newest) — kept from an
// earlier search with the same filters if there is one.
func remoteFor(run *searchRun, from string) *remotePages {
	sig := run.ref.domain + "\x00" + run.filters.Encode() + "\x00" + from
	if remoteCur == nil || remoteCur.sig != sig {
		remoteCur = &remotePages{sig: sig, cursor: from}
	}
	run.remoteIdx = 0
	return remoteCur
}

// fetchRemote gets the next page from the server and decrypts it.
func (run *searchRun) fetchRemote(a *App, rp *remotePages) error {
	v := url.Values{}
	for k, val := range run.filters {
		v[k] = val
	}
	v.Set("limit", strconv.Itoa(searchPage))
	if rp.cursor != "" {
		v.Set("cursor", rp.cursor)
	}
	r, err := run.ref.scan(v)
	if err != nil {
		return err
	}
	for i := range r.Messages {
		a.text.open(&r.Messages[i].ChatMessage)
	}
	rp.msgs = append(rp.msgs, r.Messages...)
	if r.Next == nil || *r.Next == "" || len(r.Messages) == 0 {
		rp.done = true
	} else {
		rp.cursor = *r.Next
	}
	return nil
}

// fetchMessages gets matches found in the copy from the server, to show
// them as they are now (with their files). Any that are gone are dropped
// from the copy.
func (a *App) fetchMessages(ref serverRef, h *localHistory, ids []string) ([]SearchResult, error) {
	var out []SearchResult
	for len(ids) > 0 {
		chunk := ids[:min(len(ids), 100)]
		ids = ids[len(chunk):]
		v := url.Values{}
		v.Set("ids", strings.Join(chunk, ","))
		v.Set("limit", strconv.Itoa(len(chunk)))
		r, err := ref.scan(v)
		if err != nil {
			return nil, err
		}
		got := map[string]bool{}
		for i := range r.Messages {
			a.text.open(&r.Messages[i].ChatMessage)
			got[r.Messages[i].ID] = true
		}
		out = append(out, r.Messages...)
		var gone []string
		for _, id := range chunk {
			if !got[id] {
				gone = append(gone, id)
			}
		}
		if len(gone) > 0 && h != nil {
			h.mu.Lock()
			if h.loaded {
				if err := h.commit(&histOp{Del: gone}); err != nil {
					log.Printf("search copy: %v", err)
				}
			}
			h.mu.Unlock()
		}
	}
	sortNewestFirst(out)
	return out, nil
}

// searchOld is for servers from before /api/search/scan: their own search
// (which can only find unencrypted messages), newest first.
func (a *App) searchOld(text, from, has, inBoard, after, before string) (*SearchPage, error) {
	res, err := a.SearchServerFiltered(text, from, has, inBoard, after, before)
	if err != nil {
		return nil, err
	}
	sortNewestFirst(res)
	return &SearchPage{Results: res, Searched: len(res), Total: len(res)}, nil
}

func sortNewestFirst(rs []SearchResult) {
	// Ids are UUIDv7: they sort by time.
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].ID > rs[j].ID })
}

func isNotFound(err error) bool {
	var h *httpError
	return errors.As(err, &h) && h.code == 404
}

// emit sends an event to the page (when there is one: not in tests).
func (a *App) emit(name string, data interface{}) {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, name, data)
	}
}

// ── Everyone who ever wrote, for from: ───────────────────────────────────────

// SearchAuthor is someone who has written in a channel you can read (or a
// member who hasn't yet).
type SearchAuthor struct {
	Username string `json:"username"`
	Messages int    `json:"messages"`
	LastAt   string `json:"last_at"`
	Imported bool   `json:"imported"` // history brought over from elsewhere, not claimed
	Member   bool   `json:"member"`
}

var (
	authorsMu   sync.Mutex
	authorsFor  string
	authorsAt   time.Time
	authorsList []SearchAuthor
)

// SearchAuthors lists everyone who has ever written in the channels you can
// read, those who wrote most recently first — for from: suggestions, so
// someone who last wrote years ago is there too. Empty from a server that
// can't list them (the page falls back to who it knows of).
func (a *App) SearchAuthors() ([]SearchAuthor, error) {
	ref := a.serverRef()
	authorsMu.Lock()
	defer authorsMu.Unlock()
	if authorsFor == ref.domain && time.Since(authorsAt) < authorsFresh {
		return authorsList, nil
	}
	var r struct {
		Authors []SearchAuthor `json:"authors"`
	}
	if err := ref.getJSON("/api/search/authors", &r); err != nil {
		if isNotFound(err) {
			return []SearchAuthor{}, nil
		}
		return nil, err
	}
	if r.Authors == nil {
		r.Authors = []SearchAuthor{}
	}
	authorsFor, authorsAt, authorsList = ref.domain, time.Now(), r.Authors
	return r.Authors, nil
}

// ── Keeping the copy ─────────────────────────────────────────────────────────

var (
	histMu      sync.Mutex
	histCur     *localHistory
	unsupported = map[string]bool{} // servers without /api/search/changes (yet)
)

var errCopyUnsupported = errors.New("this server can't keep a copy up to date (it needs updating)")

// copyPath: where the copy for the signed-in account and current server
// goes ("" if there's no server or account).
func (a *App) copyPath() (path, domain string, acct *Account) {
	a.mu.Lock()
	acct, domain = a.account, a.domain
	a.mu.Unlock()
	if acct == nil || domain == "" {
		return "", "", nil
	}
	return filepath.Join(historyDir(acct.Slug), historyName(domain)+".copy"), domain, acct
}

// searchCopy: the copy for this account and server, or nil when it's
// switched off (or can't be made).
func (a *App) searchCopy() *localHistory {
	path, domain, acct := a.copyPath()
	if path == "" {
		return nil
	}
	histMu.Lock()
	defer histMu.Unlock()
	if unsupported[normaliseDomain(domain)] {
		return nil
	}
	if _, err := os.Stat(path + ".off"); err == nil {
		return nil
	}
	if histCur != nil && histCur.path == path {
		return histCur
	}
	key, err := historyKey(acct.PrivateKey.Seed(), domain)
	if err != nil {
		return nil
	}
	h, err := newLocalHistory(path, key)
	if err != nil {
		return nil
	}
	if old := histCur; old != nil {
		go func() { // another server's: out of memory, unless it's downloading
			old.mu.Lock()
			if old.keep == 0 && old.loaded {
				old.unload()
			}
			old.mu.Unlock()
		}()
	}
	histCur = h
	return h
}

type changesResp struct {
	Changes []struct {
		Seq          int64         `json:"seq"`
		Kind         string        `json:"kind"`
		ID           string        `json:"id"`
		BoardID      string        `json:"board_id"`
		ImportedFrom string        `json:"imported_from"`
		Username     string        `json:"username"`
		Message      *SearchResult `json:"message"`
	} `json:"changes"`
	Last   int64    `json:"last"`
	More   bool     `json:"more"`
	Reset  bool     `json:"reset"`
	Boards []string `json:"boards"`
}

func (st histState) clone() histState {
	c := st
	c.Boards = append([]string(nil), st.Boards...)
	if st.Refill != nil {
		c.Refill = make(map[string]string, len(st.Refill))
		for k, v := range st.Refill {
			c.Refill[k] = v
		}
	}
	return c
}

func (st *histState) covers(id string) bool {
	return st.Head != "" && id <= st.Head && (st.Complete || id >= st.Tail)
}

func (a *App) recsOf(ms []SearchResult) []histRec {
	out := make([]histRec, 0, len(ms))
	for i := range ms {
		out = append(out, a.text.recOf(&ms[i]))
	}
	return out
}

// copyUpdate brings the copy up to date: what changed in the history it
// has, new messages since, channels to fetch again, and messages that
// couldn't be read before. A new copy starts with the newest page.
// report, if set, hears how it's going ("updating", messages so far).
func (a *App) copyUpdate(ctx context.Context, ref serverRef, h *localHistory, report func(stage string, n int)) error {
	h.syncMu.Lock()
	defer h.syncMu.Unlock()
	h.mu.Lock()
	if !h.loaded && report != nil {
		report("opening", 0)
	}
	err := h.load()
	st := h.st.clone()
	h.mu.Unlock()
	if err != nil {
		return err
	}
	commit := func(op *histOp) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if err := h.load(); err != nil {
			return err
		}
		return h.commit(op)
	}
	n := 0
	note := func(k int) {
		n += k
		if report != nil && n > 0 {
			report("updating", n)
		}
	}

	fresh := st.Head == ""
	for !fresh {
		var cr changesResp
		if err := ref.getJSON("/api/search/changes?after="+strconv.FormatInt(st.Seq, 10), &cr); err != nil {
			if isNotFound(err) {
				return a.markUnsupported(ref)
			}
			return err
		}
		if cr.Reset {
			fresh = true
			break
		}
		readable := map[string]bool{}
		for _, b := range cr.Boards {
			readable[b] = true
		}
		nst := st.clone()
		op := &histOp{State: &nst}
		for _, c := range cr.Changes {
			switch c.Kind {
			case "edit":
				if c.Message == nil {
					op.Del = append(op.Del, c.ID)
				} else if readable[c.BoardID] && st.covers(c.ID) {
					op.Put = append(op.Put, a.text.recOf(c.Message))
				}
			case "delete":
				op.Del = append(op.Del, c.ID)
			case "board":
				if nst.Refill == nil {
					nst.Refill = map[string]string{}
				}
				nst.Refill[c.BoardID] = ""
			case "board_gone":
				op.Drop = append(op.Drop, c.BoardID)
				delete(nst.Refill, c.BoardID)
			case "author":
				op.Author = append(op.Author, [2]string{c.ImportedFrom, c.Username})
			}
		}
		// Channels you can't read any more: let go of them.
		h.mu.Lock()
		for b := range h.boards {
			if !readable[b] {
				op.Drop = append(op.Drop, b)
				delete(nst.Refill, b)
			}
		}
		h.mu.Unlock()
		nst.Seq, nst.Boards = cr.Last, cr.Boards
		if err := commit(op); err != nil {
			return err
		}
		note(len(op.Put))
		st = nst
		if !cr.More {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	if fresh {
		var cr changesResp
		if err := ref.getJSON("/api/search/changes", &cr); err != nil {
			if isNotFound(err) {
				return a.markUnsupported(ref)
			}
			return err
		}
		v := url.Values{}
		v.Set("limit", strconv.Itoa(histPage))
		r, err := ref.scan(v)
		if err != nil {
			return err
		}
		nst := histState{Seq: cr.Last, Boards: cr.Boards, Complete: r.Next == nil || len(r.Messages) == 0}
		if len(r.Messages) > 0 {
			nst.Head, nst.Tail = r.Messages[0].ID, r.Messages[len(r.Messages)-1].ID
		}
		if err := commit(&histOp{Reset: true, Put: a.recsOf(r.Messages), State: &nst}); err != nil {
			return err
		}
		note(len(r.Messages))
		return nil
	}

	// New messages since.
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		v := url.Values{}
		v.Set("since", st.Head)
		v.Set("limit", strconv.Itoa(histPage))
		r, err := ref.scan(v)
		if err != nil {
			return err
		}
		if len(r.Messages) == 0 {
			break
		}
		nst := st.clone()
		nst.Head = r.Messages[len(r.Messages)-1].ID // oldest first: the last is the newest
		if err := commit(&histOp{Put: a.recsOf(r.Messages), State: &nst}); err != nil {
			return err
		}
		note(len(r.Messages))
		st = nst
		if r.Next == nil {
			break
		}
	}

	// Channels whose history was imported into: fetched again.
	boards := make([]string, 0, len(st.Refill))
	for b := range st.Refill {
		boards = append(boards, b)
	}
	sort.Strings(boards)
	for _, b := range boards {
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			v := url.Values{}
			v.Set("in", b)
			v.Set("limit", strconv.Itoa(histPage))
			if c := st.Refill[b]; c != "" {
				v.Set("cursor", c)
			}
			r, err := ref.scan(v)
			if err != nil {
				return err
			}
			var keep []SearchResult
			for _, m := range r.Messages {
				if st.covers(m.ID) {
					keep = append(keep, m)
				}
			}
			nst := st.clone()
			last := ""
			if len(r.Messages) > 0 {
				last = r.Messages[len(r.Messages)-1].ID
			}
			if r.Next == nil || last == "" || (!st.Complete && last < st.Tail) {
				delete(nst.Refill, b)
			} else {
				nst.Refill[b] = last
			}
			if err := commit(&histOp{Put: a.recsOf(keep), State: &nst}); err != nil {
				return err
			}
			note(len(keep))
			st = nst
			if _, more := st.Refill[b]; !more {
				break
			}
		}
	}

	// Messages that couldn't be read before (their keys may have come since).
	if time.Since(time.Unix(st.Retried, 0)) > histRetryEvery {
		h.mu.Lock()
		var ids []string
		for i := range h.recs {
			if h.recs[i].S != 0 {
				ids = append(ids, h.recs[i].ID)
				if len(ids) == 500 {
					break
				}
			}
		}
		h.mu.Unlock()
		nst := st.clone()
		nst.Retried = time.Now().Unix()
		op := &histOp{State: &nst}
		for len(ids) > 0 {
			chunk := ids[:min(len(ids), 100)]
			ids = ids[len(chunk):]
			v := url.Values{}
			v.Set("ids", strings.Join(chunk, ","))
			v.Set("limit", strconv.Itoa(len(chunk)))
			r, err := ref.scan(v)
			if err != nil {
				return err
			}
			got := map[string]bool{}
			for _, m := range r.Messages {
				got[m.ID] = true
			}
			for _, id := range chunk {
				if !got[id] {
					op.Del = append(op.Del, id)
				}
			}
			op.Put = append(op.Put, a.recsOf(r.Messages)...)
		}
		if err := commit(op); err != nil {
			return err
		}
	}
	return nil
}

// copyBackfill copies the next page of older history into the copy. It
// says how many messages that was (0 once it has everything).
func (a *App) copyBackfill(ctx context.Context, ref serverRef, h *localHistory) (int, error) {
	h.syncMu.Lock()
	defer h.syncMu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	h.mu.Lock()
	err := h.load()
	st := h.st.clone()
	h.mu.Unlock()
	if err != nil || st.Complete || st.Tail == "" {
		return 0, err
	}
	v := url.Values{}
	v.Set("cursor", st.Tail)
	v.Set("limit", strconv.Itoa(histPage))
	r, err := ref.scan(v)
	if err != nil {
		return 0, err
	}
	nst := st.clone()
	if len(r.Messages) > 0 {
		nst.Tail = r.Messages[len(r.Messages)-1].ID
	}
	nst.Complete = r.Next == nil || len(r.Messages) == 0
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.load(); err != nil {
		return 0, err
	}
	return len(r.Messages), h.commit(&histOp{Put: a.recsOf(r.Messages), State: &nst})
}

func (a *App) markUnsupported(ref serverRef) error {
	histMu.Lock()
	unsupported[normaliseDomain(ref.domain)] = true
	histMu.Unlock()
	return errCopyUnsupported
}

// ── Settings → Search ────────────────────────────────────────────────────────

// SearchCopyInfo is what Settings shows about the current server's copy.
type SearchCopyInfo struct {
	Server      string `json:"server"`
	Available   bool   `json:"available"` // connected, and the server can keep one up to date
	Enabled     bool   `json:"enabled"`
	Messages    int    `json:"messages"`
	Bytes       int64  `json:"bytes"`
	OldestAt    string `json:"oldest_at"`
	Complete    bool   `json:"complete"`
	Downloading bool   `json:"downloading"`
	Total       int    `json:"total"` // messages on the server you can read, while downloading (-1 unknown)
}

type copyJob struct {
	h      *localHistory
	cancel context.CancelFunc
	total  int
}

var (
	jobMu  sync.Mutex
	jobCur *copyJob
)

func (a *App) SearchCopyInfo() SearchCopyInfo {
	path, domain, _ := a.copyPath()
	info := SearchCopyInfo{Server: domain, Total: -1}
	if path == "" {
		return info
	}
	histMu.Lock()
	info.Available = !unsupported[normaliseDomain(domain)]
	histMu.Unlock()
	_, err := os.Stat(path + ".off")
	info.Enabled = err != nil
	h := a.searchCopy()
	if h == nil {
		return info
	}
	h.mu.Lock()
	m := h.meta()
	h.mu.Unlock()
	info.Messages, info.Bytes, info.Complete = m.Messages, m.Bytes, m.Complete
	if m.Oldest > 0 {
		info.OldestAt = time.UnixMilli(m.Oldest).UTC().Format(time.RFC3339)
	}
	jobMu.Lock()
	if jobCur != nil && jobCur.h == h {
		info.Downloading, info.Total = true, jobCur.total
	}
	jobMu.Unlock()
	return info
}

// DownloadSearchCopy copies all of the current server's history you can
// read into this computer's copy, in the background, with
// "searchcopy:progress" events as it goes.
func (a *App) DownloadSearchCopy() error {
	h := a.searchCopy()
	if h == nil {
		_, domain, _ := a.copyPath()
		histMu.Lock()
		no := unsupported[normaliseDomain(domain)]
		histMu.Unlock()
		if no {
			return errCopyUnsupported
		}
		return errors.New("keeping a copy is switched off for this server")
	}
	jobMu.Lock()
	if jobCur != nil {
		if jobCur.h == h {
			jobMu.Unlock()
			return nil // already going
		}
		jobCur.cancel() // another server's
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &copyJob{h: h, cancel: cancel, total: -1}
	jobCur = job
	jobMu.Unlock()
	ref := a.serverRef()
	go a.runCopyJob(ctx, ref, job)
	return nil
}

func (a *App) runCopyJob(ctx context.Context, ref serverRef, job *copyJob) {
	h := job.h
	h.mu.Lock()
	h.keep++
	h.mu.Unlock()
	progress := func(errMsg string) {
		h.mu.Lock()
		m := h.meta()
		h.mu.Unlock()
		jobMu.Lock()
		total := job.total
		jobMu.Unlock()
		ev := map[string]interface{}{"server": ref.domain, "messages": m.Messages, "total": total,
			"complete": m.Complete, "downloading": errMsg == "" && ctx.Err() == nil && !m.Complete, "error": errMsg}
		if m.Oldest > 0 {
			ev["oldest_at"] = time.UnixMilli(m.Oldest).UTC().Format(time.RFC3339)
		}
		a.emit("searchcopy:progress", ev)
	}
	defer func() {
		cancelled := ctx.Err() != nil
		job.cancel()
		jobMu.Lock()
		if jobCur == job {
			jobCur = nil
		}
		jobMu.Unlock()
		h.mu.Lock()
		h.keep--
		if h.loaded && h.frames > 50 {
			if err := h.compact(); err != nil {
				log.Printf("search copy: rewriting it: %v", err)
			}
		}
		h.mu.Unlock()
		if cancelled {
			progress("")
		}
	}()
	v := url.Values{}
	v.Set("count", "1")
	v.Set("limit", "1")
	if r, err := ref.scan(v); err == nil && r.Total != nil {
		jobMu.Lock()
		job.total = *r.Total
		jobMu.Unlock()
	}
	if err := a.copyUpdate(ctx, ref, h, nil); err != nil {
		if ctx.Err() == nil {
			progress(err.Error())
		}
		return
	}
	progress("")
	last := time.Now()
	for ctx.Err() == nil {
		n, err := a.copyBackfill(ctx, ref, h)
		if err != nil {
			if ctx.Err() == nil {
				progress(err.Error())
			}
			return
		}
		if n == 0 {
			progress("")
			return
		}
		if time.Since(last) > 250*time.Millisecond {
			progress("")
			last = time.Now()
		}
	}
}

// StopSearchCopyDownload stops copying (what's been copied stays).
func (a *App) StopSearchCopyDownload() {
	jobMu.Lock()
	if jobCur != nil {
		jobCur.cancel()
	}
	jobMu.Unlock()
}

// ClearSearchCopy deletes this computer's copy of the current server's
// history. Searching starts a new one (unless it's switched off).
func (a *App) ClearSearchCopy() error {
	path, _, _ := a.copyPath()
	if path == "" {
		return errors.New("not connected to a server")
	}
	a.StopSearchCopyDownload()
	a.ClearSearch()
	h := a.searchCopy()
	if h == nil {
		// Switched off (or not supported): there may still be one on disk.
		for _, p := range []string{path, path + ".info", path + ".new"} {
			os.Remove(p)
		}
		return nil
	}
	h.syncMu.Lock()
	defer h.syncMu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.remove()
	return nil
}

// SetSearchCopy switches keeping a copy of the current server's history on
// or off. Off deletes the copy there is.
func (a *App) SetSearchCopy(on bool) error {
	path, _, _ := a.copyPath()
	if path == "" {
		return errors.New("not connected to a server")
	}
	if on {
		err := os.Remove(path + ".off")
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		return err
	}
	if err := a.ClearSearchCopy(); err != nil {
		return err
	}
	histMu.Lock()
	if histCur != nil && histCur.path == path {
		histCur = nil
	}
	histMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path+".off", nil, 0600)
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

// matchesSearch: the same test as for messages in the copy, for one that's
// been fetched and decrypted.
func matchesSearch(m *SearchResult, q searchQuery, has []string) bool {
	if m.E2EE == "locked" || m.E2EE == "error" {
		return false // nothing readable to match
	}
	r := recFrom(m.ID, m.BoardID, m.Username, m.CreatedAt, m.Content, m.Attachments)
	return r.matches(q, has)
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

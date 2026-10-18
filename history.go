package main

// history.go — this computer's copy of a server's history, for search.
//
// Messages are end-to-end encrypted, so the server can't search them: this
// app does, and to find old messages without downloading years of history
// every time, it keeps what it has read in a local copy, one per account
// and server. The copy holds only what search needs — each message's id,
// channel, author, time, its words (lowercased and accent-free, with its
// files' names) and what kinds of files it has. A result is shown by asking
// the server for that message again, so nothing else (and no file keys) is
// kept.
//
// It is encrypted on disk with a key that comes from the account's own
// private key, so it opens only for that account, on this computer or one
// the account was moved to. It's kept as an append-only file of encrypted
// frames (AES-256-GCM, each sealed to its place in the file), rewritten in
// one piece now and then to drop what's been replaced.
//
// The copy always covers one unbroken stretch of the history: every message
// from Tail (the oldest it has) up to Head (the newest), across every
// channel you can read. It's brought up to date before each search (new
// messages, plus edits, deletions and imports from /api/search/changes), and
// grows backwards as searches go further back or when you ask for all of it
// (Settings → Search).

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/hkdf"
)

const (
	histMagic        = "CCSEARCH1\n"
	histFrameMax     = 256 << 20 // no frame is ever this big; anything claiming to be is damage
	histCompactEvery = 400       // frames before the file is rewritten in one piece
	histCompactBatch = 20000     // messages per frame when it is
	histIdle         = 10 * time.Minute
)

// Kinds of attachment a message has (histRec.K), for has:.
const (
	hasFile uint8 = 1 << iota
	hasImage
	hasVideo
	hasAudio
	hasLink
)

// histRec is one message as the copy keeps it.
type histRec struct {
	ID string `json:"i"`
	B  string `json:"b"`           // channel
	U  string `json:"u"`           // author's username, as the server has it
	T  int64  `json:"t"`           // sent, Unix milliseconds
	X  string `json:"x,omitempty"` // its words: " word word … " (see foldWords)
	K  uint8  `json:"k,omitempty"` // kind… bits
	S  uint8  `json:"s,omitempty"` // 1: couldn't be read (no key yet) — tried again later
	F  string `json:"f,omitempty"` // imported_from, for an imported person's messages
}

// histState is where the copy has got to.
type histState struct {
	Head     string `json:"head"`     // newest message in the copy
	Tail     string `json:"tail"`     // oldest
	Complete bool   `json:"complete"` // nothing older than Tail on the server
	Seq      int64  `json:"seq"`      // the last /api/search/changes number applied
	// The channels whose history you could read at the last update.
	Boards []string `json:"boards"`
	// Channels to fetch again within Head…Tail (history was imported into
	// them): channel → where that got to ("" = from the newest).
	Refill  map[string]string `json:"refill,omitempty"`
	Retried int64             `json:"retried,omitempty"` // unreadable ones last tried again (Unix)
}

// histOp is one change to the copy: one frame in its file.
type histOp struct {
	Reset  bool        `json:"reset,omitempty"`  // forget everything before this
	Drop   []string    `json:"drop,omitempty"`   // channels to forget
	Del    []string    `json:"del,omitempty"`    // messages to forget
	Author [][2]string `json:"author,omitempty"` // imported_from → username
	Put    []histRec   `json:"put,omitempty"`    // new or changed
	State  *histState  `json:"state,omitempty"`
}

// histMeta is what Settings shows without opening the whole copy, kept
// beside it (encrypted too).
type histMeta struct {
	Messages int   `json:"messages"`
	Oldest   int64 `json:"oldest"` // Unix ms, 0 if empty
	Complete bool  `json:"complete"`
	Bytes    int64 `json:"bytes"`
	Updated  int64 `json:"updated"`
}

type localHistory struct {
	mu     sync.Mutex
	syncMu sync.Mutex // one update or download step at a time
	path   string
	gcm    cipher.AEAD

	st     histState
	recs   []histRec // newest first
	boards map[string]bool
	loaded bool
	intern map[string]string

	f      *os.File
	fileID []byte
	frames uint64
	size   int64

	used      time.Time
	idleTimer *time.Timer
	keep      int // >0 while something (a download) needs it open
}

// historyDir is where an account's copies live.
func historyDir(slug string) string { return filepath.Join(accountsDir(), slug, "search") }

func historyName(domain string) string {
	h := sha256.Sum256([]byte("chriscord-search-copy\x00" + normaliseDomain(domain)))
	return hex.EncodeToString(h[:12])
}

func normaliseDomain(d string) string {
	d = strings.ToLower(strings.TrimRight(strings.TrimSpace(d), "/"))
	d = strings.TrimPrefix(d, "https://")
	return strings.TrimPrefix(d, "http://")
}

// historyKey: the copy's key, from the account's private key and the server.
func historyKey(seed []byte, domain string) ([]byte, error) {
	key := make([]byte, 32)
	r := hkdf.New(sha256.New, seed, []byte("chriscord-search-copy-v1"), []byte(normaliseDomain(domain)))
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, err
	}
	return key, nil
}

func newLocalHistory(path string, key []byte) (*localHistory, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &localHistory{path: path, gcm: gcm, intern: map[string]string{}}, nil
}

// ── Loading and saving ───────────────────────────────────────────────────────

// load reads the copy into memory (h.mu held). A damaged end is cut off
// (that part is fetched again); a copy that can't be opened at all — not
// this account's, or broken from the start — is started over.
func (h *localHistory) load() error {
	if h.loaded {
		h.touch()
		return nil
	}
	h.reset()
	data, err := os.ReadFile(h.path)
	if errors.Is(err, os.ErrNotExist) {
		h.loaded = true
		h.touch()
		return nil
	}
	if err != nil {
		return err
	}
	good, frames := 0, uint64(0)
	if len(data) >= len(histMagic)+16 && string(data[:len(histMagic)]) == histMagic {
		h.fileID = append([]byte(nil), data[len(histMagic):len(histMagic)+16]...)
		good = len(histMagic) + 16
		for good+4 <= len(data) {
			n := int(binary.BigEndian.Uint32(data[good:]))
			if n < h.gcm.NonceSize() || n > histFrameMax || good+4+n > len(data) {
				break
			}
			op, err := h.openFrame(data[good+4:good+4+n], frames)
			if err != nil {
				break
			}
			h.apply(op)
			frames++
			good += 4 + n
		}
	}
	if frames == 0 {
		// Nothing readable: start a new copy.
		if len(data) > 0 {
			log.Printf("search copy: %s can't be read; starting it over", filepath.Base(h.path))
		}
		h.reset() // (the file is replaced when there's something to keep)
		h.loaded = true
		h.touch()
		return nil
	}
	if good < len(data) {
		log.Printf("search copy: the end of %s was damaged; cutting it off", filepath.Base(h.path))
		if err := os.Truncate(h.path, int64(good)); err != nil {
			return err
		}
	}
	h.frames, h.size = frames, int64(good)
	h.loaded = true
	h.touch()
	if h.frames > histCompactEvery {
		if err := h.compact(); err != nil {
			log.Printf("search copy: rewriting it: %v", err)
		}
	}
	return nil
}

// reset empties the copy in memory.
func (h *localHistory) reset() {
	if h.f != nil {
		h.f.Close()
		h.f = nil
	}
	h.st = histState{}
	h.recs = nil
	h.boards = map[string]bool{}
	h.intern = map[string]string{}
	h.fileID = nil
	h.frames, h.size = 0, 0
}

func (h *localHistory) frameAAD(n uint64) []byte {
	aad := make([]byte, 0, len(h.fileID)+8)
	aad = append(aad, h.fileID...)
	return binary.BigEndian.AppendUint64(aad, n)
}

func (h *localHistory) openFrame(b []byte, n uint64) (*histOp, error) {
	ns := h.gcm.NonceSize()
	plain, err := h.gcm.Open(nil, b[:ns], b[ns:], h.frameAAD(n))
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(plain))
	if err != nil {
		return nil, err
	}
	var op histOp
	if err := json.NewDecoder(zr).Decode(&op); err != nil {
		return nil, err
	}
	return &op, nil
}

func (h *localHistory) sealFrame(op *histOp, n uint64) ([]byte, error) {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err := json.NewEncoder(zw).Encode(op); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	nonce := make([]byte, h.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	sealed := h.gcm.Seal(nonce, nonce, buf.Bytes(), h.frameAAD(n))
	out := binary.BigEndian.AppendUint32(make([]byte, 0, 4+len(sealed)), uint32(len(sealed)))
	return append(out, sealed...), nil
}

// commit applies op and adds it to the file (h.mu held).
func (h *localHistory) commit(op *histOp) error {
	if op.Reset {
		h.reset()
		_ = os.Remove(h.path)
	}
	h.apply(op)
	if h.f == nil {
		if err := h.openForAppend(); err != nil {
			return err
		}
	}
	frame, err := h.sealFrame(op, h.frames)
	if err != nil {
		return err
	}
	if _, err := h.f.Write(frame); err != nil {
		return err
	}
	h.frames++
	h.size += int64(len(frame))
	if h.frames > histCompactEvery*2 {
		return h.compact()
	}
	h.writeMeta()
	return nil
}

func (h *localHistory) openForAppend() error {
	if err := os.MkdirAll(filepath.Dir(h.path), 0700); err != nil {
		return err
	}
	if h.fileID == nil {
		// A new file.
		h.fileID = make([]byte, 16)
		if _, err := rand.Read(h.fileID); err != nil {
			return err
		}
		f, err := os.OpenFile(h.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		if _, err := f.Write(append([]byte(histMagic), h.fileID...)); err != nil {
			f.Close()
			return err
		}
		h.f, h.frames, h.size = f, 0, int64(len(histMagic)+16)
		return nil
	}
	f, err := os.OpenFile(h.path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	h.f = f
	return nil
}

// compact rewrites the file with just what's in the copy now (h.mu held).
func (h *localHistory) compact() error {
	if h.f != nil {
		h.f.Close()
		h.f = nil
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	tmp := h.path + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	oldID := h.fileID
	h.fileID = id
	size := int64(len(histMagic) + 16)
	_, err = f.Write(append([]byte(histMagic), id...))
	var n uint64
	for i := 0; err == nil && (i < len(h.recs) || n == 0); i += histCompactBatch {
		op := &histOp{Reset: n == 0, Put: h.recs[i:min(i+histCompactBatch, len(h.recs))]}
		if i+histCompactBatch >= len(h.recs) {
			st := h.st
			op.State = &st
		}
		var frame []byte
		if frame, err = h.sealFrame(op, n); err == nil {
			_, err = f.Write(frame)
			size += int64(len(frame))
			n++
		}
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, h.path)
	}
	if err != nil {
		os.Remove(tmp)
		h.fileID = oldID
		return err
	}
	h.frames, h.size = n, size
	h.writeMeta()
	return nil
}

func (h *localHistory) metaPath() string { return h.path + ".info" }

func (h *localHistory) writeMeta() {
	m := histMeta{Messages: len(h.recs), Complete: h.st.Complete, Bytes: h.size, Updated: time.Now().Unix()}
	if len(h.recs) > 0 {
		m.Oldest = h.recs[len(h.recs)-1].T
	}
	plain, _ := json.Marshal(m)
	nonce := make([]byte, h.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return
	}
	sealed := h.gcm.Seal(nonce, nonce, plain, []byte("info"))
	tmp := h.metaPath() + ".new"
	if os.WriteFile(tmp, sealed, 0600) == nil {
		_ = os.Rename(tmp, h.metaPath())
	}
}

// meta: what the copy holds, without loading it if it isn't.
func (h *localHistory) meta() histMeta {
	if h.loaded {
		m := histMeta{Messages: len(h.recs), Complete: h.st.Complete, Bytes: h.size}
		if len(h.recs) > 0 {
			m.Oldest = h.recs[len(h.recs)-1].T
		}
		return m
	}
	var m histMeta
	b, err := os.ReadFile(h.metaPath())
	if err != nil || len(b) < h.gcm.NonceSize() {
		return m
	}
	ns := h.gcm.NonceSize()
	if plain, err := h.gcm.Open(nil, b[:ns], b[ns:], []byte("info")); err == nil {
		_ = json.Unmarshal(plain, &m)
	}
	if fi, err := os.Stat(h.path); err == nil {
		m.Bytes = fi.Size()
	} else {
		m = histMeta{}
	}
	return m
}

// remove deletes the copy (h.mu held).
func (h *localHistory) remove() {
	h.reset()
	os.Remove(h.path)
	os.Remove(h.metaPath())
	os.Remove(h.path + ".new")
	h.loaded = true
}

// touch notes the copy was used; it's let go of from memory after histIdle
// unused (it stays on disk).
func (h *localHistory) touch() {
	h.used = time.Now()
	if h.idleTimer == nil {
		h.idleTimer = time.AfterFunc(histIdle, h.unloadIfIdle)
	} else {
		h.idleTimer.Reset(histIdle)
	}
}

func (h *localHistory) unloadIfIdle() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.loaded || h.keep > 0 || time.Since(h.used) < histIdle {
		if h.loaded {
			h.idleTimer.Reset(histIdle)
		}
		return
	}
	h.unload()
}

// unload lets go of the copy in memory (h.mu held).
func (h *localHistory) unload() {
	if h.f != nil {
		h.f.Close()
		h.f = nil
	}
	h.recs, h.boards, h.intern = nil, nil, nil
	h.loaded = false
}

// ── Changing it ──────────────────────────────────────────────────────────────

func (h *localHistory) in(s string) string {
	if v, ok := h.intern[s]; ok {
		return v
	}
	h.intern[s] = s
	return s
}

// find: where id is, or would go, in recs (newest first).
func (h *localHistory) find(id string) (int, bool) {
	i := sort.Search(len(h.recs), func(i int) bool { return h.recs[i].ID <= id })
	return i, i < len(h.recs) && h.recs[i].ID == id
}

// after: the index of the first message older than id ("" = the newest).
func (h *localHistory) after(id string) int {
	if id == "" {
		return 0
	}
	return sort.Search(len(h.recs), func(i int) bool { return h.recs[i].ID < id })
}

func (h *localHistory) apply(op *histOp) {
	if op.Reset {
		h.st = histState{}
		h.recs = h.recs[:0]
	}
	if len(op.Drop) > 0 || len(op.Del) > 0 {
		drop := make(map[string]bool, len(op.Drop))
		for _, b := range op.Drop {
			drop[b] = true
		}
		del := make(map[string]bool, len(op.Del))
		for _, id := range op.Del {
			del[id] = true
		}
		kept := h.recs[:0]
		for _, r := range h.recs {
			if !drop[r.B] && !del[r.ID] {
				kept = append(kept, r)
			}
		}
		clear(h.recs[len(kept):])
		h.recs = kept
	}
	for _, a := range op.Author {
		name := h.in(a[1])
		for i := range h.recs {
			if h.recs[i].F == a[0] {
				h.recs[i].U = name
			}
		}
	}
	if len(op.Put) > 0 {
		h.put(op.Put)
	}
	if op.State != nil {
		h.st = *op.State
		h.boards = make(map[string]bool, len(h.st.Boards))
		for _, b := range h.st.Boards {
			h.boards[b] = true
		}
	}
}

// put adds or replaces messages, keeping recs newest first.
func (h *localHistory) put(in []histRec) {
	batch := make([]histRec, 0, len(in))
	for _, r := range in {
		if r.ID == "" {
			continue
		}
		r.B, r.U, r.F = h.in(r.B), h.in(r.U), h.in(r.F)
		batch = append(batch, r)
	}
	sort.SliceStable(batch, func(i, j int) bool { return batch[i].ID > batch[j].ID })
	// The last of the same id wins.
	uniq := batch[:0]
	for i, r := range batch {
		if i+1 < len(batch) && batch[i+1].ID == r.ID {
			continue
		}
		uniq = append(uniq, r)
	}
	batch = uniq
	if len(batch) == 0 {
		return
	}
	switch {
	case len(h.recs) == 0 || batch[0].ID < h.recs[len(h.recs)-1].ID:
		h.recs = append(h.recs, batch...) // all older: the usual way it grows
		return
	case batch[len(batch)-1].ID > h.recs[0].ID:
		h.recs = append(batch, h.recs...) // all newer
		return
	}
	// Mixed in: replace what's there, merge in the rest.
	var fresh []histRec
	for _, r := range batch {
		if i, ok := h.find(r.ID); ok {
			h.recs[i] = r
		} else {
			fresh = append(fresh, r)
		}
	}
	if len(fresh) == 0 {
		return
	}
	merged := make([]histRec, 0, len(h.recs)+len(fresh))
	i, j := 0, 0
	for i < len(h.recs) || j < len(fresh) {
		if j >= len(fresh) || (i < len(h.recs) && h.recs[i].ID > fresh[j].ID) {
			merged = append(merged, h.recs[i])
			i++
		} else {
			merged = append(merged, fresh[j])
			j++
		}
	}
	h.recs = merged
}

// ── Messages as the copy keeps them ──────────────────────────────────────────

// recOf makes a message the copy can keep from one the server sent,
// decrypting it (without keeping its file keys — see histRec).
func (t *textCrypto) recOf(m *SearchResult) histRec {
	content, state := m.Content, m.E2EE
	var files []fileMeta
	if state == "" {
		content, state, files, _ = t.openFull(m.BoardID, m.Username, m.ImportedFrom, m.Content)
	}
	atts := make([]Attachment, 0, len(m.Attachments)+len(files))
	for _, a := range m.Attachments {
		if !isBlob(a) {
			atts = append(atts, a)
		}
	}
	if state == "ok" || state == "forged" {
		for _, f := range files {
			atts = append(atts, Attachment{Name: f.Name, Mime: f.Mime})
		}
	}
	r := recFrom(m.ID, m.BoardID, m.Username, m.CreatedAt, content, atts)
	r.F = m.ImportedFrom
	if state == "locked" || state == "error" {
		r.S, r.X, r.K = 1, "", 0
	}
	return r
}

// recFrom: the searchable parts of a message that's already readable.
func recFrom(id, board, user, created, content string, atts []Attachment) histRec {
	r := histRec{ID: id, B: board, U: user, T: parseMillis(created)}
	parts := []string{content}
	for _, a := range atts {
		parts = append(parts, a.Name)
		r.K |= hasFile
		switch {
		case strings.HasPrefix(a.Mime, "image/"):
			r.K |= hasImage
		case strings.HasPrefix(a.Mime, "video/"):
			r.K |= hasVideo
		case strings.HasPrefix(a.Mime, "audio/"):
			r.K |= hasAudio
		}
	}
	if c := strings.ToLower(content); strings.Contains(c, "http://") || strings.Contains(c, "https://") {
		r.K |= hasLink
	}
	r.X = foldWords(strings.Join(parts, "\n"))
	return r
}

// foldWords: text as " word word … ", lowercase without accents (see
// tokens), so a word is found as " word " and a phrase as " two words ".
func foldWords(s string) string {
	toks := tokens(s)
	if len(toks) == 0 {
		return ""
	}
	return " " + strings.Join(toks, " ") + " "
}

func parseMillis(s string) int64 {
	if s == "" {
		return 0
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

// matches: whether the message has every word and phrase, and every kind
// of attachment asked for.
func (r *histRec) matches(q searchQuery, has []string) bool {
	if r.S != 0 {
		return false // nothing readable to match
	}
	for _, k := range has {
		bit := kindBit(k)
		if bit == 0 || r.K&bit == 0 {
			return false
		}
	}
	for _, w := range q.words {
		if !strings.Contains(r.X, " "+w+" ") {
			return false
		}
	}
	for _, p := range q.phrases {
		if !strings.Contains(r.X, " "+strings.Join(p, " ")+" ") {
			return false
		}
	}
	return true
}

func kindBit(kind string) uint8 {
	switch kind {
	case "link":
		return hasLink
	case "file", "attachment":
		return hasFile
	case "image":
		return hasImage
	case "video":
		return hasVideo
	case "audio", "sound":
		return hasAudio
	}
	log.Printf("search: unknown has:%s", kind)
	return 0
}

// String is for logs and tests.
func (r histRec) String() string { return fmt.Sprintf("%s %s/%s %q", r.ID, r.B, r.U, r.X) }

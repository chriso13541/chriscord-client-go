package main

// textcrypt.go — end-to-end encrypted text messages.
//
// Each channel has a key, in numbered generations. Every message is
// encrypted with the newest generation before it leaves this app; the
// server stores and forwards only ciphertext. The keys themselves never
// reach the server in the clear — it stores one sealed copy ("box") of each
// generation per member, which only that member can open.
//
//   - Your encryption key (X25519) is derived from your account's identity
//     seed, so it's the same on every device you sign in on and survives
//     exporting/importing your account. Its public half is published to the
//     server signed by your identity key, and everyone checks that signature
//     before sealing anything for it.
//   - Joining a channel (or the server): whoever's online and already has
//     the channel's keys seals every generation for you (runPending), so
//     you can read the whole history.
//   - Losing access (kicked, banned, a role or channel change): the next
//     message sent in that channel first makes a new generation, sealed only
//     for the people who still have access. What you could read before, you
//     keep; nothing after.
//   - A message whose key hasn't reached you yet shows as locked, and opens
//     by itself once someone shares the key.
//
// Who counts as a member of a channel comes from the server (its roles and
// channel access). A dishonest server could add someone to that list — the
// same limit as voice. Signed membership (with the central account system)
// is what closes that; everything here keys on identity keys, not names,
// so that slots in without changing the stored data.
//
// Not covered yet: attachments (files are still uploaded as they are),
// link previews (the server fetches them, so it sees the link), and search
// (the server can't search text it can't read).

import (
	"bytes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

const (
	textPrefix      = "e2ee:1:" // content = textPrefix + generation + ":" + base64(nonce|ciphertext)
	textRefetchGap  = 5 * time.Second
	textPendingGap  = 1500 * time.Millisecond
	textPendingTick = 60 * time.Second

	textLockedMsg   = "\U0001F512 Encrypted message — waiting for someone in this channel to share its key with you"
	textBrokenMsg   = "\U0001F512 This message couldn’t be decrypted"
	textNoKeyNotice = "couldn't encrypt the message: "
)

// textMember is someone the server says is on the server (or can see a
// channel), with their published encryption key.
type textMember struct {
	Username  string  `json:"username"`
	PublicKey string  `json:"public_key"` // identity key (Ed25519), hex
	EncKey    *string `json:"enc_key"`    // X25519, hex; nil if not published yet
	Sig       *string `json:"sig"`        // identity key's signature over it, hex
}

type boardKeys struct {
	keys      map[int][]byte // generation → key
	latest    int            // the newest generation (0: none yet)
	rotate    bool           // someone who had the newest one has lost access
	members   []textMember
	fetchedAt time.Time
}

type textIdentity struct {
	idHex   string
	idPriv  ed25519.PrivateKey
	encPriv [32]byte
	encPub  [32]byte
	me      string
	domain  string
}

type textCrypto struct {
	app *App

	mu        sync.Mutex
	scope     string                 // domain|identity the cached keys belong to
	boards    map[string]*boardKeys  // board → its keys
	msgBoard  map[string]string      // message id → board (for edits)
	msgUser   map[string]string      // message id → author (for checking edits)
	locked    map[string]bool        // boards with messages that didn't open yet
	triedAt   map[string]time.Time   // board → when its keys were last fetched for opening
	curBoard  string                 // the board the page is showing
	boardLock map[string]*sync.Mutex // one key change per board at a time
	published string                 // scope whose key has been published
	files     map[string]*fileEntry  // stored file name → its key and details (filecrypt.go)
	msgFiles  map[string][]fileMeta  // message id → its encrypted attachments
	pendingT  *time.Timer
	tickStop  chan struct{}

	// The server, over HTTP (swapped out in tests).
	call func(method, path string, body, out interface{}) error
}

func newTextCrypto(a *App) *textCrypto {
	t := &textCrypto{app: a}
	t.call = a.doJSON
	t.resetLocked("")
	return t
}

func (t *textCrypto) resetLocked(scope string) {
	t.scope = scope
	t.boards = map[string]*boardKeys{}
	t.msgBoard = map[string]string{}
	t.msgUser = map[string]string{}
	t.locked = map[string]bool{}
	t.triedAt = map[string]time.Time{}
	t.boardLock = map[string]*sync.Mutex{}
	t.files = map[string]*fileEntry{}
	t.msgFiles = map[string][]fileMeta{}
	t.curBoard = ""
}

// doJSON makes one request to the server with the session token.
func (a *App) doJSON(method, path string, body, out interface{}) error {
	a.mu.Lock()
	domain, token := a.domain, a.token
	a.mu.Unlock()
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, normaliseHTTP(domain)+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Session-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("network error: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e map[string]string
		json.NewDecoder(resp.Body).Decode(&e)
		if msg, ok := e["error"]; ok {
			return &httpError{resp.StatusCode, msg}
		}
		return &httpError{resp.StatusCode, fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func isConflict(err error) bool {
	var h *httpError
	return errors.As(err, &h) && h.code == http.StatusConflict
}

// ── Keys ──────────────────────────────────────────────────────────────────────

// encKeyFromSeed derives the account's encryption key from its identity
// seed: the same account gets the same key everywhere.
func encKeyFromSeed(seed []byte) (priv, pub [32]byte) {
	io.ReadFull(hkdf.New(sha256.New, seed, nil, []byte("chriscord-e2ee-v1/x25519")), priv[:])
	p, _ := curve25519.X25519(priv[:], curve25519.Basepoint)
	copy(pub[:], p)
	return
}

// encKeyMessage is what the identity key signs to vouch for an encryption key.
func encKeyMessage(encPub []byte) []byte {
	return append([]byte("chriscord-e2ee-v1/enc-key\x00"), encPub...)
}

// identity is this account's keys, or ok=false when signed out.
func (t *textCrypto) identity() (id textIdentity, ok bool) {
	a := t.app
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.account == nil || len(a.account.PrivateKey) != ed25519.PrivateKeySize {
		return id, false
	}
	id.idPriv = a.account.PrivateKey
	id.idHex = hex.EncodeToString(a.account.PublicKey)
	id.encPriv, id.encPub = encKeyFromSeed(a.account.PrivateKey.Seed())
	id.me, id.domain = a.username, a.domain
	return id, true
}

// memberEncKey checks a member's published encryption key against their
// identity key, and returns it.
func (t *textCrypto) memberEncKey(m textMember, domain string) ([32]byte, bool) {
	var enc [32]byte
	if m.EncKey == nil || m.Sig == nil {
		return enc, false
	}
	id, err1 := hex.DecodeString(m.PublicKey)
	e, err2 := hex.DecodeString(*m.EncKey)
	sig, err3 := hex.DecodeString(*m.Sig)
	if err1 != nil || err2 != nil || err3 != nil || len(id) != ed25519.PublicKeySize || len(e) != 32 ||
		!ed25519.Verify(id, encKeyMessage(e), sig) {
		return enc, false
	}
	if m.Username != "" && !pinIdentity(domain, m.Username, strings.ToLower(m.PublicKey)) {
		log.Printf("e2ee: WARNING: %s's identity key is different from the one seen before", m.Username)
	}
	copy(enc[:], e)
	return enc, true
}

// boxCipher is the one-off cipher that carries a channel key from one
// member (sender) to another (recipient): X25519 between their encryption
// keys, through HKDF bound to the channel, the generation and both people.
func boxCipher(myPriv, peerPub [32]byte, board string, gen int, senderID, recipientID string, senderEnc, recipientEnc [32]byte) (cipher.AEAD, error) {
	shared, err := curve25519.X25519(myPriv[:], peerPub[:])
	if err != nil {
		return nil, err
	}
	var info bytes.Buffer
	fmt.Fprintf(&info, "chriscord-e2ee-v1/channel-key\x00%s\x00%d\x00%s\x00%s\x00", board, gen,
		strings.ToLower(senderID), strings.ToLower(recipientID))
	info.Write(senderEnc[:])
	info.Write(recipientEnc[:])
	k := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(hkdf.New(sha256.New, shared, nil, info.Bytes()), k); err != nil {
		return nil, err
	}
	return chacha20poly1305.New(k)
}

func sealBox(id textIdentity, recipientID string, recipientEnc [32]byte, board string, gen int, key []byte) (string, error) {
	c, err := boxCipher(id.encPriv, recipientEnc, board, gen, id.idHex, recipientID, id.encPub, recipientEnc)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, c.NonceSize(), c.NonceSize()+len(key)+c.Overhead())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(c.Seal(nonce, nonce, key, nil)), nil
}

func openBox(id textIdentity, senderID string, senderEnc [32]byte, board string, gen int, box string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(box)
	if err != nil {
		return nil, err
	}
	c, err := boxCipher(id.encPriv, senderEnc, board, gen, senderID, id.idHex, senderEnc, id.encPub)
	if err != nil {
		return nil, err
	}
	if len(raw) < c.NonceSize() {
		return nil, errors.New("short box")
	}
	key, err := c.Open(nil, raw[:c.NonceSize()], raw[c.NonceSize():], nil)
	if err == nil && len(key) != chacha20poly1305.KeySize {
		err = errors.New("wrong key size")
	}
	return key, err
}

// ── Messages ──────────────────────────────────────────────────────────────────

// textPlain is what's inside an encrypted message: who wrote it (so the
// server can't pass one person's message off as another's), the text, and
// its encrypted attachments — where each is, what it really is, and its key
// (filecrypt.go).
type textPlain struct {
	U string     `json:"u"`
	T string     `json:"t"`
	A []fileMeta `json:"a,omitempty"`
}

func textAAD(board string, gen int) []byte {
	return []byte(fmt.Sprintf("chriscord-e2ee-v1/msg\x00%s\x00%d", board, gen))
}

func sealText(key []byte, board string, gen int, author, text string) (string, error) {
	return sealPlain(key, board, gen, textPlain{U: author, T: text})
}

func sealPlain(key []byte, board string, gen int, p textPlain) (string, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return "", err
	}
	pt, _ := json.Marshal(p)
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(pt)+aead.Overhead())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := aead.Seal(nonce, nonce, pt, textAAD(board, gen))
	return textPrefix + strconv.Itoa(gen) + ":" + base64.StdEncoding.EncodeToString(ct), nil
}

// parseText splits an encrypted message's content; ok is false for a
// plain (unencrypted) one.
func parseText(content string) (gen int, blob []byte, ok bool) {
	rest, found := strings.CutPrefix(content, textPrefix)
	if !found {
		return 0, nil, false
	}
	g, b64, found := strings.Cut(rest, ":")
	if !found {
		return 0, nil, false
	}
	gen, err := strconv.Atoi(g)
	if err != nil || gen < 1 {
		return 0, nil, false
	}
	blob, err = base64.StdEncoding.DecodeString(b64)
	return gen, blob, err == nil
}

func openText(key []byte, board string, gen int, blob []byte) (textPlain, error) {
	var p textPlain
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return p, err
	}
	if len(blob) < aead.NonceSize() {
		return p, errors.New("short message")
	}
	pt, err := aead.Open(nil, blob[:aead.NonceSize()], blob[aead.NonceSize():], textAAD(board, gen))
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal(pt, &p)
}

// ── Talking to the server ─────────────────────────────────────────────────────

type keysResp struct {
	Latest int  `json:"latest"`
	Rotate bool `json:"rotate"`
	Boxes  []struct {
		Generation int        `json:"generation"`
		Box        string     `json:"box"`
		Sender     textMember `json:"sender"`
	} `json:"boxes"`
	Members []textMember `json:"members"`
}

// begin starts over for a newly connected server (keys are per server and
// account), publishes this account's encryption key, and looks for anyone
// waiting on keys this app can share.
func (t *textCrypto) begin() {
	id, ok := t.identity()
	if !ok {
		return
	}
	scope := id.domain + "|" + id.idHex
	t.mu.Lock()
	if t.scope != scope {
		t.resetLocked(scope)
	}
	if t.tickStop != nil {
		close(t.tickStop)
	}
	stop := make(chan struct{})
	t.tickStop = stop
	t.mu.Unlock()
	go func() {
		t.ensurePublished()
		t.runPending()
		tick := time.NewTicker(textPendingTick)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				t.runPending()
			}
		}
	}()
}

// end stops the background key sharing (leaving the server).
func (t *textCrypto) end() {
	t.mu.Lock()
	if t.tickStop != nil {
		close(t.tickStop)
		t.tickStop = nil
	}
	if t.pendingT != nil {
		t.pendingT.Stop()
	}
	t.mu.Unlock()
}

// ensurePublished puts this account's encryption key on the server (once
// per connection's scope; the server ignores a key it already has).
func (t *textCrypto) ensurePublished() {
	id, ok := t.identity()
	if !ok {
		return
	}
	scope := id.domain + "|" + id.idHex
	t.mu.Lock()
	done := t.published == scope
	t.mu.Unlock()
	if done {
		return
	}
	sig := ed25519.Sign(id.idPriv, encKeyMessage(id.encPub[:]))
	err := t.call("PUT", "/api/e2ee/enc-key", map[string]string{
		"enc_key": hex.EncodeToString(id.encPub[:]), "sig": hex.EncodeToString(sig),
	}, nil)
	if err != nil {
		log.Printf("e2ee: publishing this account's encryption key: %v", err)
		return
	}
	t.mu.Lock()
	t.published = scope
	t.mu.Unlock()
}

// fetch gets a board's keys from the server — opening every generation
// sealed for this account — plus who can see it and whether it needs a new
// generation.
func (t *textCrypto) fetch(board string) (*boardKeys, error) {
	id, ok := t.identity()
	if !ok {
		return nil, errors.New("not signed in")
	}
	var r keysResp
	if err := t.call("GET", "/api/boards/"+url.PathEscape(board)+"/keys", nil, &r); err != nil {
		return nil, err
	}
	bk := &boardKeys{keys: map[int][]byte{}, latest: r.Latest, rotate: r.Rotate, members: r.Members, fetchedAt: time.Now()}
	for _, b := range r.Boxes {
		senderEnc, ok := t.memberEncKey(b.Sender, id.domain)
		if !ok {
			log.Printf("e2ee: a key for %s from %s isn't signed properly — ignored", board, b.Sender.PublicKey)
			continue
		}
		key, err := openBox(id, b.Sender.PublicKey, senderEnc, board, b.Generation, b.Box)
		if err != nil {
			log.Printf("e2ee: a key for %s (generation %d) didn't open: %v", board, b.Generation, err)
			continue
		}
		bk.keys[b.Generation] = key
	}
	t.mu.Lock()
	if old := t.boards[board]; old != nil {
		for g, k := range old.keys { // keep what's already open (e.g. made here)
			if bk.keys[g] == nil {
				bk.keys[g] = k
			}
		}
	}
	t.boards[board] = bk
	t.mu.Unlock()
	return bk, nil
}

func (t *textCrypto) key(board string, gen int) []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	if bk := t.boards[board]; bk != nil {
		return bk.keys[gen]
	}
	return nil
}

func (t *textCrypto) lockFor(board string) *sync.Mutex {
	t.mu.Lock()
	defer t.mu.Unlock()
	l := t.boardLock[board]
	if l == nil {
		l = &sync.Mutex{}
		t.boardLock[board] = l
	}
	return l
}

// newGeneration makes the board's next key and seals it for everyone who
// can see the board (this account included).
func (t *textCrypto) newGeneration(board string, bk *boardKeys) error {
	id, ok := t.identity()
	if !ok {
		return errors.New("not signed in")
	}
	gen := bk.latest + 1
	key := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return err
	}
	boxes := []map[string]string{}
	haveSelf := false
	for _, m := range bk.members {
		enc, ok := t.memberEncKey(m, id.domain)
		if !ok {
			continue // not on an encryption-capable app yet: they get it once they are (runPending)
		}
		box, err := sealBox(id, m.PublicKey, enc, board, gen, key)
		if err != nil {
			continue
		}
		haveSelf = haveSelf || strings.EqualFold(m.PublicKey, id.idHex)
		boxes = append(boxes, map[string]string{"recipient": strings.ToLower(m.PublicKey), "box": box})
	}
	if !haveSelf {
		box, err := sealBox(id, id.idHex, id.encPub, board, gen, key)
		if err != nil {
			return err
		}
		boxes = append(boxes, map[string]string{"recipient": id.idHex, "box": box})
	}
	if err := t.call("POST", "/api/boards/"+url.PathEscape(board)+"/keys", map[string]interface{}{
		"generation": gen, "boxes": boxes,
	}, nil); err != nil {
		return err
	}
	t.mu.Lock()
	if cur := t.boards[board]; cur != nil {
		cur.keys[gen] = key
		cur.latest, cur.rotate = gen, false
	}
	t.mu.Unlock()
	log.Printf("e2ee: new key (generation %d) for %s, for %d member(s)", gen, board, len(boxes))
	return nil
}

// seal encrypts a message's text for sending in board. Empty text stays
// empty (a files-only message).
func (t *textCrypto) seal(board, text string) (string, error) {
	return t.sealWith(board, text, nil)
}

// sealWith encrypts a message's text and its encrypted attachments'
// details. Nothing at all stays empty.
func (t *textCrypto) sealWith(board, text string, files []fileMeta) (string, error) {
	if text == "" && len(files) == 0 {
		return "", nil
	}
	id, ok := t.identity()
	if !ok {
		return "", errors.New("not signed in")
	}
	t.ensurePublished()
	l := t.lockFor(board)
	l.Lock()
	defer l.Unlock()
	for attempt := 0; ; attempt++ {
		// Fresh every time: whether someone just lost access matters here.
		bk, err := t.fetch(board)
		if err != nil {
			return "", fmt.Errorf("%s%w", textNoKeyNotice, err)
		}
		if bk.latest == 0 || bk.rotate || t.key(board, bk.latest) == nil {
			// No key yet, someone lost access, or the newest key hasn't been
			// shared with this account: start a new generation.
			err := t.newGeneration(board, bk)
			if isConflict(err) && attempt < 2 {
				continue // someone else just made it: use theirs
			}
			if err != nil {
				return "", fmt.Errorf("%s%w", textNoKeyNotice, err)
			}
		}
		t.mu.Lock()
		gen := t.boards[board].latest
		key := t.boards[board].keys[gen]
		t.mu.Unlock()
		return sealPlain(key, board, gen, textPlain{U: id.me, T: text, A: files})
	}
}

// openContent decrypts one message's content for showing. state is "" for
// a plain (unencrypted) message, "ok", "locked" (no key yet), "error" or
// "forged" (it says it's from someone other than who the server says).
func (t *textCrypto) openContent(board, author, content string) (string, string) {
	text, state, _ := t.openFull(board, author, content)
	return text, state
}

// openFull is openContent plus the message's encrypted attachments.
func (t *textCrypto) openFull(board, author, content string) (string, string, []fileMeta) {
	gen, blob, ok := parseText(content)
	if !ok {
		return content, "", nil
	}
	key := t.key(board, gen)
	if key == nil {
		t.mu.Lock()
		// Ask the server again — at most every few seconds, unless it's a
		// generation newer than any seen yet (someone just started one).
		newer := t.boards[board] == nil || gen > t.boards[board].latest
		try := newer || time.Since(t.triedAt[board]) > textRefetchGap
		if try {
			t.triedAt[board] = time.Now()
		}
		t.mu.Unlock()
		if try {
			if _, err := t.fetch(board); err != nil {
				log.Printf("e2ee: fetching keys for %s: %v", board, err)
			}
			key = t.key(board, gen)
		}
	}
	if key == nil {
		t.mu.Lock()
		t.locked[board] = true
		t.mu.Unlock()
		return textLockedMsg, "locked", nil
	}
	p, err := openText(key, board, gen, blob)
	if err != nil {
		return textBrokenMsg, "error", nil
	}
	if author != "" && p.U != author {
		return fmt.Sprintf("⚠ Written by %s, not %s: %s", p.U, author, p.T), "forged", p.A
	}
	return p.T, "ok", p.A
}

// open decrypts a message (and the preview of the one it replies to) in place.
func (t *textCrypto) open(m *ChatMessage) {
	if m == nil {
		return
	}
	t.mu.Lock()
	if m.ID != "" {
		t.msgBoard[m.ID] = m.BoardID
		t.msgUser[m.ID] = m.Username
	}
	t.mu.Unlock()
	var files []fileMeta
	m.Content, m.E2EE, files = t.openFull(m.BoardID, m.Username, m.Content)
	m.Attachments = t.showAttachments(m.ID, m.Attachments, m.E2EE, files)
	if m.Reply != nil {
		m.Reply.Content, _ = t.openContent(m.BoardID, m.Reply.Username, m.Reply.Content)
	}
}

func (t *textCrypto) openAll(ms []ChatMessage) {
	for i := range ms {
		t.open(&ms[i])
	}
}

// openEdit decrypts an edited message's new content.
func (t *textCrypto) openEdit(id, board, content string) (string, string) {
	t.mu.Lock()
	author := t.msgUser[id]
	t.mu.Unlock()
	text, state, files := t.openFull(board, author, content)
	if state == "ok" || state == "forged" {
		t.showAttachments(id, nil, state, files) // keep what an edit carries over
	}
	return text, state
}

// boardOf is the board a message is in, as far as this app knows.
func (t *textCrypto) boardOf(msgID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if b, ok := t.msgBoard[msgID]; ok {
		return b
	}
	return t.curBoard
}

func (t *textCrypto) setCurrent(board string) {
	t.mu.Lock()
	t.curBoard = board
	t.mu.Unlock()
}

// changed: keys moved on the server (someone published a key, a channel got
// a new generation, someone shared keys), or who can see what did.
func (t *textCrypto) changed() {
	t.mu.Lock()
	if t.pendingT != nil {
		t.pendingT.Stop()
	}
	t.pendingT = time.AfterFunc(textPendingGap, func() {
		t.runPending()
		t.retryLocked()
	})
	t.mu.Unlock()
}

// retryLocked tries again for boards with messages that didn't open; if the
// one on screen now can, the page gets its history again.
func (t *textCrypto) retryLocked() {
	t.mu.Lock()
	var boards []string
	for b := range t.locked {
		boards = append(boards, b)
	}
	cur := t.curBoard
	t.mu.Unlock()
	for _, b := range boards {
		before := 0
		t.mu.Lock()
		if bk := t.boards[b]; bk != nil {
			before = len(bk.keys)
		}
		t.mu.Unlock()
		bk, err := t.fetch(b)
		if err != nil || len(bk.keys) == before {
			continue
		}
		t.mu.Lock()
		delete(t.locked, b)
		t.mu.Unlock()
		if b == cur {
			_ = t.app.SubscribeBoard(b) // the server sends the history again, now readable
		}
		if t.app.ctx != nil {
			wailsruntime.EventsEmit(t.app.ctx, "chat:keys", b)
		}
	}
}

type pendingResp struct {
	Pending []struct {
		BoardID    string     `json:"board_id"`
		Generation int        `json:"generation"`
		Recipient  textMember `json:"recipient"`
	} `json:"pending"`
}

// runPending seals keys this account holds for members who can see a
// channel but don't have them yet — newcomers, or people who've just
// updated — so they can read its history.
func (t *textCrypto) runPending() {
	id, ok := t.identity()
	if !ok {
		return
	}
	var r pendingResp
	if err := t.call("GET", "/api/e2ee/pending", nil, &r); err != nil {
		if !strings.Contains(err.Error(), "network") {
			log.Printf("e2ee: checking who needs keys: %v", err)
		}
		return
	}
	fetched := map[string]bool{}
	var boxes []map[string]interface{}
	for _, p := range r.Pending {
		enc, ok := t.memberEncKey(p.Recipient, id.domain)
		if !ok {
			continue
		}
		key := t.key(p.BoardID, p.Generation)
		if key == nil && !fetched[p.BoardID] {
			fetched[p.BoardID] = true
			t.fetch(p.BoardID)
			key = t.key(p.BoardID, p.Generation)
		}
		if key == nil {
			continue
		}
		box, err := sealBox(id, p.Recipient.PublicKey, enc, p.BoardID, p.Generation, key)
		if err != nil {
			continue
		}
		boxes = append(boxes, map[string]interface{}{
			"board_id": p.BoardID, "generation": p.Generation,
			"recipient": strings.ToLower(p.Recipient.PublicKey), "box": box,
		})
	}
	if len(boxes) == 0 {
		return
	}
	if err := t.call("POST", "/api/e2ee/boxes", map[string]interface{}{"boxes": boxes}, nil); err != nil {
		log.Printf("e2ee: sharing keys: %v", err)
		return
	}
	log.Printf("e2ee: shared %d channel key(s) with members who needed them", len(boxes))
}

package main

// filecrypt.go — end-to-end encrypted attachments.
//
// Every file gets its own random key and is encrypted on this computer
// before it's uploaded, in 64 KB chunks: each chunk is sealed on its own
// (ChaCha20-Poly1305, nonce = its position, plus a flag on the last one so
// a file can't be cut short unnoticed). The server stores a nameless blob
// ("<uuid>.e2ee") and only ever learns its size.
//
// What the file really is — its name, type, size and key — travels inside
// the message's encrypted content (textPlain.A). The message's plain
// attachment list keeps just the blob's address, so the server can still
// tell which files belong to a message (to delete them with it).
//
// Showing one: the page asks for /e2ee-file/<stored name>, which this app's
// own asset handler answers (serveE2EEFile) by fetching the encrypted
// chunks from the server and decrypting them. A Range request (a video
// seeking) fetches and decrypts only the chunks it covers. Downloading one,
// or a zip of all of a message's files, goes through the browser as before,
// from a link to this app that decrypts as it streams (serveDownload).

import (
	"archive/zip"
	"context"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	fileChunk       = 64 * 1024         // plaintext bytes per chunk
	fileHeaderLen   = 8                 // "CCE2", version, 3 reserved
	fileLocalPrefix = "/e2ee-file/"     // what the page loads them from
	fileBlobName    = "attachment.e2ee" // what the server is told the file's called
	fileOpaqueName  = "encrypted"
	fileOpaqueMime  = "application/octet-stream"
	fileRangeCap    = 4 << 20 // most decrypted bytes one Range answer carries (the player asks again)
)

var fileMagic = [fileHeaderLen]byte{'C', 'C', 'E', '2', 1, 0, 0, 0}

// fileMeta is an encrypted attachment, as described inside its message.
type fileMeta struct {
	URL     string `json:"u"` // the blob on the server: /api/files/<stored name>
	Name    string `json:"n"`
	Mime    string `json:"m"`
	Size    int64  `json:"z"` // plaintext bytes
	Key     string `json:"k"` // base64
	Spoiler bool   `json:"s,omitempty"`
	// Missing: an imported attachment the export didn't include
	// (chriscord-import) — there's no file, just its name, shown as
	// "not found".
	Missing bool `json:"x,omitempty"`
}

type fileEntry struct {
	meta fileMeta
	aead cipher.AEAD
}

// ── The format ────────────────────────────────────────────────────────────────

func fileChunks(size int64) int64 {
	if size <= 0 {
		return 1 // an empty file is one empty chunk
	}
	return (size + fileChunk - 1) / fileChunk
}

func fileCipherSize(size int64) int64 {
	return fileHeaderLen + size + chacha20poly1305.Overhead*fileChunks(size)
}

func chunkNonce(i int64, last bool) []byte {
	n := make([]byte, chacha20poly1305.NonceSize)
	binary.BigEndian.PutUint64(n[:8], uint64(i))
	if last {
		n[8] = 1
	}
	return n
}

// chunkPlainLen is how many plaintext bytes chunk i of a size-byte file holds.
func chunkPlainLen(size, i int64) int64 {
	if i == fileChunks(size)-1 {
		return size - i*fileChunk
	}
	return fileChunk
}

// encryptFile writes src to dst encrypted under aead; returns the
// plaintext size.
func encryptFile(dst io.Writer, src io.Reader, aead cipher.AEAD) (int64, error) {
	if _, err := dst.Write(fileMagic[:]); err != nil {
		return 0, err
	}
	cur, next := make([]byte, fileChunk), make([]byte, fileChunk)
	n, err := io.ReadFull(src, cur)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return 0, err
	}
	out := make([]byte, 0, fileChunk+chacha20poly1305.Overhead)
	var total int64
	for i := int64(0); ; i++ {
		// Read ahead, to know whether this chunk is the last.
		m := 0
		if n == fileChunk {
			m, err = io.ReadFull(src, next)
			if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
				return 0, err
			}
		}
		last := m == 0
		out = aead.Seal(out[:0], chunkNonce(i, last), cur[:n], nil)
		if _, err := dst.Write(out); err != nil {
			return 0, err
		}
		total += int64(n)
		if last {
			return total, nil
		}
		cur, next, n = next, cur, m
	}
}

// decryptChunks reads chunks first..last (inclusive) of a size-byte file
// from src, which starts at chunk first, and writes the plaintext bytes
// from offset from to offset to (inclusive) to dst.
func decryptChunks(dst io.Writer, src io.Reader, aead cipher.AEAD, size, first, last, from, to int64) error {
	total := fileChunks(size)
	buf := make([]byte, fileChunk+chacha20poly1305.Overhead)
	plain := make([]byte, 0, fileChunk)
	for i := first; i <= last; i++ {
		ct := buf[:chunkPlainLen(size, i)+chacha20poly1305.Overhead]
		if _, err := io.ReadFull(src, ct); err != nil {
			return fmt.Errorf("reading chunk %d: %w", i, err)
		}
		var err error
		plain, err = aead.Open(plain[:0], chunkNonce(i, i == total-1), ct, nil)
		if err != nil {
			return fmt.Errorf("chunk %d didn't decrypt (tampered with?)", i)
		}
		start, end := i*fileChunk, i*fileChunk+int64(len(plain)) // this chunk's span
		lo, hi := max(start, from), min(end, to+1)
		if lo < hi {
			if _, err := dst.Write(plain[lo-start : hi-start]); err != nil {
				return err
			}
		}
	}
	return nil
}

func newFileKey() ([]byte, cipher.AEAD, error) {
	key := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, nil, err
	}
	aead, err := chacha20poly1305.New(key)
	return key, aead, err
}

// fileTypes: the usual media types, the same on every computer (Windows
// only knows what's in its registry). The page decides how to show a file
// — picture, player or download chip — from its type.
var fileTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".avif": "image/avif", ".bmp": "image/bmp", ".svg": "image/svg+xml",
	".mp4": "video/mp4", ".m4v": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime",
	".mkv": "video/x-matroska", ".avi": "video/x-msvideo",
	".mp3": "audio/mpeg", ".ogg": "audio/ogg", ".oga": "audio/ogg", ".opus": "audio/ogg",
	".wav": "audio/wav", ".flac": "audio/flac", ".m4a": "audio/mp4", ".aac": "audio/aac",
	".pdf": "application/pdf", ".txt": "text/plain", ".zip": "application/zip",
}

func mimeOf(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if m, ok := fileTypes[ext]; ok {
		return m
	}
	m := mime.TypeByExtension(ext)
	if m == "" {
		return fileOpaqueMime
	}
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	return m
}

// ── Keeping track of files ────────────────────────────────────────────────────

// remember records an encrypted file's details and key, and returns the
// address the page loads it from.
func (t *textCrypto) remember(m fileMeta) (string, error) {
	stored := path.Base(m.URL)
	if !strings.HasPrefix(m.URL, "/api/files/") || stored == "" || stored == "." || stored == "/" {
		return "", errors.New("not a file on this server")
	}
	key, err := base64.StdEncoding.DecodeString(m.Key)
	if err != nil || len(key) != chacha20poly1305.KeySize {
		return "", errors.New("bad file key")
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return "", err
	}
	t.mu.Lock()
	t.files[stored] = &fileEntry{meta: m, aead: aead}
	t.mu.Unlock()
	return fileLocalPrefix + stored, nil
}

func (t *textCrypto) fileFor(local string) *fileEntry {
	stored, ok := strings.CutPrefix(local, fileLocalPrefix)
	if !ok {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.files[stored]
}

// isBlob: one of a message's plain attachment entries that just points at
// an encrypted file (its real details are inside the message).
func isBlob(a Attachment) bool {
	return a.Mime == fileOpaqueMime && strings.HasSuffix(a.URL, ".e2ee")
}

// showAttachments is what the page gets as a message's attachments: plain
// ones as they are, and the encrypted ones as described inside the message,
// at their local addresses. For a message that didn't open, the blob
// entries are left out (there's nothing to show).
func (t *textCrypto) showAttachments(msgID string, plain []Attachment, state string, files []fileMeta) []Attachment {
	out := make([]Attachment, 0, len(plain)+len(files))
	for _, a := range plain {
		if !isBlob(a) {
			out = append(out, a)
		}
	}
	if state != "ok" && state != "forged" {
		return out
	}
	kept := make([]fileMeta, 0, len(files))
	for _, f := range files {
		if f.Missing {
			kept = append(kept, f) // (kept through edits, like the rest)
			out = append(out, Attachment{Name: f.Name, Mime: f.Mime, Spoiler: f.Spoiler, Missing: true})
			continue
		}
		local, err := t.remember(f)
		if err != nil {
			log.Printf("e2ee: an attachment in %s: %v", msgID, err)
			continue
		}
		kept = append(kept, f)
		out = append(out, Attachment{URL: local, Name: f.Name, Mime: f.Mime, Spoiler: f.Spoiler})
	}
	if msgID != "" {
		t.mu.Lock()
		t.msgFiles[msgID] = kept
		t.mu.Unlock()
	}
	return out
}

// sealMessage encrypts a message for sending: its text plus the details of
// any encrypted attachments (uploaded through UploadFileWithProgress), whose
// plain entries are swapped for bare blob addresses.
func (t *textCrypto) sealMessage(board, text string, atts []Attachment) (string, []Attachment, error) {
	var files []fileMeta
	out := make([]Attachment, 0, len(atts))
	for _, a := range atts {
		if a.Missing {
			continue // a "not found" placeholder: nothing to send
		}
		e := t.fileFor(a.URL)
		if e == nil {
			if strings.HasPrefix(a.URL, fileLocalPrefix) {
				return "", nil, errors.New("one of the attachments isn't available any more — attach it again")
			}
			out = append(out, a) // not ours to encrypt (e.g. from before)
			continue
		}
		m := e.meta
		if a.Name != "" {
			m.Name = a.Name
		}
		m.Spoiler = a.Spoiler
		files = append(files, m)
		out = append(out, Attachment{URL: m.URL, Name: fileOpaqueName, Mime: fileOpaqueMime})
	}
	content, err := t.sealWith(board, text, files, previewFor(text))
	return content, out, err
}

// sealEdit encrypts an edited message's new text, keeping its attachments.
func (t *textCrypto) sealEdit(msgID, text string) (string, error) {
	t.mu.Lock()
	files := t.msgFiles[msgID]
	t.mu.Unlock()
	return t.sealWith(t.boardOf(msgID), text, files, previewFor(text))
}

// ── Uploading ─────────────────────────────────────────────────────────────────

// uploadEncrypted encrypts src while uploading it, and returns the result as
// the page sees it: the file's local address, real name and type.
func (a *App) uploadEncrypted(domain, token, filename string, src io.Reader) (*UploadResult, error) {
	key, aead, err := newFileKey()
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	sizeCh := make(chan int64, 1)
	go func() {
		fw, err := mw.CreateFormFile("file", fileBlobName)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		n, err := encryptFile(fw, src, aead)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		sizeCh <- n
		mw.Close()
		pw.Close()
	}()

	// No limit on how long the whole upload takes (a big file on a slow
	// connection can take hours); it's given up on only if it stops moving:
	// nothing sent for uploadStall, or no answer uploadAnswerWait after the
	// last byte.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := &uploadWatch{r: pr}
	body.touch()
	stalled := make(chan string, 1)
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				idle, done := body.idle()
				switch {
				case !done && idle > uploadStall:
					stalled <- "the connection stopped sending"
					cancel()
					return
				case done && idle > uploadAnswerWait:
					stalled <- "the server didn't answer after the file was sent"
					cancel()
					return
				}
			}
		}
	}()
	req, err := http.NewRequestWithContext(ctx, "POST", normaliseHTTP(domain)+"/api/upload", body)
	if err != nil {
		pr.CloseWithError(err)
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Session-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		pr.CloseWithError(err)
		select {
		case why := <-stalled:
			return nil, fmt.Errorf("upload failed: %s", why)
		default:
		}
		return nil, fmt.Errorf("upload failed: %w", err)
	}
	cancel() // answered: the watchdog's job is done (the body below is small)
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e map[string]string
		json.NewDecoder(resp.Body).Decode(&e)
		if msg, ok := e["error"]; ok {
			return nil, fmt.Errorf("%s", msg)
		}
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var result UploadResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	var size int64
	select {
	case size = <-sizeCh:
	case <-time.After(5 * time.Second):
		return nil, errors.New("upload finished before the file was read")
	}
	m := fileMeta{URL: result.URL, Name: filename, Mime: mimeOf(filename), Size: size,
		Key: base64.StdEncoding.EncodeToString(key)}
	local, err := a.text.remember(m)
	if err != nil {
		return nil, err
	}
	return &UploadResult{URL: local, Filename: filename, Mime: m.Mime}, nil
}

const (
	uploadStall      = 2 * time.Minute  // most an upload may go without sending anything
	uploadAnswerWait = 10 * time.Minute // most to wait for the server's answer once it's all sent
)

// uploadWatch notes when the upload last moved, and when it finished.
type uploadWatch struct {
	r    io.Reader
	last atomic.Int64 // unix nanoseconds of the last read that got data
	done atomic.Bool
}

func (p *uploadWatch) touch() { p.last.Store(time.Now().UnixNano()) }

func (p *uploadWatch) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.touch()
	}
	if err == io.EOF {
		p.done.Store(true)
		p.touch()
	}
	return n, err
}

// idle: how long since it last moved, and whether it's all been sent.
func (p *uploadWatch) idle() (time.Duration, bool) {
	return time.Since(time.Unix(0, p.last.Load())), p.done.Load()
}

// ── Showing (the page's /e2ee-file/ requests) ─────────────────────────────────

var errNotConnected = errors.New("not connected to a server")

// fetchBlob gets bytes from..to (inclusive) of an encrypted file from the
// server (to < 0: through the end).
func (a *App) fetchBlob(serverURL string, from, to int64) (io.ReadCloser, error) {
	a.mu.Lock()
	domain, token := a.domain, a.token
	a.mu.Unlock()
	if domain == "" {
		// Between servers (or reconnecting): the page asked for a picture
		// that was still loading. It asks again once it's back.
		return nil, errNotConnected
	}
	req, err := http.NewRequest("GET", normaliseHTTP(domain)+serverURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Session-Token", token)
	if from > 0 || to >= 0 {
		r := "bytes=" + strconv.FormatInt(from, 10) + "-"
		if to >= 0 {
			r += strconv.FormatInt(to, 10)
		}
		req.Header.Set("Range", r)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusPartialContent:
		return resp.Body, nil
	case resp.StatusCode == http.StatusOK:
		// The whole file came back: skip to where we asked to start.
		if _, err := io.CopyN(io.Discard, resp.Body, from); err != nil {
			resp.Body.Close()
			return nil, err
		}
		return resp.Body, nil
	default:
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
}

// parseRange reads a single "bytes=a-b" / "a-" / "-n" range for a size-byte
// file. ok is false when there's no usable one (then: the whole file).
func parseRange(h string, size int64) (from, to int64, ok bool) {
	spec, found := strings.CutPrefix(strings.TrimSpace(h), "bytes=")
	if !found || strings.Contains(spec, ",") || size <= 0 {
		return 0, 0, false
	}
	a, b, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}
	switch {
	case a == "" && b != "": // the last n bytes
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		return max(size-n, 0), size - 1, true
	case a != "":
		f, err := strconv.ParseInt(a, 10, 64)
		if err != nil || f < 0 || f >= size {
			return 0, 0, false
		}
		t := size - 1
		if b != "" {
			if v, err := strconv.ParseInt(b, 10, 64); err == nil && v < t {
				t = v
			}
		}
		if t < f {
			return 0, 0, false
		}
		return f, t, true
	}
	return 0, 0, false
}

// serveE2EEFile answers the page's /e2ee-file/<stored name> requests.
func (a *App) serveE2EEFile(w http.ResponseWriter, r *http.Request) {
	e := a.text.fileFor(r.URL.Path)
	if e == nil {
		http.NotFound(w, r)
		return
	}
	size := e.meta.Size
	from, to, ranged := parseRange(r.Header.Get("Range"), size)
	if !ranged {
		from, to = 0, size-1
	} else if to-from+1 > fileRangeCap {
		to = from + fileRangeCap - 1 // a player asks for the rest as it needs it
	}

	h := w.Header()
	h.Set("Content-Type", e.meta.Mime)
	h.Set("Accept-Ranges", "bytes")
	h.Set("Cache-Control", "private, max-age=86400, immutable")
	h.Set("X-Content-Type-Options", "nosniff")
	if size == 0 {
		h.Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
		return
	}
	first, last := from/fileChunk, to/fileChunk
	cStart := fileHeaderLen + first*(fileChunk+chacha20poly1305.Overhead)
	cEnd := min(fileHeaderLen+(last+1)*(fileChunk+chacha20poly1305.Overhead), fileCipherSize(size)) - 1
	body, err := a.fetchBlob(e.meta.URL, cStart, cEnd)
	if errors.Is(err, errNotConnected) {
		http.Error(w, "not connected", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		log.Printf("e2ee: fetching %s: %v", e.meta.URL, err)
		http.Error(w, "couldn't fetch the file", http.StatusBadGateway)
		return
	}
	defer body.Close()
	h.Set("Content-Length", strconv.FormatInt(to-from+1, 10))
	if ranged {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if err := decryptChunks(w, body, e.aead, size, first, last, from, to); err != nil {
		log.Printf("e2ee: %s: %v", e.meta.URL, err) // the response is cut short
	}
}

// ── Saving ────────────────────────────────────────────────────────────────────

// decryptTo writes a whole encrypted file, decrypted, to dst.
func (a *App) decryptTo(dst io.Writer, e *fileEntry) error {
	body, err := a.fetchBlob(e.meta.URL, 0, -1)
	if err != nil {
		return err
	}
	defer body.Close()
	var magic [fileHeaderLen]byte
	if _, err := io.ReadFull(body, magic[:]); err != nil || magic != fileMagic {
		return errors.New("not an encrypted Chriscord file")
	}
	size := e.meta.Size
	return decryptChunks(dst, body, e.aead, size, 0, fileChunks(size)-1, 0, size-1)
}

// ── Downloads through the browser ─────────────────────────────────────────────
//
// Downloads open in your browser like before, so it saves them its usual
// way (download bar, progress, your downloads folder). The browser can't
// decrypt the server's blobs, though, so the link it's given is to this
// app: a small web server on this computer only (127.0.0.1, a random port),
// which decrypts as it streams. Each link is a random token for one file
// (or one message's zip), usable for linkTTL — like the server's own
// download links — and only works while Chriscord is open.

const linkTTL = 5 * time.Minute

type localLink struct {
	name    string       // what the browser saves it as
	files   []*fileEntry // one file, or a message's files
	zip     bool
	expires time.Time
}

type localDownloads struct {
	mu    sync.Mutex
	addr  string // "127.0.0.1:<port>" once started
	links map[string]*localLink
}

var downloads localDownloads

// link starts the local server if needed and returns a download link for l.
func (a *App) downloadLink(l *localLink) (string, error) {
	downloads.mu.Lock()
	defer downloads.mu.Unlock()
	if downloads.addr == "" {
		ln, err := net.Listen("tcp", "127.0.0.1:0") // this computer only
		if err != nil {
			return "", fmt.Errorf("couldn't start the download link: %w", err)
		}
		downloads.addr = ln.Addr().String()
		downloads.links = map[string]*localLink{}
		srv := &http.Server{Handler: http.HandlerFunc(a.serveDownload), ReadHeaderTimeout: 10 * time.Second}
		go srv.Serve(ln)
	}
	now := time.Now()
	for t, old := range downloads.links { // tidy up expired ones
		if now.After(old.expires) {
			delete(downloads.links, t)
		}
	}
	tok := make([]byte, 16)
	if _, err := rand.Read(tok); err != nil {
		return "", err
	}
	token := hex.EncodeToString(tok)
	l.expires = now.Add(linkTTL)
	downloads.links[token] = l
	return "http://" + downloads.addr + "/dl/" + token + "/" + url.PathEscape(l.name), nil
}

// serveDownload: the browser fetching one of those links.
func (a *App) serveDownload(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/dl/")
	token, _, _ := strings.Cut(rest, "/")
	downloads.mu.Lock()
	l := downloads.links[token]
	downloads.mu.Unlock()
	if !ok || l == nil || time.Now().After(l.expires) {
		http.Error(w, "This download link has expired — click Download in Chriscord again.", http.StatusGone)
		return
	}
	h := w.Header()
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": l.name}))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	if !l.zip {
		e := l.files[0]
		h.Set("Content-Type", e.meta.Mime)
		h.Set("Content-Length", strconv.FormatInt(e.meta.Size, 10))
		if r.Method == http.MethodHead {
			return
		}
		if err := a.decryptTo(w, e); err != nil {
			log.Printf("e2ee: download of %s: %v", e.meta.Name, err) // the browser sees it fail
		}
		return
	}
	h.Set("Content-Type", "application/zip")
	if r.Method == http.MethodHead {
		return
	}
	zw := zip.NewWriter(w)
	used := map[string]int{}
	for _, e := range l.files {
		name := safeFileName(e.meta.Name)
		if n := used[name]; n > 0 { // the same name twice: "photo (2).png"
			ext := filepath.Ext(name)
			name = fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(name, ext), n+1, ext)
		}
		used[safeFileName(e.meta.Name)]++
		// Stored, not compressed: like the server's zips (media doesn't
		// shrink, and it keeps this as fast as the download).
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store, Modified: time.Now()})
		if err != nil {
			return
		}
		if err := a.decryptTo(fw, e); err != nil {
			log.Printf("e2ee: zipping %s: %v", e.meta.Name, err)
			return // cut short: the browser shows the download as failed
		}
	}
	zw.Close()
}

// AttachmentDownloadLink gives a link for the browser to download one
// encrypted attachment (its /e2ee-file/ address) from, decrypted.
func (a *App) AttachmentDownloadLink(local, name string) (string, error) {
	e := a.text.fileFor(local)
	if e == nil {
		return "", errors.New("that file isn't available — reopen the channel and try again")
	}
	if name == "" {
		name = e.meta.Name
	}
	return a.downloadLink(&localLink{name: safeFileName(name), files: []*fileEntry{e}})
}

// AttachmentsZipLink gives a link for the browser to download all of a
// message's encrypted attachments as one .zip, made here (the server only
// has the encrypted blobs, so it can't make it).
func (a *App) AttachmentsZipLink(msgID string) (string, error) {
	a.text.mu.Lock()
	files := append([]fileMeta(nil), a.text.msgFiles[msgID]...)
	a.text.mu.Unlock()
	entries := make([]*fileEntry, 0, len(files))
	for _, f := range files {
		if e := a.text.fileFor(fileLocalPrefix + path.Base(f.URL)); e != nil {
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		return "", errors.New("this message has no encrypted files")
	}
	return a.downloadLink(&localLink{name: "attachments.zip", files: entries, zip: true})
}

// safeFileName keeps a file name from pointing anywhere else.
func safeFileName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.TrimLeft(name, ".")
	name = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	if name == "" {
		return "file"
	}
	return name
}

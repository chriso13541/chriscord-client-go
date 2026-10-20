package main

// msgsig.go — who wrote a message (the SDK's msgsig.go matches).
//
// A channel's key is shared by everyone who may read it, so it can't say
// who wrote a message: anyone with it (a bot or plugin the channel was
// given to, say) could seal one claiming to be someone else. So each
// message also carries its writer's identity key (K) and their signature
// (S) over what it says. The app checks the signature, that the key is the
// one the writer is known by, and (on the signed member list) what they
// are — so a bot's or plugin's message is always marked as one, whatever
// the server says, and can't pass for a person's.

import (
	"crypto/ed25519"
	"encoding/hex"
	"strconv"
	"strings"
)

const msgSigContext = "chriscord-e2ee-v1/msg-sig\x00"

// msgSigFile and msgSigPreview are what's signed of an attachment and a
// link preview.
type msgSigFile struct {
	URL, Name, Mime string
	Size            int64
	Key             string
	Spoiler, Gone   bool
}

// msgSigMessage is what a message's writer signs: the channel, the key
// generation, who they say they are, the text, its attachments (each one's
// file and key) and its link preview.
func msgSigMessage(board string, gen int, user, text string, files []msgSigFile, preview []string) []byte {
	var b strings.Builder
	b.WriteString(msgSigContext)
	for _, s := range []string{board, strconv.Itoa(gen), user, text, strconv.Itoa(len(files))} {
		b.WriteString(strconv.Itoa(len(s)))
		b.WriteByte(':')
		b.WriteString(s)
	}
	for _, f := range files {
		for _, s := range []string{f.URL, f.Name, f.Mime, strconv.FormatInt(f.Size, 10), f.Key, strconv.FormatBool(f.Spoiler), strconv.FormatBool(f.Gone)} {
			b.WriteString(strconv.Itoa(len(s)))
			b.WriteByte(':')
			b.WriteString(s)
		}
	}
	b.WriteString(strconv.Itoa(len(preview)))
	for _, s := range preview {
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(len(s)))
		b.WriteByte(':')
		b.WriteString(s)
	}
	return []byte(b.String())
}

// msgSigCheck: whether sigHex is keyHex's signature over msg.
func msgSigCheck(keyHex, sigHex string, msg []byte) bool {
	k, err1 := hex.DecodeString(keyHex)
	s, err2 := hex.DecodeString(sigHex)
	return err1 == nil && err2 == nil && len(k) == ed25519.PublicKeySize && ed25519.Verify(k, msg, s)
}

// ── This app's message contents ──────────────────────────────────────────────

func msgSigFiles(fs []fileMeta) []msgSigFile {
	out := make([]msgSigFile, len(fs))
	for i, f := range fs {
		out[i] = msgSigFile{URL: f.URL, Name: f.Name, Mime: f.Mime, Size: f.Size, Key: f.Key, Spoiler: f.Spoiler, Gone: f.Missing}
	}
	return out
}

func msgSigPreviewOf(l *textLinkPreview) []string {
	if l == nil {
		return nil
	}
	return []string{l.U, l.T, l.D, l.S, l.K, l.V, l.I}
}

// signPlain signs p as written by this account (K, S).
func signPlain(p *textPlain, board string, gen int, id textIdentity) {
	p.K = id.idHex
	p.S = hex.EncodeToString(ed25519.Sign(id.idPriv, msgSigMessage(board, gen, p.U, p.T, msgSigFiles(p.A), msgSigPreviewOf(p.L))))
}

// msgWriter is what a message's signature says about its writer.
type msgWriter struct {
	Kind     string // "bot" or "plugin" (as the signed member list has them), else ""
	Unsigned bool
	Head     string // the signed member list's head the writer saw (textPlain.RH)
}

// checkWriter checks a message's signature: by the key it carries, over
// what it says, and that key the one its writer is known by (the server's
// member list for the channel, which the app pins) and on the signed
// member list. why is "" when it checks out (or isn't signed).
func (t *textCrypto) checkWriter(board string, gen int, p textPlain) (w msgWriter, why string) {
	if p.S == "" || p.K == "" {
		return msgWriter{Unsigned: true}, ""
	}
	if !msgSigCheck(p.K, p.S, msgSigMessage(board, gen, p.U, p.T, msgSigFiles(p.A), msgSigPreviewOf(p.L))) {
		return w, "its signature doesn't check out"
	}
	key := strings.ToLower(p.K)
	if known := t.memberKey(board, p.U); known != "" && known != key {
		return w, "it was signed by someone else's key"
	}
	kind, state := t.app.rosterKindOf(key)
	if state == rosterActive {
		if kind == "" && !t.app.rosterKnew(key) {
			return w, "its writer has never been on the signed member list"
		}
		if kind == kindBot || kind == kindPlugin {
			w.Kind = kind
		}
	}
	return w, ""
}

// memberKey: the identity key the server lists user with in board ("" if
// it doesn't).
func (t *textCrypto) memberKey(board, user string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	bk := t.boards[board]
	if bk == nil {
		return ""
	}
	for _, m := range bk.members {
		if m.Username == user {
			return strings.ToLower(m.PublicKey)
		}
	}
	return ""
}

package main

// e2ee_frame.go — the pure crypto pieces of call encryption (see e2ee.go
// for how they're used): sealing and opening media frames, fitting a
// sealed H.264 frame back into something the RTP packetiser accepts,
// wrapping one person's sending key for another, and the call code.
// Nothing in here knows about the app, the network or WebRTC.
//
// A sealed frame is
//
//	ciphertext | 16-byte tag | counter (4, big-endian) | key id (1) | magic (2)
//
// ChaCha20-Poly1305 under the sender's current key. The nonce is the
// counter (one counter per key, shared by all of the sender's tracks, so a
// nonce is never reused), and the trailer plus the media kind are the
// associated data — so a frame can't be moved between audio and video,
// or have its key id or counter changed, without failing to open.

import (
	"bytes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

const (
	e2eeTrailerLen = 4 + 1 + 2
	e2eeOverhead   = chacha20poly1305.Overhead + e2eeTrailerLen
	e2eeMagic0     = 0xCC // "chriscord"
	e2eeMagic1     = 0xE2 // "e2ee"

	// The NAL header byte a sealed H.264 frame travels under: type 1 (a
	// plain slice) — anything 1..23 goes through pion's packetiser and
	// depacketiser untouched, and it's never SPS/PPS (7/8), which the
	// packetiser would hold back to bundle with the next frame.
	e2eeH264NALHeader = 0x01

	// What the media kinds are called in the associated data.
	kindAudio = 'a'
	kindVP8   = 'v'
	kindH264  = 'h'
)

var errE2EEFrame = errors.New("not a sealed frame")

// newFrameAEAD makes the cipher for one 32-byte sending key.
func newFrameAEAD(key []byte) (cipher.AEAD, error) { return chacha20poly1305.New(key) }

func frameNonce(counter uint32) []byte {
	n := make([]byte, chacha20poly1305.NonceSize)
	binary.BigEndian.PutUint32(n[8:], counter)
	return n
}

// sealFrameWith encrypts one media frame. kind is kindAudio, kindVP8 or
// kindH264 (an H.264 frame comes back already wrapped as a NAL unit).
func sealFrameWith(aead cipher.AEAD, kid byte, counter uint32, kind byte, frame []byte) []byte {
	trailer := [e2eeTrailerLen]byte{}
	binary.BigEndian.PutUint32(trailer[:4], counter)
	trailer[4], trailer[5], trailer[6] = kid, e2eeMagic0, e2eeMagic1
	aad := append(trailer[:], kind)
	out := make([]byte, 0, len(frame)+e2eeOverhead)
	out = aead.Seal(out, frameNonce(counter), frame, aad)
	out = append(out, trailer[:]...)
	if kind == kindH264 {
		return wrapH264(out)
	}
	return out
}

// frameHeader reads a sealed frame's key id and counter (after
// unwrapping, for H.264) without decrypting it. Returns the bytes to open.
func frameHeader(kind byte, frame []byte) (sealed []byte, kid byte, counter uint32, err error) {
	sealed = frame
	if kind == kindH264 {
		if sealed, err = unwrapH264(frame); err != nil {
			return nil, 0, 0, err
		}
	}
	n := len(sealed)
	if n < e2eeOverhead || sealed[n-2] != e2eeMagic0 || sealed[n-1] != e2eeMagic1 {
		return nil, 0, 0, errE2EEFrame
	}
	return sealed, sealed[n-3], binary.BigEndian.Uint32(sealed[n-7 : n-3]), nil
}

// openSealed decrypts a frame frameHeader accepted.
func openSealed(aead cipher.AEAD, kind byte, sealed []byte) ([]byte, error) {
	n := len(sealed)
	trailer := sealed[n-e2eeTrailerLen:]
	counter := binary.BigEndian.Uint32(trailer[:4])
	aad := append(append([]byte(nil), trailer...), kind)
	return aead.Open(nil, frameNonce(counter), sealed[:n-e2eeTrailerLen], aad)
}

// ── H.264 ─────────────────────────────────────────────────────────────────────
//
// A video frame goes through pion's H.264 packetiser, which splits its input
// at start codes (00 00 01) and reads each piece's first byte as a NAL
// header. Ciphertext is random bytes, so it would sometimes contain a start
// code and get cut in half. So the sealed frame is sent as one NAL unit with
// a fixed header byte, and escaped the way H.264 itself does it (emulation
// prevention: a 03 after any 00 00 that's followed by 00..03) so it can't
// contain one. The receiver's depacketiser hands back the same NAL, and the
// escaping is undone before opening. The server only forwards packets and
// never looks inside them, so nothing else changes.

func wrapH264(sealed []byte) []byte {
	out := make([]byte, 0, len(sealed)+len(sealed)/64+8)
	out = append(out, 0, 0, 0, 1, e2eeH264NALHeader)
	zeros := 0
	for _, b := range sealed {
		if zeros >= 2 && b <= 3 {
			out = append(out, 3)
			zeros = 0
		}
		out = append(out, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out // ends with the magic, never 00, so nothing to pad
}

func unwrapH264(au []byte) ([]byte, error) {
	// Skip a leading start code (the depacketiser adds 00 00 00 01).
	switch {
	case bytes.HasPrefix(au, []byte{0, 0, 0, 1}):
		au = au[4:]
	case bytes.HasPrefix(au, []byte{0, 0, 1}):
		au = au[3:]
	}
	if len(au) < 2 || au[0] != e2eeH264NALHeader {
		return nil, errE2EEFrame
	}
	au = au[1:]
	out := make([]byte, 0, len(au))
	zeros := 0
	for i := 0; i < len(au); i++ {
		b := au[i]
		if zeros >= 2 && b == 3 {
			zeros = 0
			continue // an escape byte
		}
		if zeros >= 2 && b <= 2 {
			return nil, errE2EEFrame // a start code: more than one NAL — not ours
		}
		out = append(out, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out, nil
}

// ── Replay protection ─────────────────────────────────────────────────────────

// replayWindow remembers which counters of one key have been seen, over the
// last 4096 — a frame the server sends twice (or late, past that) is
// dropped. Out-of-order arrival within the window is fine.
type replayWindow struct {
	have bool
	max  uint32
	bits [64]uint64
}

func (w *replayWindow) bit(c uint32) (int, uint64) { i := c % 4096; return int(i / 64), 1 << (i % 64) }

// check reports whether c is new, and records it if so.
func (w *replayWindow) check(c uint32) bool {
	if !w.have {
		w.have, w.max = true, c
		i, m := w.bit(c)
		w.bits[i] |= m
		return true
	}
	if c > w.max {
		// Clear what slides out of the window.
		if c-w.max >= 4096 {
			w.bits = [64]uint64{}
		} else {
			for x := w.max + 1; x != c+1; x++ {
				i, m := w.bit(x)
				w.bits[i] &^= m
			}
		}
		w.max = c
	} else if w.max-c >= 4096 {
		return false
	}
	i, m := w.bit(c)
	if w.bits[i]&m != 0 {
		return false
	}
	w.bits[i] |= m
	return true
}

// ── Key exchange ──────────────────────────────────────────────────────────────

// newEphemeral makes this call's X25519 key pair.
func newEphemeral() (priv, pub [32]byte, err error) {
	if _, err = io.ReadFull(rand.Reader, priv[:]); err != nil {
		return
	}
	p, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	copy(pub[:], p)
	return
}

// helloMessage is what a participant signs with their identity key to say
// "this is my key for this call": tied to the server's channel and their
// name, so it can't be replayed into another call or for someone else.
func helloMessage(board, user string, eph []byte) []byte {
	var b bytes.Buffer
	b.WriteString("chriscord-e2ee-v1/hello\x00")
	b.WriteString(board)
	b.WriteByte(0)
	b.WriteString(user)
	b.WriteByte(0)
	b.Write(eph)
	return b.Bytes()
}

func verifyHello(identity ed25519.PublicKey, board, user string, eph, sig []byte) bool {
	return len(identity) == ed25519.PublicKeySize && len(eph) == 32 &&
		ed25519.Verify(identity, helloMessage(board, user, eph), sig)
}

// keyWrapCipher derives the one-off cipher that carries a sending key from
// one participant to another: X25519 between their two call keys, through
// HKDF bound to the channel, both names, both keys and the key id.
func keyWrapCipher(myPriv, peerPub [32]byte, board, from, to string, fromPub, toPub [32]byte, kid uint32) (cipher.AEAD, error) {
	shared, err := curve25519.X25519(myPriv[:], peerPub[:])
	if err != nil {
		return nil, err // the peer's key was a low-order point
	}
	var info bytes.Buffer
	fmt.Fprintf(&info, "chriscord-e2ee-v1/key\x00%s\x00%s\x00%s\x00", board, from, to)
	info.Write(fromPub[:])
	info.Write(toPub[:])
	binary.Write(&info, binary.BigEndian, kid)
	k := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(hkdf.New(sha256.New, shared, nil, info.Bytes()), k); err != nil {
		return nil, err
	}
	return chacha20poly1305.New(k)
}

// wrapKey seals a sending key for one peer: a random nonce, then the
// sealed key.
func wrapKey(myPriv, myPub, peerPub [32]byte, board, from, to string, kid uint32, key []byte) ([]byte, error) {
	c, err := keyWrapCipher(myPriv, peerPub, board, from, to, myPub, peerPub, kid)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, c.NonceSize(), c.NonceSize()+len(key)+c.Overhead())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return c.Seal(nonce, nonce, key, nil), nil
}

func unwrapKey(myPriv, myPub, peerPub [32]byte, board, from, to string, kid uint32, box []byte) ([]byte, error) {
	c, err := keyWrapCipher(myPriv, peerPub, board, from, to, peerPub, myPub, kid)
	if err != nil {
		return nil, err
	}
	if len(box) < c.NonceSize() {
		return nil, errors.New("short key box")
	}
	key, err := c.Open(nil, box[:c.NonceSize()], box[c.NonceSize():], nil)
	if err == nil && len(key) != chacha20poly1305.KeySize {
		err = errors.New("wrong key size")
	}
	return key, err
}

// ── Call code ─────────────────────────────────────────────────────────────────

// callCode is a short code worked out from everyone in the call and their
// identity keys, as this app sees them. Everyone in the call should see the
// same one: if two people read theirs out and they differ, someone isn't
// who the server says (a swapped key) or someone's being shown a different
// call. It only changes when the people in the call do. 20 digits, in four
// groups of five.
func callCode(members map[string]string) string {
	names := make([]string, 0, len(members))
	for n := range members {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	h.Write([]byte("chriscord-e2ee-v1/code\x00"))
	for _, n := range names {
		fmt.Fprintf(h, "%s\x00%s\x00", n, strings.ToLower(members[n]))
	}
	num := new(big.Int).SetBytes(h.Sum(nil))
	digits := fmt.Sprintf("%020s", new(big.Int).Mod(num, new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil)).String())
	return digits[0:5] + " " + digits[5:10] + " " + digits[10:15] + " " + digits[15:20]
}

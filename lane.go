package main

// lane.go — plugin lanes: an end-to-end encrypted line between one
// member's app and a plugin (the same file as the app's lane.go: keep them
// in step).
//
// A plugin shouldn't be able to read the server's channels, and the
// server shouldn't be able to read what members ask plugins. So instead of
// chat messages (which need channel keys) or plain messages through the
// server, a member's app opens a "lane" to the plugin:
//
//   - The app makes a one-off X25519 key and works out a secret with the
//     plugin's published encryption key (signed by the plugin's identity
//     key, which the signed member list says is a plugin). Two keys come
//     from it: one for each direction.
//   - Its first message (the hello) says who's asking: the member's
//     identity key, signing this lane and this plugin, so the server can't
//     pretend to be someone else, or send it on to another plugin.
//   - After that, commands and clicks go one way and answers and panels the
//     other, each sealed (ChaCha20-Poly1305) with a counter that only goes
//     up, so the server can't replay or reorder them.
//
// The server passes the sealed messages along without being able to read
// them: it sees who talks to which plugin, when, and how much.

import (
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

const (
	laneContext = "chriscord-lane-v1"
	laneMaxData = 64 << 10
	// How far a hello's time may be from the plugin's clock.
	laneClockSkew = 10 * time.Minute
)

// Directions, for the counters and the sealing.
const (
	laneToPlugin   byte = 1
	laneFromPlugin byte = 2
)

// laneHello is a lane's first message, from the member's app.
type laneHello struct {
	V      int    `json:"v"`
	Plugin string `json:"plugin"` // the plugin's identity key, hex
	Key    string `json:"key"`    // the member's identity key, hex
	Name   string `json:"name"`   // the member's name (as they're known on the server)
	At     int64  `json:"at"`     // unix seconds
	Sig    string `json:"sig"`    // the member's signature: laneHelloMessage
}

// laneHelloMessage is what the member's identity key signs.
func laneHelloMessage(sid, plugin string, at int64) []byte {
	b := []byte(laneContext + "/hello\x00" + sid + "\x00" + plugin + "\x00")
	return binary.BigEndian.AppendUint64(b, uint64(at))
}

// laneKeys is one lane's keys and counters (one side's view).
type laneKeys struct {
	SID    string
	toP    cipher.AEAD
	fromP  cipher.AEAD
	sentN  uint64 // the next counter this side sends with
	gotN   uint64 // the highest counter received (+1); 0: none yet
	gotAny bool
	// missed: counters skipped — messages the server held back (or lost).
	// It can't change or replay them, but it can drop them; this shows it.
	missed  uint64
	created time.Time
	used    time.Time
}

// laneDerive works out a lane's keys from the shared secret.
func laneDerive(shared []byte, eph, pluginEnc [32]byte) (*laneKeys, error) {
	sum := sha256.Sum256(eph[:])
	sid := hex.EncodeToString(sum[:16])
	salt := append(append([]byte{}, eph[:]...), pluginEnc[:]...)
	r := hkdf.New(sha256.New, shared, salt, []byte(laneContext))
	k1, k2 := make([]byte, 32), make([]byte, 32)
	if _, err := io.ReadFull(r, k1); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(r, k2); err != nil {
		return nil, err
	}
	a1, err := chacha20poly1305.New(k1)
	if err != nil {
		return nil, err
	}
	a2, err := chacha20poly1305.New(k2)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	return &laneKeys{SID: sid, toP: a1, fromP: a2, created: now, used: now}, nil
}

func laneNonce(n uint64) []byte {
	nonce := make([]byte, chacha20poly1305.NonceSize)
	binary.BigEndian.PutUint64(nonce[4:], n)
	return nonce
}

func laneAAD(sid string, dir byte) []byte { return append([]byte(laneContext+"\x00"+sid+"\x00"), dir) }

// seal: one message from this side (dir), with the next counter.
func (k *laneKeys) seal(dir byte, plain []byte) (n uint64, data string) {
	aead := k.toP
	if dir == laneFromPlugin {
		aead = k.fromP
	}
	n = k.sentN
	k.sentN++
	k.used = time.Now()
	return n, base64.StdEncoding.EncodeToString(aead.Seal(nil, laneNonce(n), plain, laneAAD(k.SID, dir)))
}

// open: one message to this side (dir: the direction it travelled). Its
// counter has to be higher than any before (no replays, no going back).
func (k *laneKeys) open(dir byte, n uint64, data string) ([]byte, error) {
	if len(data) > laneMaxData*4/3+8 {
		return nil, errors.New("too big")
	}
	if k.gotAny && n < k.gotN {
		return nil, errors.New("an old or repeated message")
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, errors.New("unreadable")
	}
	aead := k.toP
	if dir == laneFromPlugin {
		aead = k.fromP
	}
	plain, err := aead.Open(nil, laneNonce(n), raw, laneAAD(k.SID, dir))
	if err != nil {
		return nil, errors.New("doesn't open (tampered with, or not for this lane)")
	}
	switch {
	case k.gotAny && n > k.gotN:
		k.missed += n - k.gotN
	case !k.gotAny && n > 0:
		k.missed += n
	}
	k.gotN, k.gotAny = n+1, true
	k.used = time.Now()
	return plain, nil
}

// laneOpenFromApp: the member's side — a new lane to a plugin. Returns the
// lane, its one-off public key and the sealed hello (counter 0).
func laneOpenFromApp(pluginKey string, pluginEnc [32]byte, me ed25519.PrivateKey, name string) (*laneKeys, string, string, error) {
	var ephPriv [32]byte
	if _, err := rand.Read(ephPriv[:]); err != nil {
		return nil, "", "", err
	}
	ephPubB, err := curve25519.X25519(ephPriv[:], curve25519.Basepoint)
	if err != nil {
		return nil, "", "", err
	}
	shared, err := curve25519.X25519(ephPriv[:], pluginEnc[:])
	if err != nil {
		return nil, "", "", err
	}
	var ephPub [32]byte
	copy(ephPub[:], ephPubB)
	k, err := laneDerive(shared, ephPub, pluginEnc)
	if err != nil {
		return nil, "", "", err
	}
	at := time.Now().Unix()
	h := laneHello{V: 1, Plugin: strings.ToLower(pluginKey), Key: hex.EncodeToString(me.Public().(ed25519.PublicKey)), Name: name, At: at}
	h.Sig = hex.EncodeToString(ed25519.Sign(me, laneHelloMessage(k.SID, h.Plugin, at)))
	plain, _ := json.Marshal(h)
	_, data := k.seal(laneToPlugin, plain)
	return k, base64.StdEncoding.EncodeToString(ephPub[:]), data, nil
}

// laneAcceptAtPlugin: the plugin's side — a hello from a member's app.
// Checks it was sealed for this plugin's key, signed by the member it says,
// for this plugin and this lane, and recently.
func laneAcceptAtPlugin(myKey string, encPriv [32]byte, ephB64, data string, now time.Time) (*laneKeys, *laneHello, error) {
	ephRaw, err := base64.StdEncoding.DecodeString(ephB64)
	if err != nil || len(ephRaw) != 32 {
		return nil, nil, errors.New("bad lane key")
	}
	var eph [32]byte
	copy(eph[:], ephRaw)
	shared, err := curve25519.X25519(encPriv[:], eph[:])
	if err != nil {
		return nil, nil, err
	}
	encPubB, _ := curve25519.X25519(encPriv[:], curve25519.Basepoint)
	var encPub [32]byte
	copy(encPub[:], encPubB)
	k, err := laneDerive(shared, eph, encPub)
	if err != nil {
		return nil, nil, err
	}
	plain, err := k.open(laneToPlugin, 0, data)
	if err != nil {
		return nil, nil, err
	}
	var h laneHello
	if err := json.Unmarshal(plain, &h); err != nil || h.V != 1 {
		return nil, nil, errors.New("unreadable hello")
	}
	if h.Plugin != strings.ToLower(myKey) {
		return nil, nil, errors.New("a hello for another plugin")
	}
	pub, err1 := hex.DecodeString(h.Key)
	sig, err2 := hex.DecodeString(h.Sig)
	if err1 != nil || err2 != nil || len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, laneHelloMessage(k.SID, h.Plugin, h.At), sig) {
		return nil, nil, errors.New("the hello isn't signed by who it says")
	}
	if d := now.Sub(time.Unix(h.At, 0)); d > laneClockSkew || d < -laneClockSkew {
		return nil, nil, errors.New("an old hello")
	}
	h.Key = strings.ToLower(h.Key)
	return k, &h, nil
}

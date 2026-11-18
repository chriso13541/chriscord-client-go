package main

// pqcrypt.go — the quantum-safe half of key exchange (hybrid X25519 +
// ML-KEM-768), shared by channel keys (textcrypt.go) and call keys
// (e2ee.go / e2ee_frame.go).
//
// Why: everything the server relays — sealed channel keys, call key
// messages — can be kept forever ("harvest now, decrypt later"). X25519
// alone falls to a large enough quantum computer, so whoever kept those
// boxes could open them then. Each box is now sealed under a key that
// needs BOTH secrets:
//
//   - the same X25519 exchange as before (sender's key × recipient's key).
//     This is also what proves who sealed it: only the sender or the
//     recipient can work it out, so a server can't slip someone a key of
//     its own choosing. (A KEM on its own can't do that — anyone can
//     encapsulate to a public key.)
//   - an ML-KEM-768 encapsulation to the recipient (FIPS 203), which a
//     quantum computer can't undo.
//
// Both go through HKDF together with what the box is for (the context),
// the recipient's ML-KEM key and the KEM ciphertext, so neither half can be
// swapped or replayed into another box. Breaking the box takes breaking
// X25519 and ML-KEM; a flaw in either one alone isn't enough.
//
// Only the Go standard library is used (crypto/mlkem, crypto/hkdf: Go 1.24
// and later), so nothing depends on a draft wire format.
//
// Not covered here: signatures are still Ed25519. Forging one needs a
// quantum computer at the time — nothing recorded now helps with that — so
// it's the next step (ML-DSA), not this one.

import (
	"bytes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	// pqSuite names the algorithms; it's in everything signed or derived
	// below, so a future suite can't be mistaken for this one.
	pqSuite = "x25519+mlkem768+hkdf-sha256+chacha20poly1305"
	// pqBoxPrefix marks a hybrid channel-key box (textcrypt.go). A box
	// without it is the older X25519-only kind, which is still opened (old
	// history) but never made any more.
	pqBoxPrefix = "pq1:"
	// pqVersion is the version of call-key messages (hello, key) that carry
	// the hybrid exchange. Older ones are ignored.
	pqVersion = 2
)

// pqKeyFromSeed derives the account's ML-KEM key from its identity seed, the
// same way its X25519 key is (encKeyFromSeed): the same account gets the
// same key on every device.
func pqKeyFromSeed(seed []byte) (*mlkem.DecapsulationKey768, error) {
	s, err := hkdf.Key(sha256.New, seed, nil, "chriscord-e2ee-v2/mlkem768", mlkem.SeedSize)
	if err != nil {
		return nil, err
	}
	defer clear(s)
	return mlkem.NewDecapsulationKey768(s)
}

// encBundleMessage is what the identity key signs to vouch for an account's
// encryption keys together: its X25519 key and its ML-KEM key. Signing them
// as one means a server can't pair someone's real X25519 key with an ML-KEM
// key of its own.
func encBundleMessage(x25519Pub, mlkemEK []byte) []byte {
	var b bytes.Buffer
	b.WriteString("chriscord-e2ee-v2/enc-bundle\x00" + pqSuite + "\x00")
	b.Write(x25519Pub)
	b.Write(mlkemEK)
	return b.Bytes()
}

// pqDerive makes the one-off cipher for a box from both shared secrets.
func pqDerive(dh, kemSS, recipientEK, kemCT, context []byte) (cipher.AEAD, error) {
	if len(dh) != 32 || len(kemSS) != mlkem.SharedKeySize {
		return nil, errors.New("pq: bad shared secret")
	}
	ikm := make([]byte, 0, len(dh)+len(kemSS))
	ikm = append(append(ikm, dh...), kemSS...)
	defer clear(ikm)
	ekH, ctH := sha256.Sum256(recipientEK), sha256.Sum256(kemCT)
	var info bytes.Buffer
	info.WriteString("chriscord-e2ee-v2/hybrid\x00" + pqSuite + "\x00")
	info.Write(ekH[:])
	info.Write(ctH[:])
	info.Write(context)
	k, err := hkdf.Key(sha256.New, ikm, nil, info.String(), chacha20poly1305.KeySize)
	if err != nil {
		return nil, err
	}
	defer clear(k)
	return chacha20poly1305.New(k)
}

// pqSeal seals plain for the holder of ek, given the X25519 secret already
// worked out between sender and recipient. The result is
// KEM ciphertext || nonce || sealed.
func pqSeal(dh []byte, ek *mlkem.EncapsulationKey768, context, plain []byte) ([]byte, error) {
	if ek == nil {
		return nil, errors.New("pq: no ML-KEM key")
	}
	ss, ct := ek.Encapsulate()
	defer clear(ss)
	aead, err := pqDerive(dh, ss, ek.Bytes(), ct, context)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(ct)+aead.NonceSize()+len(plain)+aead.Overhead())
	out = append(out, ct...)
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plain, nil), nil
}

// pqOpen opens what pqSeal made, with the recipient's ML-KEM key.
func pqOpen(dh []byte, dk *mlkem.DecapsulationKey768, context, box []byte) ([]byte, error) {
	if dk == nil {
		return nil, errors.New("pq: no ML-KEM key")
	}
	n := chacha20poly1305.NonceSize
	if len(box) < mlkem.CiphertextSize768+n+chacha20poly1305.Overhead {
		return nil, errors.New("pq: short box")
	}
	ct := box[:mlkem.CiphertextSize768]
	ss, err := dk.Decapsulate(ct)
	if err != nil {
		return nil, err
	}
	defer clear(ss)
	aead, err := pqDerive(dh, ss, dk.EncapsulationKey().Bytes(), ct, context)
	if err != nil {
		return nil, err
	}
	rest := box[mlkem.CiphertextSize768:]
	return aead.Open(nil, rest[:n], rest[n:], nil)
}

package main

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/mlkem"
	"crypto/rand"
	"testing"
)

func pqTestDH(t *testing.T) (a, b *ecdh.PrivateKey, shared []byte) {
	t.Helper()
	a, _ = ecdh.X25519().GenerateKey(rand.Reader)
	b, _ = ecdh.X25519().GenerateKey(rand.Reader)
	s1, err1 := a.ECDH(b.PublicKey())
	s2, err2 := b.ECDH(a.PublicKey())
	if err1 != nil || err2 != nil || !bytes.Equal(s1, s2) {
		t.Fatal("x25519 setup")
	}
	return a, b, s1
}

func TestPQSealOpen(t *testing.T) {
	_, _, dh := pqTestDH(t)
	dk, _ := mlkem.GenerateKey768()
	key := bytes.Repeat([]byte{7}, 32)
	ctx := []byte("board\x00gen 1")
	box, err := pqSeal(dh, dk.EncapsulationKey(), ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(box) != mlkem.CiphertextSize768+12+32+16 {
		t.Fatalf("box is %d bytes", len(box))
	}
	got, err := pqOpen(dh, dk, ctx, box)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("didn't open: %v", err)
	}
	// Two seals of the same key differ (fresh encapsulation and nonce).
	box2, _ := pqSeal(dh, dk.EncapsulationKey(), ctx, key)
	if bytes.Equal(box, box2) {
		t.Fatal("seal isn't randomised")
	}
}

// Each half has to be right: the X25519 secret (who sealed it), the ML-KEM
// key (who it's for), the context, and every byte of the box.
func TestPQOpenRejects(t *testing.T) {
	_, _, dh := pqTestDH(t)
	_, _, otherDH := pqTestDH(t)
	dk, _ := mlkem.GenerateKey768()
	otherDK, _ := mlkem.GenerateKey768()
	key := bytes.Repeat([]byte{9}, 32)
	box, _ := pqSeal(dh, dk.EncapsulationKey(), []byte("ctx"), key)

	if _, err := pqOpen(otherDH, dk, []byte("ctx"), box); err == nil {
		t.Error("opened with the wrong X25519 secret (a server forging a key)")
	}
	if _, err := pqOpen(dh, otherDK, []byte("ctx"), box); err == nil {
		t.Error("opened with the wrong ML-KEM key")
	}
	if _, err := pqOpen(dh, dk, []byte("ctx2"), box); err == nil {
		t.Error("opened with the wrong context")
	}
	for _, i := range []int{0, mlkem.CiphertextSize768 - 1, mlkem.CiphertextSize768, len(box) - 1} {
		bad := append([]byte(nil), box...)
		bad[i] ^= 1
		if _, err := pqOpen(dh, dk, []byte("ctx"), bad); err == nil {
			t.Errorf("opened with byte %d flipped", i)
		}
	}
	if _, err := pqOpen(dh, dk, []byte("ctx"), box[:100]); err == nil {
		t.Error("opened a short box")
	}
	if _, err := pqSeal(dh[:16], dk.EncapsulationKey(), nil, key); err == nil {
		t.Error("sealed with a short X25519 secret")
	}
	if _, err := pqSeal(dh, nil, nil, key); err == nil {
		t.Error("sealed with no ML-KEM key")
	}
}

// Same seed, same key — on every device, and after export/import.
func TestPQKeyFromSeed(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	a, err1 := pqKeyFromSeed(priv.Seed())
	b, err2 := pqKeyFromSeed(append([]byte(nil), priv.Seed()...))
	if err1 != nil || err2 != nil || !bytes.Equal(a.EncapsulationKey().Bytes(), b.EncapsulationKey().Bytes()) {
		t.Fatal("not deterministic")
	}
	_, priv2, _ := ed25519.GenerateKey(rand.Reader)
	c, _ := pqKeyFromSeed(priv2.Seed())
	if bytes.Equal(a.EncapsulationKey().Bytes(), c.EncapsulationKey().Bytes()) {
		t.Fatal("different accounts, same key")
	}
}

func TestEncBundleMessageBindsBoth(t *testing.T) {
	x := bytes.Repeat([]byte{1}, 32)
	ek := bytes.Repeat([]byte{2}, mlkem.EncapsulationKeySize768)
	m := encBundleMessage(x, ek)
	if bytes.Equal(m, encBundleMessage(bytes.Repeat([]byte{3}, 32), ek)) ||
		bytes.Equal(m, encBundleMessage(x, bytes.Repeat([]byte{4}, mlkem.EncapsulationKeySize768))) {
		t.Fatal("bundle message doesn't cover both keys")
	}
	if bytes.Equal(m[:28], encKeyMessage(x)[:28]) {
		t.Fatal("v2 bundle shares the v1 signing context")
	}
}

// The server checks this signature too (channel_keys.rs ENC_BUNDLE_CONTEXT):
// changing the bytes here means changing them there.
func TestEncBundleMessageMatchesServer(t *testing.T) {
	const server = "chriscord-e2ee-v2/enc-bundle\x00x25519+mlkem768+hkdf-sha256+chacha20poly1305\x00"
	if m := encBundleMessage(nil, nil); string(m) != server {
		t.Fatalf("bundle context %q doesn't match the server's", m)
	}
}

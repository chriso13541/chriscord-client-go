package main

// Pins cloudproto.go: the same test runs in chriscord-client-go and
// chriscord-accounts, so if the two copies ever drift apart, one fails.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
)

func TestCloudProtoVectors(t *testing.T) {
	k := CloudKDF{Alg: "argon2id", Salt: "000102030405060708090a0b0c0d0e0f", Time: 1, MemKiB: 19 * 1024, Threads: 1}
	auth, wrap, err := k.PasswordKeys("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	check := func(name string, got []byte, want string) {
		t.Helper()
		if hex.EncodeToString(got) != want {
			t.Errorf("%s = %x, want %s", name, got, want)
		}
	}
	check("authKey", auth, "31a46107521101f212a7ce441e69c81651f5184ab977e1e68725e0b476d4c379")
	check("wrapKey", wrap, "4a2a688c1af88200a4d14529afaa4f180b04dd1d34edde755c4b95ba06033025")
	ra, rw := RecoveryKeys([]byte("0123456789abcdefghij"))
	check("recoveryAuth", ra, "fdddd37ccddb9320fab71cba0b67445554775949503d1c16dce5936c6c19402b")
	check("recoveryWrap", rw, "2c87e28342db43b487e654e2e538243f987b8933159a261850adfeb6443990c7")
	check("identityWrap", IdentityWrapKey(make([]byte, 32)), "273e099edcaef52a3ae4e3d5687f32711e86ab2a64b038c117f738d57241e1ed")
	if got := string(CloudSigMessage("claim", "n", "k", "u")); got != "chriscord-cloud-v1/claim\x00n\x00k\x00u" {
		t.Errorf("CloudSigMessage = %q", got)
	}
	if got := k.String(); got != "argon2id:000102030405060708090a0b0c0d0e0f:1:19456:1" {
		t.Errorf("KDF.String = %q", got)
	}
}

func TestCloudProtoBindings(t *testing.T) {
	key, wrapKey := make([]byte, 32), make([]byte, 32)
	rand.Read(key)
	rand.Read(wrapKey)
	pub := strings.Repeat("ab", 32)
	w, err := WrapAccountKey(wrapKey, key, pub, WrapPassword)
	if err != nil || !ValidWrapped(w) {
		t.Fatal(err)
	}
	if got, err := UnwrapAccountKey(wrapKey, w, strings.ToUpper(pub), WrapPassword); err != nil || !bytes.Equal(got, key) {
		t.Fatal("round trip failed", err)
	}
	if _, err := UnwrapAccountKey(wrapKey, w, strings.Repeat("cd", 32), WrapPassword); err == nil {
		t.Fatal("a wrap opened for another account")
	}
	if _, err := UnwrapAccountKey(wrapKey, w, pub, WrapRecovery); err == nil {
		t.Fatal("a password wrap opened as a recovery wrap")
	}
	blob, _ := SealVault(key, pub, 7, []byte("vault"))
	if p, err := OpenVault(key, pub, 7, blob); err != nil || string(p) != "vault" {
		t.Fatal("vault round trip failed", err)
	}
	if _, err := OpenVault(key, pub, 6, blob); err == nil {
		t.Fatal("a vault opened as another version")
	}

	pk, sk, _ := ed25519.GenerateKey(rand.Reader)
	msg := CloudSigMessage("login", "nonce", hex.EncodeToString(pk))
	sig := hex.EncodeToString(ed25519.Sign(sk, msg))
	if !VerifyCloudSig(hex.EncodeToString(pk), sig, msg) || VerifyCloudSig(hex.EncodeToString(pk), sig, CloudSigMessage("delete", "nonce", hex.EncodeToString(pk))) {
		t.Fatal("signature checks")
	}

	raw, shown, _ := NewRecoveryKey()
	if len(shown) != 39 || strings.Count(shown, "-") != 7 {
		t.Fatalf("recovery key looks like %q", shown)
	}
	if got, err := ParseRecoveryKey(" " + strings.ToLower(strings.ReplaceAll(shown, "-", "  ")) + " "); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("recovery key didn't parse back", err)
	}
	if _, err := ParseRecoveryKey("ABCD-EFGH"); err == nil {
		t.Fatal("short recovery key accepted")
	}

	for name, ok := range map[string]bool{"chris": true, "Chris.O": true, "a_b-c": true, "x": false, "-x": false, "chris o": false, "émile": false, strings.Repeat("a", 33): false} {
		if (ValidCloudUsername(name) == nil) != ok {
			t.Errorf("ValidCloudUsername(%q) should be %v", name, ok)
		}
	}
	if e, err := NormalizeEmail("  Chris@Example.COM "); err != nil || e != "chris@example.com" {
		t.Errorf("NormalizeEmail = %q %v", e, err)
	}
	for _, bad := range []string{"nope", "a@b", "a b@c.d", "a@b.c\r\nBcc: x@y.z"} {
		if _, err := NormalizeEmail(bad); err == nil {
			t.Errorf("NormalizeEmail(%q) accepted", bad)
		}
	}
	if err := (CloudKDF{Alg: "argon2id", Salt: "00", Time: 1, MemKiB: 64, Threads: 1}).Validate(); err == nil {
		t.Error("a cheap KDF was accepted")
	}
}

func TestLogProto(t *testing.T) {
	e := LogEntry{Seq: 1, PublicKey: strings.Repeat("ab", 32), Event: "register", Username: "chris", Nonce: "n", Signature: "s", At: 100, Prev: LogGenesis}
	if got := LogEntryHash(e); got != "0fc1a46227936ea0f6a67a880ceb449606e0537a24ed1c03eb10591af7c198c1" {
		t.Errorf("LogEntryHash = %s", got)
	}
	if got := string(LogHeadMessage(5, "ABCD", 7)); got != "chriscord-cloud-v1/log-head\x00\x005\x00abcd\x007" {
		t.Errorf("LogHeadMessage = %q", got)
	}
	pk, sk, _ := ed25519.GenerateKey(rand.Reader)
	h := LogHead{Seq: 5, Hash: "abcd", Time: 7, Key: hex.EncodeToString(pk)}
	h.Signature = hex.EncodeToString(ed25519.Sign(sk, LogHeadMessage(h.Seq, h.Hash, h.Time)))
	if !h.Valid() {
		t.Error("a signed head didn't verify")
	}
	h.Seq = 6
	if h.Valid() {
		t.Error("a changed head verified")
	}
	// Entries: the key's own signature, or none for "expired".
	e2 := LogEntry{PublicKey: hex.EncodeToString(pk), Event: "register", Username: "chris", Nonce: "n"}
	e2.Signature = hex.EncodeToString(ed25519.Sign(sk, CloudSigMessage("claim", "n", e2.PublicKey, "chris")))
	if !LogEntrySigned(e2) {
		t.Error("a claim didn't verify")
	}
	e2.Username = "someone"
	if LogEntrySigned(e2) {
		t.Error("a claim for another name verified")
	}
	if !LogEntrySigned(LogEntry{Event: "expired"}) || LogEntrySigned(LogEntry{Event: "expired", Signature: "x"}) || LogEntrySigned(LogEntry{Event: "made-up"}) {
		t.Error("expired / unknown events")
	}
}

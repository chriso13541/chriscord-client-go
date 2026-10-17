package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"
)

// The same vectors are pinned in the bot SDK's botproto_test.go and the
// server's bots.rs tests.
func TestBotSigMessageLayout(t *testing.T) {
	got := BotSigMessage("cert", "a", "Music", "", "b", "1700000000")
	want := "chriscord-bot-v1/cert\x00a\x00Music\x00\x00b\x001700000000"
	if string(got) != want {
		t.Fatalf("got %q", got)
	}
}

func fixedKey(b byte) ed25519.PrivateKey {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = b
	}
	return ed25519.NewKeyFromSeed(seed)
}

// A certificate made from fixed keys signs to a fixed value (Ed25519 is
// deterministic), so the app, the SDK and the server agree byte for byte.
func TestBotCertVector(t *testing.T) {
	maker, bot := fixedKey(7), fixedKey(9)
	c, err := NewBotCert(maker, bot.Public().(ed25519.PublicKey), "Music", "Plays music", 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	if c.BotKey != hex.EncodeToString(bot.Public().(ed25519.PublicKey)) || c.MakerKey != hex.EncodeToString(maker.Public().(ed25519.PublicKey)) {
		t.Fatal("keys")
	}
	const (
		wantBot   = "fd1724385aa0c75b64fb78cd602fa1d991fdebf76b13c58ed702eac835e9f618"
		wantMaker = "ea4a6c63e29c520abef5507b132ec5f9954776aebebe7b92421eea691446d22c"
		wantSig   = "7c0888bb7cb5cb6c53f1502e44e2fa811920e49772c864473ca007333662e1323933dfed914ebbee982570096d972c9a37e751ba84346e6664cf6728dc30150e"
	)
	if c.BotKey != wantBot || c.MakerKey != wantMaker || c.Sig != wantSig {
		t.Fatalf("bot_key %s maker_key %s sig %s", c.BotKey, c.MakerKey, c.Sig)
	}
}

func TestBotCertChecks(t *testing.T) {
	maker, bot := fixedKey(1), fixedKey(2)
	c, err := NewBotCert(maker, bot.Public().(ed25519.PublicKey), "DJ", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	tamper := []func(c *BotCert){
		func(c *BotCert) { c.Name = "DK" },
		func(c *BotCert) { c.Description = "x" },
		func(c *BotCert) { c.Created++ },
		func(c *BotCert) { c.BotKey = c.MakerKey },
		func(c *BotCert) { c.MakerKey = hex.EncodeToString(fixedKey(3).Public().(ed25519.PublicKey)) },
		func(c *BotCert) { c.V = 2 },
		func(c *BotCert) { c.BotKey = strings.ToUpper(c.BotKey) },
	}
	for i, f := range tamper {
		cp := *c
		f(&cp)
		if cp.Check() == nil {
			t.Errorf("tamper %d accepted", i)
		}
	}
	for _, bad := range []string{"x", " padded", "a\x00b", strings.Repeat("n", 33), "tab\tname"} {
		if _, err := NewBotCert(maker, bot.Public().(ed25519.PublicKey), bad, "", 1); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	if _, err := NewBotCert(maker, bot.Public().(ed25519.PublicKey), "Okay", strings.Repeat("d", 191), 1); err == nil {
		t.Error("long description accepted")
	}
	if _, err := NewBotCert(maker, bot.Public().(ed25519.PublicKey), "Émoji 🎶", strings.Repeat("d", 190), 1); err != nil {
		t.Errorf("good cert refused: %v", err)
	}
}

func TestBotToken(t *testing.T) {
	maker, bot := fixedKey(1), fixedKey(2)
	c, _ := NewBotCert(maker, bot.Public().(ed25519.PublicKey), "Music", "d", 5)
	tok, err := EncodeBotToken(bot, c)
	if err != nil || !strings.HasPrefix(tok, "ccbot_1_") {
		t.Fatal(tok, err)
	}
	k, c2, err := ParseBotToken("  " + tok + "\n")
	if err != nil || !k.Equal(bot) || *c2 != *c {
		t.Fatalf("round trip: %v", err)
	}
	// Another key with this certificate.
	other, _ := EncodeBotToken(fixedKey(4), c)
	if _, _, err := ParseBotToken(other); err == nil {
		t.Error("mismatched key accepted")
	}
	for _, bad := range []string{"", "ccbot_1_", "ccbot_1_!!!", "nope_" + tok[8:], tok[:len(tok)-3]} {
		if _, _, err := ParseBotToken(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestBotAddLinkAndRevocation(t *testing.T) {
	maker, bot := fixedKey(1), fixedKey(2)
	c, _ := NewBotCert(maker, bot.Public().(ed25519.PublicKey), "Music", "d", 5)
	link := BotAddLink(c, 0x31005)
	if !strings.HasPrefix(link, "https://chriscord.app/addbot#") {
		t.Fatal(link)
	}
	for _, in := range []string{link, link[strings.Index(link, "#")+1:]} {
		r, err := ParseBotAddLink(in)
		if err != nil || r.Cert != *c || r.Permissions != 0x31005 {
			t.Fatalf("add link: %v", err)
		}
	}
	if _, err := ParseBotAddLink("https://chriscord.app/addbot#bm9wZQ"); err == nil {
		t.Error("junk link accepted")
	}
	r := NewBotRevocation(maker, c.BotKey, 99)
	if !r.Valid() {
		t.Fatal("revocation invalid")
	}
	r.At++
	if r.Valid() {
		t.Fatal("tampered revocation valid")
	}
}

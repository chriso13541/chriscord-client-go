package main

// botproto.go — what makes a bot a bot: its certificate, its token, its
// "Add to server" link and its revocation. The same file (bar the package
// line and this sentence) is in the bot SDK (chriscord-bot-sdk), and the
// server checks the same signatures (bots.rs). botproto_test.go pins the
// formats in both.
//
//   certificate  {v, bot_key, name, description, maker_key, created, sig}:
//                the maker's key signs BotSigMessage("cert", bot_key, name,
//                description, maker_key, created)
//   token        "ccbot_1_" + base64url(bot key seed (32 bytes) || certificate JSON)
//                — everything the bot needs, and the only secret. Never sent anywhere.
//   add link     https://chriscord.app/addbot#<base64url(JSON {cert, permissions})>
//                — public: the certificate and what the bot asks for
//   revocation   {bot_key, maker_key, at, sig}: the maker's key signs
//                BotSigMessage("revoke", bot_key, maker_key, at)

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	botProto       = "chriscord-bot-v1"
	BotTokenPrefix = "ccbot_1_"
	BotAddLinkBase = "https://chriscord.app/addbot#"
	BotNameMin     = 2
	BotNameMax     = 32
	BotDescMax     = 190
)

// BotSigMessage is what a maker's key signs: "chriscord-bot-v1/<action>",
// then each field after a zero byte. Fields never contain a zero byte
// (checked), so a message can only be read one way.
func BotSigMessage(action string, fields ...string) []byte {
	var b strings.Builder
	b.WriteString(botProto + "/" + action)
	for _, f := range fields {
		b.WriteByte(0)
		b.WriteString(f)
	}
	return []byte(b.String())
}

// BotCert is a bot's certificate: "this key is my bot", signed by its maker.
type BotCert struct {
	V           int    `json:"v"`
	BotKey      string `json:"bot_key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MakerKey    string `json:"maker_key"`
	Created     int64  `json:"created"`
	Sig         string `json:"sig"`
}

func (c *BotCert) message() []byte {
	return BotSigMessage("cert", c.BotKey, c.Name, c.Description, c.MakerKey, strconv.FormatInt(c.Created, 10))
}

func isHexKey(k string) bool {
	if len(k) != 64 {
		return false
	}
	for _, r := range k {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// CheckBotName says what's wrong with a bot name ("" if nothing).
func CheckBotName(name string) string {
	n := utf8.RuneCountInString(name)
	if strings.TrimSpace(name) != name || n < BotNameMin || n > BotNameMax {
		return fmt.Sprintf("A bot's name is %d–%d characters, with no spaces at either end", BotNameMin, BotNameMax)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "A bot's name can't contain control characters"
		}
	}
	return ""
}

// CheckBotDescription says what's wrong with a description ("" if nothing).
func CheckBotDescription(d string) string {
	if utf8.RuneCountInString(d) > BotDescMax || strings.ContainsRune(d, 0) {
		return fmt.Sprintf("A bot's description is up to %d characters", BotDescMax)
	}
	return ""
}

// Check verifies everything about a certificate, including its signature.
func (c *BotCert) Check() error {
	switch {
	case c.V != 1:
		return errors.New("this bot certificate is from a newer version of chriscord")
	case !isHexKey(c.BotKey) || !isHexKey(c.MakerKey):
		return errors.New("the bot certificate has a malformed key")
	case c.BotKey == c.MakerKey:
		return errors.New("a bot needs its own key, not its maker's")
	}
	if msg := CheckBotName(c.Name); msg != "" {
		return errors.New(msg)
	}
	if msg := CheckBotDescription(c.Description); msg != "" {
		return errors.New(msg)
	}
	maker, _ := hex.DecodeString(c.MakerKey)
	sig, err := hex.DecodeString(c.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(maker, c.message(), sig) {
		return errors.New("the bot certificate isn't signed by its maker")
	}
	return nil
}

// NewBotCert makes and signs a certificate for botPub with the maker's key.
func NewBotCert(maker ed25519.PrivateKey, botPub ed25519.PublicKey, name, description string, created int64) (*BotCert, error) {
	c := &BotCert{V: 1, BotKey: hex.EncodeToString(botPub), Name: name, Description: description,
		MakerKey: hex.EncodeToString(maker.Public().(ed25519.PublicKey)), Created: created}
	if msg := CheckBotName(name); msg != "" {
		return nil, errors.New(msg)
	}
	if msg := CheckBotDescription(description); msg != "" {
		return nil, errors.New(msg)
	}
	c.Sig = hex.EncodeToString(ed25519.Sign(maker, c.message()))
	return c, c.Check()
}

// EncodeBotToken makes a bot's token from its key and certificate.
func EncodeBotToken(bot ed25519.PrivateKey, cert *BotCert) (string, error) {
	j, err := json.Marshal(cert)
	if err != nil {
		return "", err
	}
	return BotTokenPrefix + base64.RawURLEncoding.EncodeToString(append(append([]byte{}, bot.Seed()...), j...)), nil
}

// ParseBotToken reads a token back into the bot's key and certificate, and
// checks they belong together.
func ParseBotToken(token string) (ed25519.PrivateKey, *BotCert, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(token), BotTokenPrefix)
	if !ok {
		return nil, nil, errors.New("not a chriscord bot token (they start with " + BotTokenPrefix + ")")
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil || len(raw) <= ed25519.SeedSize {
		return nil, nil, errors.New("the bot token is damaged")
	}
	key := ed25519.NewKeyFromSeed(raw[:ed25519.SeedSize])
	var cert BotCert
	if err := json.Unmarshal(raw[ed25519.SeedSize:], &cert); err != nil {
		return nil, nil, errors.New("the bot token is damaged")
	}
	if err := cert.Check(); err != nil {
		return nil, nil, err
	}
	if cert.BotKey != hex.EncodeToString(key.Public().(ed25519.PublicKey)) {
		return nil, nil, errors.New("the bot token's key doesn't match its certificate")
	}
	return key, &cert, nil
}

// BotAddRequest is what an "Add to server" link carries.
type BotAddRequest struct {
	Cert        BotCert `json:"cert"`
	Permissions uint64  `json:"permissions"`
}

// BotAddLink makes a bot's "Add to server" link.
func BotAddLink(cert *BotCert, permissions uint64) string {
	j, _ := json.Marshal(BotAddRequest{Cert: *cert, Permissions: permissions})
	return BotAddLinkBase + base64.RawURLEncoding.EncodeToString(j)
}

// ParseBotAddLink reads an "Add to server" link (or just the part after #).
func ParseBotAddLink(link string) (*BotAddRequest, error) {
	link = strings.TrimSpace(link)
	if i := strings.Index(link, "#"); i >= 0 {
		link = link[i+1:]
	}
	raw, err := base64.RawURLEncoding.DecodeString(link)
	if err != nil {
		return nil, errors.New("that isn't a bot's Add to server link")
	}
	var r BotAddRequest
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, errors.New("that isn't a bot's Add to server link")
	}
	if err := r.Cert.Check(); err != nil {
		return nil, err
	}
	return &r, nil
}

// BotRevocation is a maker retiring a bot.
type BotRevocation struct {
	BotKey   string `json:"bot_key"`
	MakerKey string `json:"maker_key"`
	At       int64  `json:"at"`
	Sig      string `json:"sig"`
}

// NewBotRevocation signs a revocation of botKey (hex) with the maker's key.
func NewBotRevocation(maker ed25519.PrivateKey, botKey string, at int64) BotRevocation {
	r := BotRevocation{BotKey: strings.ToLower(botKey), MakerKey: hex.EncodeToString(maker.Public().(ed25519.PublicKey)), At: at}
	r.Sig = hex.EncodeToString(ed25519.Sign(maker, BotSigMessage("revoke", r.BotKey, r.MakerKey, strconv.FormatInt(at, 10))))
	return r
}

// Valid checks a revocation's signature.
func (r BotRevocation) Valid() bool {
	maker, err1 := hex.DecodeString(r.MakerKey)
	sig, err2 := hex.DecodeString(r.Sig)
	return err1 == nil && err2 == nil && isHexKey(r.BotKey) && isHexKey(r.MakerKey) && len(sig) == ed25519.SignatureSize &&
		ed25519.Verify(maker, BotSigMessage("revoke", r.BotKey, r.MakerKey, strconv.FormatInt(r.At, 10)), sig)
}

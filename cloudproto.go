package main

// cloudproto.go — the chriscord.app account protocol: every key derivation,
// key wrap, vault seal and signed message, shared by the app and the
// accounts service. This file is byte-for-byte the same in
// chriscord-client-go and chriscord-accounts: change it in both or neither
// (cloudproto_test.go pins the outputs, so a drift fails the tests).
//
// Keys, and who ever sees them:
//
//   password ──Argon2id(salt)──▶ master        (app only)
//     master ──HKDF "auth"──────▶ authKey       sent at login; the service
//                                               stores only Argon2id(authKey)
//     master ──HKDF "wrap"──────▶ passwordWrap  (app only)
//
//   recovery key (20 random bytes, shown once as XXXX-XXXX-…)
//            ──HKDF "recovery-auth"──▶ recoveryAuth   (stored hashed)
//            ──HKDF "recovery-wrap"──▶ recoveryWrap   (app only)
//
//   identity seed (the Ed25519 account key)
//            ──HKDF "identity-wrap"──▶ identityWrap   (app only)
//
//   accountKey (32 random bytes) is stored three times on the service,
//   wrapped (AES-256-GCM) by passwordWrap, recoveryWrap and identityWrap.
//   It encrypts the vault: the identity seed, profile, pictures, settings
//   and saved servers. The service never holds anything that opens it.
//
// Every wrap and vault is bound (as GCM additional data) to the account's
// public key, which never changes — so a wrapped key or vault can't be
// moved to another account, and renaming doesn't touch any of it. The
// vault is also bound to its version number, so the service can't pass an
// old copy off as a new one.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/hkdf"
)

const cloudProto = "chriscord-cloud-v1"

// Default Argon2id cost for the password: the same "feel it for a second"
// cost as the local passphrase (account.go). Stored per account, so it can
// be raised later without breaking anyone.
const (
	cloudKDFTime    = 3
	cloudKDFMemKiB  = 64 * 1024
	cloudKDFThreads = 4
)

// Limits on what a stored KDF may ask for: a service can't make the app
// spend gigabytes, and nobody can register a uselessly cheap one.
const (
	cloudKDFMinMemKiB = 19 * 1024
	cloudKDFMaxMemKiB = 1024 * 1024
	cloudKDFMaxTime   = 10
)

const cloudMinPassword = 10

// CloudKDF is how a password is stretched. The salt is random per account
// (and per password change); the service hands it out at prelogin.
type CloudKDF struct {
	Alg     string `json:"alg"`
	Salt    string `json:"salt"` // hex, 16 bytes
	Time    uint32 `json:"time"`
	MemKiB  uint32 `json:"mem_kib"`
	Threads uint8  `json:"threads"`
}

func NewCloudKDF() (CloudKDF, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return CloudKDF{}, err
	}
	return CloudKDF{Alg: "argon2id", Salt: hex.EncodeToString(salt), Time: cloudKDFTime, MemKiB: cloudKDFMemKiB, Threads: cloudKDFThreads}, nil
}

func (k CloudKDF) Validate() error {
	salt, err := hex.DecodeString(k.Salt)
	switch {
	case k.Alg != "argon2id":
		return errors.New("unsupported password hashing")
	case err != nil || len(salt) != 16:
		return errors.New("bad salt")
	case k.Time < 1 || k.Time > cloudKDFMaxTime:
		return errors.New("bad time cost")
	case k.MemKiB < cloudKDFMinMemKiB || k.MemKiB > cloudKDFMaxMemKiB:
		return errors.New("bad memory cost")
	case k.Threads < 1 || k.Threads > 16:
		return errors.New("bad thread count")
	}
	return nil
}

// String is the KDF as signed fields (see CloudSigMessage).
func (k CloudKDF) String() string {
	return fmt.Sprintf("%s:%s:%d:%d:%d", k.Alg, strings.ToLower(k.Salt), k.Time, k.MemKiB, k.Threads)
}

func cloudHKDF(secret []byte, label string) []byte {
	out := make([]byte, 32)
	io.ReadFull(hkdf.New(sha256.New, secret, nil, []byte(cloudProto+"/"+label)), out)
	return out
}

// PasswordKeys stretches password into the login key (authKey, sent) and
// the key that wraps accountKey (never sent).
func (k CloudKDF) PasswordKeys(password string) (authKey, wrapKey []byte, err error) {
	if err := k.Validate(); err != nil {
		return nil, nil, err
	}
	salt, _ := hex.DecodeString(k.Salt)
	master := argon2.IDKey([]byte(password), salt, k.Time, k.MemKiB, k.Threads, 32)
	authKey, wrapKey = cloudHKDF(master, "auth"), cloudHKDF(master, "wrap")
	for i := range master {
		master[i] = 0
	}
	return authKey, wrapKey, nil
}

// RecoveryKeys: a recovery key is already 160 random bits, so it needs no
// stretching — just the same auth/wrap split as a password.
func RecoveryKeys(recovery []byte) (authKey, wrapKey []byte) {
	return cloudHKDF(recovery, "recovery-auth"), cloudHKDF(recovery, "recovery-wrap")
}

// IdentityWrapKey lets any device holding the account's key open
// accountKey (and so sync) without the password.
func IdentityWrapKey(seed []byte) []byte { return cloudHKDF(seed, "identity-wrap") }

// Kinds of accountKey wrap (part of their additional data).
const (
	WrapPassword = "password"
	WrapRecovery = "recovery"
	WrapIdentity = "identity"
)

func cloudAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func wrapAAD(pubHex, kind string) []byte {
	return []byte(cloudProto + "/wrap\x00" + kind + "\x00" + strings.ToLower(pubHex))
}

// WrapAccountKey seals accountKey under wrapKey: hex(nonce || ciphertext).
func WrapAccountKey(wrapKey, accountKey []byte, pubHex, kind string) (string, error) {
	gcm, err := cloudAEAD(wrapKey)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return hex.EncodeToString(gcm.Seal(nonce, nonce, accountKey, wrapAAD(pubHex, kind))), nil
}

func UnwrapAccountKey(wrapKey []byte, wrapped, pubHex, kind string) ([]byte, error) {
	gcm, err := cloudAEAD(wrapKey)
	if err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(wrapped)
	if err != nil || len(raw) < gcm.NonceSize()+gcm.Overhead() {
		return nil, errors.New("wrapped key is corrupt")
	}
	key, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], wrapAAD(pubHex, kind))
	if err != nil || len(key) != 32 {
		return nil, errors.New("couldn't open the account key")
	}
	return key, nil
}

// ValidWrapped checks a wrapped key's shape (the service can't open it).
func ValidWrapped(wrapped string) bool {
	raw, err := hex.DecodeString(wrapped)
	return err == nil && len(raw) == 12+32+16
}

var vaultMagic = []byte("CCV1")

func vaultAAD(pubHex string, version int64) []byte {
	return []byte(cloudProto + "/vault\x00" + strings.ToLower(pubHex) + "\x00" + strconv.FormatInt(version, 10))
}

// SealVault encrypts the vault for the given version: "CCV1" || nonce || ct.
func SealVault(accountKey []byte, pubHex string, version int64, plain []byte) ([]byte, error) {
	gcm, err := cloudAEAD(accountKey)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append(append([]byte{}, vaultMagic...), nonce...)
	return gcm.Seal(out, nonce, plain, vaultAAD(pubHex, version)), nil
}

func OpenVault(accountKey []byte, pubHex string, version int64, blob []byte) ([]byte, error) {
	gcm, err := cloudAEAD(accountKey)
	if err != nil {
		return nil, err
	}
	if !ValidVaultBlob(blob) {
		return nil, errors.New("not a Chriscord vault")
	}
	rest := blob[len(vaultMagic):]
	plain, err := gcm.Open(nil, rest[:gcm.NonceSize()], rest[gcm.NonceSize():], vaultAAD(pubHex, version))
	if err != nil {
		return nil, errors.New("the synced copy doesn't open (wrong key, or it was tampered with)")
	}
	return plain, nil
}

// ValidVaultBlob checks a vault's shape (the service can't open it).
func ValidVaultBlob(blob []byte) bool {
	return len(blob) >= len(vaultMagic)+12+16 && string(blob[:len(vaultMagic)]) == string(vaultMagic)
}

// CloudSigMessage is what the account key signs to approve an action:
// the action and a one-time server nonce, then the action's fields, all
// NUL-separated (no field may contain NUL; usernames, emails and hex
// can't).
func CloudSigMessage(action, nonce string, fields ...string) []byte {
	var b strings.Builder
	b.WriteString(cloudProto + "/" + action)
	b.WriteByte(0)
	b.WriteString(nonce)
	for _, f := range fields {
		b.WriteByte(0)
		b.WriteString(f)
	}
	return []byte(b.String())
}

// VerifyCloudSig checks sigHex against pubHex for the given message.
func VerifyCloudSig(pubHex, sigHex string, msg []byte) bool {
	pub, err1 := hex.DecodeString(pubHex)
	sig, err2 := hex.DecodeString(sigHex)
	return err1 == nil && err2 == nil && len(pub) == ed25519.PublicKeySize && len(sig) == ed25519.SignatureSize &&
		ed25519.Verify(pub, msg, sig)
}

// ── Recovery keys ──────────────────────────────────────────────────────────

var recoveryEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewRecoveryKey makes a recovery key: 20 random bytes, shown as eight
// groups of four (A–Z, 2–7).
func NewRecoveryKey() ([]byte, string, error) {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", err
	}
	s := recoveryEnc.EncodeToString(raw)
	groups := make([]string, 0, 8)
	for i := 0; i < len(s); i += 4 {
		groups = append(groups, s[i:i+4])
	}
	return raw, strings.Join(groups, "-"), nil
}

// ParseRecoveryKey reads a typed recovery key: any case, spaces or dashes
// anywhere, and the usual look-alikes (0/O, 1/I, 8/B) forgiven.
func ParseRecoveryKey(s string) ([]byte, error) {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch {
		case r == '-' || unicode.IsSpace(r):
		case r == '0':
			b.WriteRune('O')
		case r == '1':
			b.WriteRune('I')
		case r == '8':
			b.WriteRune('B')
		default:
			b.WriteRune(r)
		}
	}
	raw, err := recoveryEnc.DecodeString(b.String())
	if err != nil || len(raw) != 20 {
		return nil, errors.New("that doesn't look like a recovery key (it's 32 letters and numbers, like ABCD-EFGH-…)")
	}
	return raw, nil
}

// ── Usernames and emails ───────────────────────────────────────────────────

// ValidCloudUsername: 2–32 letters, digits, '_', '.', '-', starting with
// a letter or digit. Unique ignoring case (see CloudUsernameKey).
func ValidCloudUsername(name string) error {
	if n := len(name); n < 2 || n > 32 {
		return errors.New("usernames are 2–32 characters")
	}
	for i, r := range name {
		alnum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if i == 0 && !alnum {
			return errors.New("usernames start with a letter or number")
		}
		if !alnum && r != '_' && r != '.' && r != '-' {
			return errors.New("usernames can use letters, numbers, _ . and -")
		}
	}
	return nil
}

func CloudUsernameKey(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// NormalizeEmail trims and lowercases an address and checks it's plausible
// (the verification link is the real check).
func NormalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndexByte(email, '@')
	if len(email) > 254 || at < 1 || at == len(email)-1 || !strings.Contains(email[at:], ".") {
		return "", errors.New("that email address doesn't look right")
	}
	for _, r := range email {
		if r <= ' ' || r == 0x7f || r == '<' || r == '>' || r == ',' || r == ';' || r == '"' {
			return "", errors.New("that email address doesn't look right")
		}
	}
	return email, nil
}

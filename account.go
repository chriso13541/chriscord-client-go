package main

import (
	"archive/zip"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// ── On-disk formats ─────────────────────────────────────────────────────────
//
// Each account lives in its own directory under accountsDir(), named after
// its fingerprint (so it's collision-proof by construction and needs no
// sanitizing):
//
//   accounts/<fingerprint>/identity.json  — public key (plaintext) + the
//                                            private key seed, encrypted
//                                            under a passphrase
//   accounts/<fingerprint>/account.json   — username (plaintext, not secret)
//   accounts/<fingerprint>/pfp.png        — optional, may not exist
//
// identity.json and account.json are copied byte-for-byte into and out of
// the exported archive — no re-encoding of those files themselves. The
// archive as a whole gets one more layer of encryption on top for export,
// covering everything inside it (including a bundled server-list snapshot
// — see "Outer container encryption" below); that's the part that's new.

// Argon2id parameters for deriving the AES-256 key from a passphrase.
// ~64MB/3 passes/4 lanes is a deliberately "feel it for a second" cost —
// this only runs once per app launch, same tradeoff as unlocking a LUKS
// volume at boot.
const (
	kdfTime    = 3
	kdfMemKiB  = 64 * 1024
	kdfThreads = 4
	kdfKeyLen  = 32 // AES-256
)

type encryptedIdentity struct {
	PublicKey  string    `json:"public_key"` // hex, plaintext — not sensitive
	KDF        string    `json:"kdf"`
	KDFSalt    string    `json:"kdf_salt"`
	KDFTime    uint32    `json:"kdf_time"`
	KDFMemKiB  uint32    `json:"kdf_memory_kib"`
	KDFThreads uint8     `json:"kdf_threads"`
	Cipher     string    `json:"cipher"`
	Nonce      string    `json:"nonce"`
	Ciphertext string    `json:"ciphertext"` // encrypted 32-byte Ed25519 seed + GCM tag
	CreatedAt  time.Time `json:"created_at"`
}

type accountMeta struct {
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
}

// Account is the unlocked, in-memory identity. Never marshaled to JSON and
// never handed to the frontend directly — see AccountView for that.
type Account struct {
	Slug       string // == fingerprint, also the directory name
	Username   string
	PublicKey  ed25519.PublicKey
	PrivateKey ed25519.PrivateKey
	HasAvatar  bool
	AvatarPath string
}

func (a *Account) View() *AccountView {
	if a == nil {
		return nil
	}
	return &AccountView{
		Username:    a.Username,
		Fingerprint: fingerprintOf(a.PublicKey),
		HasAvatar:   a.HasAvatar,
	}
}

// ── Paths ────────────────────────────────────────────────────────────────

func chriscordConfigDir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		base = "." // extremely unlikely fallback; keeps things working regardless
	}
	return filepath.Join(base, "chriscord")
}

func accountsDir() string { return filepath.Join(chriscordConfigDir(), "accounts") }
func activeAccountFile() string { return filepath.Join(chriscordConfigDir(), "active_account") }

func fingerprintOf(pub []byte) string {
	h := hex.EncodeToString(pub)
	if len(h) > 16 {
		return h[:16]
	}
	return h
}

// ── Active-account pointer ──────────────────────────────────────────────────

func setActiveSlug(slug string) error {
	if err := os.MkdirAll(chriscordConfigDir(), 0700); err != nil {
		return err
	}
	return os.WriteFile(activeAccountFile(), []byte(slug), 0600)
}

func getActiveSlug() (string, error) {
	data, err := os.ReadFile(activeAccountFile())
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// HasAnyAccount reports whether an account is set up and ready to unlock —
// used at startup to decide between the onboarding flow and the passphrase
// prompt.
func HasAnyAccount() bool {
	slug, err := getActiveSlug()
	if err != nil || slug == "" {
		return false
	}
	_, err = os.Stat(filepath.Join(accountsDir(), slug, "identity.json"))
	return err == nil
}

// ListAccounts describes every locally saved account without unlocking any
// of them (account.json is plaintext). Not wired into a switcher UI yet,
// but costs nothing to have ready for one.
func ListAccounts() []AccountSummary {
	entries, err := os.ReadDir(accountsDir())
	if err != nil {
		return []AccountSummary{}
	}
	out := make([]AccountSummary, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(accountsDir(), e.Name())
		username := "unnamed"
		if meta, err := readAccountMeta(dir); err == nil {
			username = meta.Username
		}
		_, hasAvatar := pfpIfExists(dir)
		out = append(out, AccountSummary{Slug: e.Name(), Username: username, HasAvatar: hasAvatar})
	}
	return out
}

// ── account.json ─────────────────────────────────────────────────────────

func readAccountMeta(dir string) (*accountMeta, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "account.json"))
	if err != nil {
		return nil, err
	}
	var m accountMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func writeAccountMeta(dir, username string) error {
	m := accountMeta{Username: username, CreatedAt: time.Now().UTC()}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "account.json"), data, 0600)
}

// ── pfp.png — optional at every layer, on purpose ──────────────────────────
//
// A profile picture is never required to create, import, unlock, or export
// an account. Every function here treats "file not present" as a normal,
// silent case rather than an error — nothing downstream should ever have to
// handle a "missing pfp" error, because one is never raised.

func pfpIfExists(dir string) (path string, ok bool) {
	p := filepath.Join(dir, "pfp.png")
	if info, err := os.Stat(p); err == nil && !info.IsDir() {
		return p, true
	}
	return "", false
}

// copyOptionalPfp best-effort copies sourcePath into dir as pfp.png.
// Deliberately swallows every error — a bad or missing source should never
// block account creation/import.
func copyOptionalPfp(sourcePath, dir string) {
	if sourcePath == "" {
		return
	}
	src, err := os.Open(sourcePath)
	if err != nil {
		return
	}
	defer src.Close()
	dst, err := os.OpenFile(filepath.Join(dir, "pfp.png"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer dst.Close()
	io.Copy(dst, src)
}

// ── Encryption ───────────────────────────────────────────────────────────

func writeEncryptedIdentity(dir string, pub ed25519.PublicKey, seed []byte, passphrase string) error {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("could not generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(passphrase), salt, kdfTime, kdfMemKiB, kdfThreads, kdfKeyLen)

	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("could not generate nonce: %w", err)
	}
	ciphertext := gcm.Seal(nil, nonce, seed, nil)

	id := encryptedIdentity{
		PublicKey: hex.EncodeToString(pub), KDF: "argon2id", KDFSalt: hex.EncodeToString(salt),
		KDFTime: kdfTime, KDFMemKiB: kdfMemKiB, KDFThreads: kdfThreads,
		Cipher: "aes-256-gcm", Nonce: hex.EncodeToString(nonce),
		Ciphertext: hex.EncodeToString(ciphertext), CreatedAt: time.Now().UTC(),
	}
	data, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "identity.json"), data, 0600)
}

// decryptIdentity derives the AES key from passphrase+salt and opens the
// GCM-sealed seed. A wrong passphrase fails the GCM auth tag check and
// returns a clean "incorrect passphrase" error — it can't silently produce
// garbage key material, which is the property we actually want here.
func decryptIdentity(dir, passphrase string) (ed25519.PublicKey, ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "identity.json"))
	if err != nil {
		return nil, nil, fmt.Errorf("could not read identity: %w", err)
	}
	var id encryptedIdentity
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, nil, fmt.Errorf("identity.json is corrupt: %w", err)
	}
	pub, err := hex.DecodeString(id.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, nil, fmt.Errorf("identity.json has a corrupt public key")
	}
	salt, err := hex.DecodeString(id.KDFSalt)
	if err != nil {
		return nil, nil, fmt.Errorf("identity.json has a corrupt salt")
	}
	nonce, err := hex.DecodeString(id.Nonce)
	if err != nil {
		return nil, nil, fmt.Errorf("identity.json has a corrupt nonce")
	}
	ciphertext, err := hex.DecodeString(id.Ciphertext)
	if err != nil {
		return nil, nil, fmt.Errorf("identity.json has corrupt ciphertext")
	}

	key := argon2.IDKey([]byte(passphrase), salt, id.KDFTime, id.KDFMemKiB, id.KDFThreads, kdfKeyLen)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	seed, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("incorrect passphrase")
	}

	priv := ed25519.NewKeyFromSeed(seed)
	if string(priv.Public().(ed25519.PublicKey)) != string(pub) {
		return nil, nil, fmt.Errorf("identity.json is corrupt (key mismatch)")
	}
	return ed25519.PublicKey(pub), priv, nil
}

// ── Outer container encryption ──────────────────────────────────────────
//
// identity.json's private key is already encrypted at rest — that alone
// protects the key material even in a plaintext zip. But unzipping an
// exported account still exposes the username and pfp.png in the clear,
// and once the export also carries the local server list (below), it
// carries join keys too. So the exported file itself gets a second,
// separate layer of encryption wrapping the whole archive — a different
// derived key from a fresh salt, never reused from identity.json's own
// encryption even though both come from the same passphrase.

// Magic bytes identifying an encrypted account-key container, so import
// can tell it apart from a plain zip (an older export, or a folder someone
// zipped by hand without going through ExportAccount).
var containerMagic = []byte("CCAK1")

func encryptContainer(plainZip []byte, passphrase string) ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("could not generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(passphrase), salt, kdfTime, kdfMemKiB, kdfThreads, kdfKeyLen)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("could not generate nonce: %w", err)
	}
	ciphertext := gcm.Seal(nil, nonce, plainZip, nil)

	out := make([]byte, 0, len(containerMagic)+len(salt)+len(nonce)+len(ciphertext))
	out = append(out, containerMagic...)
	out = append(out, salt...)
	out = append(out, nonce...)
	out = append(out, ciphertext...)
	return out, nil
}

func decryptContainer(data []byte, passphrase string) ([]byte, error) {
	if len(data) < len(containerMagic) || string(data[:len(containerMagic)]) != string(containerMagic) {
		return nil, fmt.Errorf("not a valid account key file")
	}
	rest := data[len(containerMagic):]
	const saltLen, nonceLen = 16, 12 // fixed sizes used by encryptContainer above
	if len(rest) < saltLen+nonceLen {
		return nil, fmt.Errorf("account key file is corrupt")
	}
	salt, nonce, ciphertext := rest[:saltLen], rest[saltLen:saltLen+nonceLen], rest[saltLen+nonceLen:]

	key := argon2.IDKey([]byte(passphrase), salt, kdfTime, kdfMemKiB, kdfThreads, kdfKeyLen)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("incorrect passphrase")
	}
	return plain, nil
}

// ── Account lifecycle ────────────────────────────────────────────────────

// CreateAccount generates a fresh Ed25519 identity, encrypts it under
// passphrase, and makes it the active account. pfpSourcePath may be empty.
func CreateAccount(username, passphrase, pfpSourcePath string) (*Account, error) {
	if strings.TrimSpace(username) == "" {
		return nil, fmt.Errorf("username cannot be empty")
	}
	if passphrase == "" {
		return nil, fmt.Errorf("passphrase cannot be empty")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("could not generate identity key: %w", err)
	}
	slug := fingerprintOf(pub)
	dir := filepath.Join(accountsDir(), slug)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("could not create account directory: %w", err)
	}
	if err := writeEncryptedIdentity(dir, pub, priv.Seed(), passphrase); err != nil {
		return nil, err
	}
	if err := writeAccountMeta(dir, username); err != nil {
		return nil, err
	}
	copyOptionalPfp(pfpSourcePath, dir)
	if err := setActiveSlug(slug); err != nil {
		return nil, err
	}

	avatarPath, hasAvatar := pfpIfExists(dir)
	return &Account{
		Slug: slug, Username: username, PublicKey: pub, PrivateKey: priv,
		HasAvatar: hasAvatar, AvatarPath: avatarPath,
	}, nil
}

// unlockSlug decrypts an already-imported/created account by its slug.
func unlockSlug(slug, passphrase string) (*Account, error) {
	dir := filepath.Join(accountsDir(), slug)
	pub, priv, err := decryptIdentity(dir, passphrase)
	if err != nil {
		return nil, err
	}
	meta, err := readAccountMeta(dir)
	username := "unnamed"
	if err == nil {
		username = meta.Username
	}
	avatarPath, hasAvatar := pfpIfExists(dir)
	return &Account{
		Slug: slug, Username: username, PublicKey: pub, PrivateKey: priv,
		HasAvatar: hasAvatar, AvatarPath: avatarPath,
	}, nil
}

// UnlockActiveAccount decrypts whichever account is currently marked active.
func UnlockActiveAccount(passphrase string) (*Account, error) {
	slug, err := getActiveSlug()
	if err != nil || slug == "" {
		return nil, fmt.Errorf("no account is set up yet")
	}
	return unlockSlug(slug, passphrase)
}

// ImportAccount accepts either a directory or a .zip containing
// identity.json (+ optional account.json / pfp.png), copies it into local
// account storage under its own fingerprint, and unlocks it. This is the
// exact mirror of ExportAccount — the files aren't re-encoded, just moved.
func ImportAccount(sourcePath, passphrase string) (*Account, error) {
	info, err := os.Stat(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", sourcePath, err)
	}

	srcDir := sourcePath
	cleanup := func() {}
	if !info.IsDir() {
		raw, err := os.ReadFile(sourcePath)
		if err != nil {
			return nil, fmt.Errorf("could not read %s: %w", sourcePath, err)
		}
		// An encrypted container (current Export format) needs decrypting
		// to get the raw zip bytes back; a plain .zip (an older export, or
		// one assembled by hand) is used as-is. Either way we end up with
		// zip bytes to extract — nothing on disk needs to be a real zip
		// file for the encrypted case, it's decrypted straight into memory.
		zipBytes := raw
		if len(raw) >= len(containerMagic) && string(raw[:len(containerMagic)]) == string(containerMagic) {
			zipBytes, err = decryptContainer(raw, passphrase)
			if err != nil {
				return nil, err
			}
		} else if !strings.EqualFold(filepath.Ext(sourcePath), ".zip") {
			return nil, fmt.Errorf("expected an account key file or a folder")
		}

		tmp, err := os.MkdirTemp("", "chriscord-import-*")
		if err != nil {
			return nil, fmt.Errorf("could not create temp directory: %w", err)
		}
		if err := unzipBytes(zipBytes, tmp); err != nil {
			os.RemoveAll(tmp)
			return nil, fmt.Errorf("could not extract account key: %w", err)
		}
		srcDir, cleanup = tmp, func() { os.RemoveAll(tmp) }
	}
	defer cleanup()

	identityRaw, err := os.ReadFile(filepath.Join(srcDir, "identity.json"))
	if err != nil {
		return nil, fmt.Errorf("no identity.json found in %s", sourcePath)
	}
	var id encryptedIdentity
	if err := json.Unmarshal(identityRaw, &id); err != nil {
		return nil, fmt.Errorf("identity.json is not valid: %w", err)
	}
	pubBytes, err := hex.DecodeString(id.PublicKey)
	if err != nil || len(pubBytes) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("identity.json has a corrupt public key")
	}
	slug := fingerprintOf(pubBytes)

	destDir := filepath.Join(accountsDir(), slug)
	if err := os.MkdirAll(destDir, 0700); err != nil {
		return nil, fmt.Errorf("could not create account directory: %w", err)
	}
	if err := copyFile(filepath.Join(srcDir, "identity.json"), filepath.Join(destDir, "identity.json"), 0600); err != nil {
		return nil, fmt.Errorf("could not import identity: %w", err)
	}
	if meta, err := readAccountMeta(srcDir); err == nil {
		writeAccountMeta(destDir, meta.Username)
	} else {
		writeAccountMeta(destDir, "unnamed") // account.json is optional too — don't fail the import over it
	}
	copyOptionalPfp(filepath.Join(srcDir, "pfp.png"), destDir)
	mergeServersIfPresent(filepath.Join(srcDir, "servers.json"))

	account, err := unlockSlug(slug, passphrase)
	if err != nil {
		return nil, err // files are imported either way; just not unlocked with a bad passphrase
	}
	if err := setActiveSlug(slug); err != nil {
		return nil, err
	}
	return account, nil
}

// mergeServersIfPresent folds a bundled server-list snapshot into this
// install's own saved servers, keyed by domain. An existing local entry
// for a domain is left alone rather than overwritten — the bundle is a
// snapshot from whenever it was exported, and shouldn't clobber a join key
// that's since been updated locally.
func mergeServersIfPresent(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return // no servers.json in this bundle — nothing to do
	}
	var bundled []SavedServer
	if err := json.Unmarshal(raw, &bundled); err != nil {
		return
	}
	current := loadServers()
	existing := make(map[string]bool, len(current))
	for _, s := range current {
		existing[s.Domain] = true
	}
	changed := false
	for _, s := range bundled {
		if !existing[s.Domain] {
			current = upsertServer(current, s)
			changed = true
		}
	}
	if changed {
		saveServers(current)
	}
}

// ExportAccount zips identity.json, account.json, pfp.png (if present), and
// a snapshot of the local saved-server list (if any) for an already-
// imported/created account, then encrypts the whole archive under
// passphrase — see "Outer container encryption" above for why. passphrase
// must match the one this account's identity.json was encrypted under;
// verified up front so a typo produces a clean error instead of a useless
// file at the destination.
func ExportAccount(slug, destPath, passphrase string) error {
	dir := filepath.Join(accountsDir(), slug)
	if _, _, err := decryptIdentity(dir, passphrase); err != nil {
		return err // wrong passphrase, or the account itself is corrupt
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"identity.json", "account.json", "pfp.png"} {
		srcPath := filepath.Join(dir, name)
		if _, err := os.Stat(srcPath); err != nil {
			continue // pfp.png especially is commonly absent — skip quietly
		}
		if err := addFileToZip(zw, srcPath, name); err != nil {
			return err
		}
	}
	// Bundle a snapshot of the local saved-server list too, so importing on
	// a new machine doesn't mean re-adding every server by hand. This is
	// exactly why the outer encryption above matters: a server's join key
	// travels along with it now.
	if servers := loadServers(); len(servers) > 0 {
		if data, err := json.MarshalIndent(servers, "", "  "); err == nil {
			if w, err := zw.Create("servers.json"); err == nil {
				w.Write(data)
			}
		}
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("could not build account key archive: %w", err)
	}

	encrypted, err := encryptContainer(buf.Bytes(), passphrase)
	if err != nil {
		return err
	}
	return os.WriteFile(destPath, encrypted, 0600)
}

// ── Small file/zip helpers ───────────────────────────────────────────────

func copyFile(srcPath, dstPath string, perm os.FileMode) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	defer dst.Close()
	_, err = io.Copy(dst, src)
	return err
}

func addFileToZip(zw *zip.Writer, srcPath, nameInZip string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	w, err := zw.Create(nameInZip)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, src)
	return err
}

// unzipBytes extracts in-memory zip data to destDir, rejecting any entry
// that would escape it (zip-slip) — worth guarding since these bundles are
// meant to be handed around and could come from anywhere, including a repo
// you didn't create. Takes bytes rather than a file path because the
// decrypted container's zip data never touches disk as an actual .zip file.
func unzipBytes(data []byte, destDir string) error {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	for _, f := range r.File {
		cleanName := filepath.Clean(f.Name)
		if strings.HasPrefix(cleanName, "..") || filepath.IsAbs(cleanName) {
			return fmt.Errorf("unsafe path in zip: %s", f.Name)
		}
		targetPath := filepath.Join(destDir, cleanName)
		if f.FileInfo().IsDir() {
			os.MkdirAll(targetPath, 0700)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(targetPath), 0700); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(targetPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

package main

// "Keep me signed in": the account key is saved locked to this computer's
// Windows user (DPAPI, see remember_windows.go), so the next start can
// sign in without the passphrase.
//
// What's saved is the 32-byte key itself, encrypted by Windows with a key
// derived from your Windows sign-in; the passphrase is never stored. The
// file is useless on another computer, to another Windows user, or copied
// off this one. Like a browser's saved passwords, a program running as you
// on this computer could still ask Windows to decrypt it. That's the
// trade-off for not typing the passphrase.
//
// Logging out deletes it. If it can't be decrypted any more (a Windows
// password reset by an administrator, a profile restored onto a new PC),
// it's deleted and the passphrase is asked for as usual.

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const rememberFileName = "remember.bin"

func rememberPath(slug string) string { return filepath.Join(accountsDir(), slug, rememberFileName) }

// rememberEntropy ties a saved key to its account, so one account's file
// can't be swapped in for another's.
func rememberEntropy(slug string) []byte { return []byte("chriscord-remember-me:" + slug) }

// saveRemembered keeps acct signed in on this computer.
func saveRemembered(acct *Account) error {
	if !rememberSupported {
		return fmt.Errorf("staying signed in isn't available on this system yet")
	}
	seed := acct.PrivateKey.Seed()
	blob, err := osProtect(seed, rememberEntropy(acct.Slug))
	for i := range seed {
		seed[i] = 0
	}
	if err != nil {
		return fmt.Errorf("couldn't save your sign-in: %w", err)
	}
	tmp := rememberPath(acct.Slug) + ".tmp"
	if err := os.WriteFile(tmp, blob, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, rememberPath(acct.Slug))
}

// forgetRemembered deletes a saved sign-in (fine if there isn't one).
func forgetRemembered(slug string) {
	if slug != "" {
		os.Remove(rememberPath(slug))
	}
}

func isRemembered(slug string) bool {
	_, err := os.Stat(rememberPath(slug))
	return rememberSupported && err == nil
}

// loadRemembered signs in with a saved key: decrypted by Windows, then
// checked against the account's public key so a wrong or damaged file can
// never sign in as anyone.
func loadRemembered(slug string) (*Account, error) {
	blob, err := os.ReadFile(rememberPath(slug))
	if err != nil {
		return nil, err
	}
	seed, err := osUnprotect(blob, rememberEntropy(slug))
	if err != nil {
		return nil, fmt.Errorf("the saved sign-in can't be unlocked on this computer: %w", err)
	}
	defer func() {
		for i := range seed {
			seed[i] = 0
		}
	}()
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("the saved sign-in is damaged")
	}
	dir := filepath.Join(accountsDir(), slug)
	raw, err := os.ReadFile(filepath.Join(dir, "identity.json"))
	if err != nil {
		return nil, fmt.Errorf("could not read identity: %w", err)
	}
	var id encryptedIdentity
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, fmt.Errorf("identity.json is corrupt: %w", err)
	}
	pub, err := hex.DecodeString(id.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("identity.json has a corrupt public key")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	if !bytes.Equal(priv.Public().(ed25519.PublicKey), pub) {
		return nil, fmt.Errorf("the saved sign-in doesn't match this account")
	}
	return accountFromKeys(slug, ed25519.PublicKey(pub), priv), nil
}

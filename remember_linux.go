//go:build linux

package main

// "Keep me signed in" on Linux.
//
// Windows has one answer (DPAPI, remember_windows.go). Linux has two:
//
//  1. The desktop's keyring, through the freedesktop Secret Service over
//     D-Bus — GNOME Keyring, KWallet (5.97+), KeePassXC… all speak it.
//     A random 32-byte key is kept there (one item for all of this user's
//     Chriscord accounts, "Chriscord sign-in key"), unlocked with your
//     login like any saved password. Only that key is in the keyring; the
//     account key itself stays in remember.bin, encrypted with it.
//
//  2. No keyring (a plain X session with i3, dwm, Openbox… and nothing
//     running gnome-keyring): the same random key goes in a file instead,
//     ~/.config/chriscord/remember.key, readable only by you (0600).
//     That keeps the account key off the disk in the clear and tied to
//     this config folder, but anything that can read your home folder can
//     use it — the same as Discord's saved token, or a browser's saved
//     passwords without a keyring. Logging out deletes remember.bin.
//
// Which one sealed a file is recorded in its first bytes, so a file made
// with the keyring is never silently "unlocked" with a key file, and vice
// versa. Either way the account and file name are bound in (entropy, as
// associated data), and remember.go checks the result against the
// account's public key.
//
// If the keyring is there but can't be reached right now (still starting
// at sign-in, locked and the unlock prompt dismissed), loading returns
// errRememberTemporary: the saved sign-in is kept and the passphrase is
// asked for this once, rather than throwing the saved sign-in away.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/crypto/chacha20poly1305"
)

const rememberSupported = true

const (
	rememberMagic      = "CCR1"
	rememberSrcKeyring = byte(1)
	rememberSrcFile    = byte(2)
	rememberKeyFile    = "remember.key"
)

// rememberDescription: the "Keep me signed in" tooltip.
func rememberDescription() string {
	if keyringReachable() {
		return "Your account key is saved encrypted, with its key in your desktop's keyring, so only you, signed in on this computer, can use it. Log out to undo."
	}
	return "Your account key is saved encrypted on this computer, with its key in a file only your user can read (no desktop keyring was found). Log out to undo."
}

func osProtect(data, entropy []byte) ([]byte, error) {
	src := rememberSrcKeyring
	key, err := keyringKey(true)
	if err != nil {
		src = rememberSrcFile
		if key, err = fileKey(true); err != nil {
			return nil, err
		}
	}
	defer wipe(key)
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte(rememberMagic), src)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, data, entropy), nil
}

func osUnprotect(blob, entropy []byte) ([]byte, error) {
	hdr := len(rememberMagic) + 1
	if len(blob) < hdr+chacha20poly1305.NonceSizeX+chacha20poly1305.Overhead || string(blob[:len(rememberMagic)]) != rememberMagic {
		return nil, errors.New("not a saved sign-in this version understands")
	}
	var key []byte
	var err error
	switch blob[len(rememberMagic)] {
	case rememberSrcKeyring:
		key, err = keyringKey(false)
		if err != nil && !errors.Is(err, errKeyringNoItem) {
			// The keyring isn't answering (yet): keep the saved sign-in.
			return nil, fmt.Errorf("%w: %v", errRememberTemporary, err)
		}
	case rememberSrcFile:
		key, err = fileKey(false)
	default:
		return nil, errors.New("not a saved sign-in this version understands")
	}
	if err != nil {
		return nil, err
	}
	defer wipe(key)
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	nonce := blob[hdr : hdr+aead.NonceSize()]
	plain, err := aead.Open(nil, nonce, blob[hdr+aead.NonceSize():], entropy)
	if err != nil {
		return nil, errors.New("it doesn't match the key it was saved with")
	}
	return plain, nil
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// ── The key file (no keyring) ──────────────────────────────────────────────

func fileKey(create bool) ([]byte, error) {
	path := filepath.Join(chriscordConfigDir(), rememberKeyFile)
	if k, err := os.ReadFile(path); err == nil {
		if len(k) != chacha20poly1305.KeySize {
			return nil, errors.New("remember.key is damaged")
		}
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0077 != 0 {
			os.Chmod(path, 0600) // someone loosened it: tighten it back
		}
		return k, nil
	} else if !os.IsNotExist(err) || !create {
		return nil, err
	}
	k := make([]byte, chacha20poly1305.KeySize)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, k, 0600); err != nil {
		return nil, err
	}
	return k, nil
}

// ── The keyring (freedesktop Secret Service) ───────────────────────────────

const (
	ssDest     = "org.freedesktop.secrets"
	ssPath     = dbus.ObjectPath("/org/freedesktop/secrets")
	ssService  = "org.freedesktop.Secret.Service"
	ssItem     = "org.freedesktop.Secret.Item"
	ssColl     = "org.freedesktop.Secret.Collection"
	ssPromptIf = "org.freedesktop.Secret.Prompt"
)

var errKeyringNoItem = errors.New("no Chriscord key in the keyring yet")

// ssSecret is the Secret Service's (oayays) Secret struct.
type ssSecret struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

func ssAttrs() map[string]string {
	return map[string]string{"application": "chriscord", "xdg:schema": "app.chriscord.RememberKey"}
}

// sessionBusUsable: only use D-Bus if a session bus is clearly there —
// otherwise godbus would try to start one (dbus-launch), which on a bare X
// session can pop things up or hang.
func sessionBusUsable() bool {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" {
		return true
	}
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		if _, err := os.Stat(filepath.Join(rt, "bus")); err == nil {
			return true
		}
	}
	return false
}

func ssCall(obj dbus.BusObject, method string, args ...interface{}) *dbus.Call {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return obj.CallWithContext(ctx, method, 0, args...)
}

// keyringReachable: a keyring answers on D-Bus (nothing is read, so no
// unlock prompt can appear).
func keyringReachable() bool {
	if !sessionBusUsable() {
		return false
	}
	bus, err := dbus.SessionBus()
	if err != nil {
		return false
	}
	var ignored dbus.Variant
	var session dbus.ObjectPath
	if ssCall(bus.Object(ssDest, ssPath), ssService+".OpenSession", "plain", dbus.MakeVariant("")).Store(&ignored, &session) != nil {
		return false
	}
	ssCall(bus.Object(ssDest, session), "org.freedesktop.Secret.Session.Close")
	return true
}

// keyringKey gets Chriscord's key from the keyring, making it first when
// create is set. errKeyringNoItem: the keyring works but has no key yet.
func keyringKey(create bool) ([]byte, error) {
	if !sessionBusUsable() {
		return nil, errors.New("no D-Bus session")
	}
	bus, err := dbus.SessionBus() // shared connection: never Close it
	if err != nil {
		return nil, err
	}
	svc := bus.Object(ssDest, ssPath)
	var ignored dbus.Variant
	var session dbus.ObjectPath
	if err := ssCall(svc, ssService+".OpenSession", "plain", dbus.MakeVariant("")).Store(&ignored, &session); err != nil {
		return nil, fmt.Errorf("no keyring: %w", err)
	}
	defer ssCall(bus.Object(ssDest, session), "org.freedesktop.Secret.Session.Close")

	var unlocked, locked []dbus.ObjectPath
	if err := ssCall(svc, ssService+".SearchItems", ssAttrs()).Store(&unlocked, &locked); err != nil {
		return nil, fmt.Errorf("keyring search failed: %w", err)
	}
	if len(unlocked) == 0 && len(locked) > 0 {
		if unlocked, err = ssUnlock(bus, locked[:1]); err != nil {
			return nil, err
		}
	}
	// (A key left in the memory-only "session" collection doesn't count.)
	unlocked = withoutSessionItems(unlocked)
	if len(unlocked) > 0 {
		var s ssSecret
		if err := ssCall(bus.Object(ssDest, unlocked[0]), ssItem+".GetSecret", session).Store(&s); err != nil {
			return nil, fmt.Errorf("couldn't read the keyring: %w", err)
		}
		if len(s.Value) != chacha20poly1305.KeySize {
			return nil, errors.New("the Chriscord key in the keyring is damaged")
		}
		return s.Value, nil
	}
	if !create {
		return nil, errKeyringNoItem
	}

	// No key yet: make one in the default collection (your login keyring).
	var coll dbus.ObjectPath
	if err := ssCall(svc, ssService+".ReadAlias", "default").Store(&coll); err != nil || coll == "/" || coll == "" {
		return nil, errors.New("the keyring has no default collection")
	}
	// The "session" collection is kept in memory only, gone at the next
	// sign-in (the keyring's default when it wasn't started with your login
	// — common outside GNOME/KDE). A key there would make the saved sign-in
	// useless next time: use the key file instead.
	if strings.HasSuffix(string(coll), "/collection/session") {
		return nil, errors.New("the keyring only keeps things until you sign out")
	}
	if _, err := ssUnlock(bus, []dbus.ObjectPath{coll}); err != nil {
		return nil, err
	}
	key := make([]byte, chacha20poly1305.KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	props := map[string]dbus.Variant{
		ssItem + ".Label":      dbus.MakeVariant("Chriscord sign-in key"),
		ssItem + ".Attributes": dbus.MakeVariant(ssAttrs()),
	}
	secret := ssSecret{Session: session, Parameters: []byte{}, Value: key, ContentType: "application/octet-stream"}
	var item, prompt dbus.ObjectPath
	if err := ssCall(bus.Object(ssDest, coll), ssColl+".CreateItem", props, secret, true).Store(&item, &prompt); err != nil {
		return nil, fmt.Errorf("couldn't save to the keyring: %w", err)
	}
	if _, err := ssPrompt(bus, prompt); err != nil {
		return nil, err
	}
	return key, nil
}

func withoutSessionItems(items []dbus.ObjectPath) []dbus.ObjectPath {
	out := items[:0:0]
	for _, it := range items {
		if !strings.Contains(string(it), "/collection/session/") {
			out = append(out, it)
		}
	}
	return out
}

// ssUnlock unlocks items or a collection, showing the keyring's own
// password prompt if it needs one.
func ssUnlock(bus *dbus.Conn, paths []dbus.ObjectPath) ([]dbus.ObjectPath, error) {
	var done []dbus.ObjectPath
	var prompt dbus.ObjectPath
	if err := ssCall(bus.Object(ssDest, ssPath), ssService+".Unlock", paths).Store(&done, &prompt); err != nil {
		return nil, fmt.Errorf("couldn't unlock the keyring: %w", err)
	}
	if prompt == "/" || prompt == "" {
		return done, nil
	}
	res, err := ssPrompt(bus, prompt)
	if err != nil {
		return nil, err
	}
	if p, ok := res.Value().([]dbus.ObjectPath); ok && len(p) > 0 {
		return p, nil
	}
	return paths, nil
}

// ssPrompt shows a keyring prompt and waits (up to two minutes) for the
// answer.
func ssPrompt(bus *dbus.Conn, prompt dbus.ObjectPath) (dbus.Variant, error) {
	if prompt == "/" || prompt == "" {
		return dbus.Variant{}, nil
	}
	match := []dbus.MatchOption{
		dbus.WithMatchObjectPath(prompt),
		dbus.WithMatchInterface(ssPromptIf),
		dbus.WithMatchMember("Completed"),
	}
	if err := bus.AddMatchSignal(match...); err != nil {
		return dbus.Variant{}, err
	}
	defer bus.RemoveMatchSignal(match...)
	ch := make(chan *dbus.Signal, 8)
	bus.Signal(ch)
	defer bus.RemoveSignal(ch)

	if err := ssCall(bus.Object(ssDest, prompt), ssPromptIf+".Prompt", "").Err; err != nil {
		return dbus.Variant{}, fmt.Errorf("keyring prompt failed: %w", err)
	}
	timeout := time.After(2 * time.Minute)
	for {
		select {
		case sig := <-ch:
			if sig == nil || sig.Path != prompt || sig.Name != ssPromptIf+".Completed" || len(sig.Body) < 2 {
				continue
			}
			if dismissed, _ := sig.Body[0].(bool); dismissed {
				return dbus.Variant{}, errors.New("the keyring wasn't unlocked")
			}
			res, _ := sig.Body[1].(dbus.Variant)
			return res, nil
		case <-timeout:
			return dbus.Variant{}, errors.New("the keyring didn't answer")
		}
	}
}

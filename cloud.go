package main

// cloud.go — optional online accounts on chriscord.app.
//
// An account stays exactly as it is (a key on this computer) unless you
// make it online. Then chriscord.app keeps:
//   - your username and public key, in a public directory (the key never
//     changes; the username can),
//   - an encrypted vault with your account key, profile, pictures,
//     appearance and saved servers, which only you can open.
// The password never leaves this app; cloudproto.go has the key layout.
//
// Signing in after the first time needs no password: this app signs a
// challenge with the account key (like joining a server) and gets the
// vault's key back wrapped under a key derived from the account key.
// The password is only for a new computer, and the recovery key for a
// forgotten password.
//
// Sync: whenever the profile, appearance or server list changes (and on
// every unlock), the vault is fetched, merged with what's here, and
// uploaded if anything changed. Merging is per part — the profile and the
// picture go by their timestamps, appearance and servers by what changed
// since the last sync on this computer (cloudState.Base), so a server
// removed here stays removed and one added elsewhere shows up.

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

const defaultCloudServer = "https://chriscord.app"

// cloudServerURL is chriscord.app, or CHRISCORD_CLOUD for testing against
// your own copy of the service.
func cloudServerURL() string {
	if v := strings.TrimSpace(os.Getenv("CHRISCORD_CLOUD")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultCloudServer
}

// ── cloud.json: this computer's record of an online account ────────────────

const cloudStateFile = "cloud.json"

type cloudState struct {
	Server   string    `json:"server"`
	Username string    `json:"username"`
	LinkedAt time.Time `json:"linked_at"`
	// The newest vault version seen: an older one from the service is
	// refused, so it can't quietly roll your data back.
	VaultVersion int64     `json:"vault_version"`
	LastSync     time.Time `json:"last_sync,omitempty"`
	Base         cloudBase `json:"base"`
}

// cloudBase is what the last sync from this computer agreed on, which is
// how a merge tells "removed here" from "added elsewhere".
type cloudBase struct {
	Servers  map[string]string `json:"servers,omitempty"`  // server key → hash of its entry
	Settings string            `json:"settings,omitempty"` // hash of client-settings.json
}

func cloudStatePath(slug string) string { return filepath.Join(accountsDir(), slug, cloudStateFile) }

func readCloudState(slug string) (*cloudState, error) {
	raw, err := os.ReadFile(cloudStatePath(slug))
	if err != nil {
		return nil, err
	}
	var st cloudState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	if st.Server == "" {
		st.Server = defaultCloudServer
	}
	return &st, nil
}

func writeCloudState(slug string, st *cloudState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := cloudStatePath(slug) + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, cloudStatePath(slug))
}

func isOnlineAccount(slug string) bool {
	_, err := os.Stat(cloudStatePath(slug))
	return err == nil
}

// ── The vault's contents ───────────────────────────────────────────────────

type vaultProfile struct {
	CreatedAt        time.Time `json:"created_at"`
	PfpUpdatedAt     int64     `json:"pfp_updated_at,omitempty"`
	Bio              string    `json:"bio,omitempty"`
	Tint             string    `json:"tint,omitempty"`
	Nickname         string    `json:"nickname,omitempty"`
	ProfileUpdatedAt int64     `json:"profile_updated_at,omitempty"`
}

type vaultDoc struct {
	Format     int             `json:"format"`
	Seed       string          `json:"seed"` // hex: the account key itself
	Profile    vaultProfile    `json:"profile"`
	Settings   json.RawMessage `json:"settings,omitempty"` // client-settings.json
	SettingsAt int64           `json:"settings_at,omitempty"`
	Servers    []SavedServer   `json:"servers"`
}

// vaultSnapshot is a vault, or the same things as they are on this
// computer, ready to compare.
type vaultSnapshot struct {
	Doc    vaultDoc
	Pfp    []byte
	Banner []byte
}

func (v *vaultSnapshot) marshal() ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name string, data []byte) error {
		w, err := zw.Create(name)
		if err == nil {
			_, err = w.Write(data)
		}
		return err
	}
	doc, err := json.Marshal(v.Doc)
	if err != nil {
		return nil, err
	}
	if err := add("vault.json", doc); err != nil {
		return nil, err
	}
	if len(v.Pfp) > 0 {
		add("pfp.png", v.Pfp)
	}
	if len(v.Banner) > 0 {
		add("banner.png", v.Banner)
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func unmarshalVault(data []byte) (*vaultSnapshot, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("the synced copy is corrupt: %w", err)
	}
	v := &vaultSnapshot{}
	found := false
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(rc, 64<<20))
		rc.Close()
		if err != nil {
			return nil, err
		}
		switch f.Name {
		case "vault.json":
			if err := json.Unmarshal(data, &v.Doc); err != nil {
				return nil, fmt.Errorf("the synced copy is corrupt: %w", err)
			}
			found = true
		case "pfp.png":
			v.Pfp = data
		case "banner.png":
			v.Banner = data
		}
	}
	if !found {
		return nil, errors.New("the synced copy is corrupt (no vault.json)")
	}
	return v, nil
}

func (v *vaultSnapshot) privateKey() (ed25519.PrivateKey, error) {
	seed, err := hex.DecodeString(v.Doc.Seed)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("the synced copy has no usable account key")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func hashOf(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func serverHash(s SavedServer) string {
	data, _ := json.Marshal(s)
	return hashOf(data)
}

// serverKey compares addresses without http(s):// or a trailing slash, as
// mergeServersIfPresent does.
func serverKey(domain string) string {
	d := strings.ToLower(strings.TrimRight(strings.TrimSpace(domain), "/"))
	return strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://")
}

func settingsHash(raw []byte) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	return hashOf(raw)
}

// localSnapshot is this computer's side of a sync.
func localSnapshot(acct *Account, servers []SavedServer) *vaultSnapshot {
	dir := filepath.Join(accountsDir(), acct.Slug)
	v := &vaultSnapshot{Doc: vaultDoc{
		Format: 1,
		Seed:   hex.EncodeToString(acct.PrivateKey.Seed()),
		Profile: vaultProfile{
			PfpUpdatedAt: acct.PfpUpdatedAt, Bio: acct.Bio, Tint: acct.Tint, Nickname: acct.Nickname,
			ProfileUpdatedAt: acct.ProfileUpdatedAt,
		},
		Servers: append([]SavedServer{}, servers...),
	}}
	if meta, err := readAccountMeta(dir); err == nil {
		v.Doc.Profile.CreatedAt = meta.CreatedAt
	}
	if acct.HasAvatar {
		v.Pfp, _ = os.ReadFile(acct.AvatarPath)
	}
	v.Banner, _ = os.ReadFile(bannerPath(dir))
	if raw, err := os.ReadFile(filepath.Join(dir, clientSettingsFile)); err == nil && json.Valid(raw) {
		v.Doc.Settings = raw
		if info, err := os.Stat(filepath.Join(dir, clientSettingsFile)); err == nil {
			v.Doc.SettingsAt = info.ModTime().Unix()
		}
	}
	return v
}

type mergeResult struct {
	merged                          *vaultSnapshot
	profile, pfp, settings, servers bool // changed here
}

// mergeVault combines this computer's copy with the synced one.
func mergeVault(local, remote *vaultSnapshot, base cloudBase) mergeResult {
	m := &vaultSnapshot{Doc: local.Doc}
	m.Pfp, m.Banner = local.Pfp, local.Banner
	r := mergeResult{merged: m}
	lp, rp := local.Doc.Profile, remote.Doc.Profile

	// Profile (bio, colour, nickname, banner) and picture: the newer one.
	if rp.ProfileUpdatedAt > lp.ProfileUpdatedAt {
		m.Doc.Profile.Bio, m.Doc.Profile.Tint, m.Doc.Profile.Nickname = rp.Bio, rp.Tint, rp.Nickname
		m.Doc.Profile.ProfileUpdatedAt = rp.ProfileUpdatedAt
		m.Banner = remote.Banner
		r.profile = true
	}
	if rp.PfpUpdatedAt > lp.PfpUpdatedAt && len(remote.Pfp) > 0 {
		m.Doc.Profile.PfpUpdatedAt = rp.PfpUpdatedAt
		m.Pfp = remote.Pfp
		r.pfp = true
	}
	if !rp.CreatedAt.IsZero() && (lp.CreatedAt.IsZero() || rp.CreatedAt.Before(lp.CreatedAt)) {
		m.Doc.Profile.CreatedAt = rp.CreatedAt
	}

	// Appearance: whichever side changed since the last sync; if both
	// did, the newer.
	lh, rh := settingsHash(local.Doc.Settings), settingsHash(remote.Doc.Settings)
	if rh != "" && rh != lh {
		takeRemote := lh == "" || lh == base.Settings ||
			(rh != base.Settings && remote.Doc.SettingsAt > local.Doc.SettingsAt)
		if takeRemote {
			m.Doc.Settings, m.Doc.SettingsAt = remote.Doc.Settings, remote.Doc.SettingsAt
			r.settings = true
		}
	}

	// Saved servers, one at a time.
	remoteBy := map[string]SavedServer{}
	for _, s := range remote.Doc.Servers {
		remoteBy[serverKey(s.Domain)] = s
	}
	seen := map[string]bool{}
	var out []SavedServer
	for _, l := range local.Doc.Servers {
		k := serverKey(l.Domain)
		if seen[k] {
			continue
		}
		seen[k] = true
		rs, inRemote := remoteBy[k]
		b, inBase := base.Servers[k]
		switch {
		case inRemote:
			lh, rh := serverHash(l), serverHash(rs)
			if lh != rh && lh == b { // changed elsewhere only
				out = append(out, rs)
				r.servers = true
			} else {
				out = append(out, l)
			}
		case inBase && serverHash(l) == b:
			r.servers = true // removed elsewhere, unchanged here: remove
		default:
			out = append(out, l) // new here (or changed here after being removed elsewhere)
		}
	}
	for _, rs := range remote.Doc.Servers {
		k := serverKey(rs.Domain)
		if seen[k] {
			continue
		}
		seen[k] = true
		if b, inBase := base.Servers[k]; inBase && serverHash(rs) == b {
			continue // removed here: stays removed
		}
		out = append(out, rs) // added elsewhere
		r.servers = true
	}
	if out == nil {
		out = []SavedServer{}
	}
	m.Doc.Servers = out
	return r
}

func baseOf(v *vaultSnapshot) cloudBase {
	b := cloudBase{Servers: map[string]string{}, Settings: settingsHash(v.Doc.Settings)}
	for _, s := range v.Doc.Servers {
		b.Servers[serverKey(s.Domain)] = serverHash(s)
	}
	return b
}

// sameVault reports whether uploading merged would change anything.
func sameVault(a, b *vaultSnapshot) bool {
	x, err1 := json.Marshal(a.Doc)
	y, err2 := json.Marshal(b.Doc)
	return err1 == nil && err2 == nil && bytes.Equal(x, y) && bytes.Equal(a.Pfp, b.Pfp) && bytes.Equal(a.Banner, b.Banner)
}

// ── Talking to chriscord.app ───────────────────────────────────────────────

type cloudAccount struct {
	Username       string `json:"username"`
	PublicKey      string `json:"public_key"`
	Email          string `json:"email"`
	EmailVerified  bool   `json:"email_verified"`
	VerifyBy       int64  `json:"verify_by"`
	CreatedAt      int64  `json:"created_at"`
	VaultVersion   int64  `json:"vault_version"`
	VaultSize      int64  `json:"vault_size"`
	VaultUpdatedAt int64  `json:"vault_updated_at"`
}

type cloudLoginResp struct {
	Token      string       `json:"token"`
	Account    cloudAccount `json:"account"`
	WrappedKey string       `json:"wrapped_key"`
	// A computer the account hasn't been used on: chriscord.app has emailed
	// a link to confirm it, and this is what to wait on (waitApproval).
	Approval  string `json:"approval"`
	Email     string `json:"email"` // where it was sent, partly hidden
	ExpiresIn int    `json:"expires_in"`
}

type cloudHTTPError struct {
	status int
	msg    string
	body   []byte
}

func (e *cloudHTTPError) Error() string { return e.msg }

func cloudStatus(err error) int {
	var he *cloudHTTPError
	if errors.As(err, &he) {
		return he.status
	}
	return 0
}

var cloudHTTP = &http.Client{Timeout: 60 * time.Second}

func cloudCall(server, method, path, token string, body, out interface{}) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, server+"/api/v1"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := cloudHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach %s — check your connection", strings.TrimPrefix(server, "https://"))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = fmt.Sprintf("%s answered %d", strings.TrimPrefix(server, "https://"), resp.StatusCode)
		}
		return &cloudHTTPError{status: resp.StatusCode, msg: e.Error, body: raw}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// cloudSign gets a one-time nonce and signs action+fields with the key.
func cloudSign(server string, priv ed25519.PrivateKey, action string, fields ...string) (nonce, sig string, err error) {
	pubHex := hex.EncodeToString(priv.Public().(ed25519.PublicKey))
	var ch struct {
		Nonce string `json:"nonce"`
	}
	if err := cloudCall(server, "POST", "/challenge", "", map[string]string{"public_key": pubHex}, &ch); err != nil {
		return "", "", err
	}
	return ch.Nonce, hex.EncodeToString(ed25519.Sign(priv, CloudSigMessage(action, ch.Nonce, fields...))), nil
}

func cloudPrelogin(server, username string) (CloudKDF, error) {
	var resp struct {
		KDF CloudKDF `json:"kdf"`
	}
	if err := cloudCall(server, "POST", "/prelogin", "", map[string]string{"username": username}, &resp); err != nil {
		return CloudKDF{}, err
	}
	if err := resp.KDF.Validate(); err != nil {
		return CloudKDF{}, fmt.Errorf("the server sent odd password settings (%v)", err)
	}
	return resp.KDF, nil
}

func cloudGetVault(server, token string) (int64, []byte, error) {
	req, _ := http.NewRequest("GET", server+"/api/v1/vault", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := cloudHTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("couldn't reach %s — check your connection", strings.TrimPrefix(server, "https://"))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, nil, &cloudHTTPError{status: resp.StatusCode, msg: fmt.Sprintf("couldn't download your synced data (%d)", resp.StatusCode)}
	}
	version, _ := strconv.ParseInt(resp.Header.Get("X-Vault-Version"), 10, 64)
	if resp.StatusCode == http.StatusNoContent {
		return version, nil, nil
	}
	blob, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	return version, blob, err
}

// cloudPutVault uploads a vault built on version base. conflict=true
// means another device uploaded first.
func cloudPutVault(server, token string, base int64, blob []byte) (version int64, conflict bool, err error) {
	req, _ := http.NewRequest("PUT", server+"/api/v1/vault", bytes.NewReader(blob))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Vault-Base", strconv.FormatInt(base, 10))
	resp, err := cloudHTTP.Do(req)
	if err != nil {
		return 0, false, fmt.Errorf("couldn't reach %s — check your connection", strings.TrimPrefix(server, "https://"))
	}
	defer resp.Body.Close()
	var out struct {
		Version int64  `json:"version"`
		Error   string `json:"error"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	switch {
	case resp.StatusCode == http.StatusConflict:
		return out.Version, true, nil
	case resp.StatusCode >= 400:
		if out.Error == "" {
			out.Error = fmt.Sprintf("couldn't upload your synced data (%d)", resp.StatusCode)
		}
		return 0, false, &cloudHTTPError{status: resp.StatusCode, msg: out.Error}
	}
	return out.Version, false, nil
}

// ── The sync engine ────────────────────────────────────────────────────────

// cloudSession is a signed-in session for the unlocked account.
type cloudSession struct {
	slug       string
	server     string
	pubHex     string
	token      string
	accountKey []byte
	account    cloudAccount
}

type cloudClient struct {
	app *App

	opMu sync.Mutex // one cloud operation at a time

	mu             sync.Mutex
	sess           *cloudSession
	timer          *time.Timer
	syncing        bool
	lastErr        string
	gone           bool   // the service says this account isn't online any more
	linked         string // the unlocked account's slug, if it's online
	applying       atomic.Bool
	feedStop       context.CancelFunc // the live change feed (feed below), if running
	approvalCancel chan struct{}      // closed to stop waiting for a new-computer confirmation

	keys *keyCheck // directory checks on other people's keys (keycheck.go)
}

func newCloudClient(a *App) *cloudClient {
	c := &cloudClient{app: a}
	c.keys = newKeyCheck(c)
	return c
}

// theCloud lets saveServers (storage.go) report changes.
var theCloud *cloudClient

func (c *cloudClient) emit(name string, data interface{}) {
	if c.app.ctx != nil {
		wailsruntime.EventsEmit(c.app.ctx, name, data)
	}
}

func (c *cloudClient) unlocked() *Account {
	c.app.mu.Lock()
	defer c.app.mu.Unlock()
	return c.app.account
}

// changed is called after anything that syncs changes here; the upload
// waits a few seconds so a burst of edits makes one. It never takes the
// App's lock: saveServers calls it while connect holds that.
func (c *cloudClient) changed() {
	if c == nil || c.applying.Load() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.linked == "" {
		return
	}
	if c.timer != nil {
		c.timer.Stop()
	}
	c.timer = time.AfterFunc(4*time.Second, func() { c.syncInBackground() })
}

func (c *cloudClient) setLinked(slug string) {
	c.mu.Lock()
	c.linked = slug
	c.mu.Unlock()
}

// afterUnlock syncs an online account as soon as it's unlocked.
func (c *cloudClient) afterUnlock() {
	acct := c.unlocked()
	if acct != nil && isOnlineAccount(acct.Slug) {
		c.setLinked(acct.Slug)
		go c.syncInBackground()
		c.startFeed(acct.Slug)
	}
}

func (c *cloudClient) syncInBackground() {
	if err := c.Sync(); err != nil {
		log.Printf("cloud: sync failed: %v", err)
	}
}

// signOut forgets the session (the account is being locked).
func (c *cloudClient) signOut() {
	c.mu.Lock()
	sess := c.sess
	c.sess = nil
	c.lastErr, c.gone, c.linked = "", false, ""
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.mu.Unlock()
	c.stopFeed()
	if sess != nil {
		go cloudCall(sess.server, "POST", "/logout", sess.token, map[string]string{}, nil)
		for i := range sess.accountKey {
			sess.accountKey[i] = 0
		}
	}
}

// accountOf reads a session's account details, which Status and Sync
// refresh while other operations are using the session.
func (c *cloudClient) accountOf(s *cloudSession) cloudAccount {
	c.mu.Lock()
	defer c.mu.Unlock()
	return s.account
}

// session signs in with the account key, or reuses the current session.
func (c *cloudClient) session(acct *Account, st *cloudState) (*cloudSession, error) {
	c.mu.Lock()
	if s := c.sess; s != nil && s.slug == acct.Slug && s.server == st.Server {
		c.mu.Unlock()
		return s, nil
	}
	c.mu.Unlock()
	pubHex := hex.EncodeToString(acct.PublicKey)
	nonce, sig, err := cloudSign(st.Server, acct.PrivateKey, "login", pubHex)
	if err != nil {
		return nil, err
	}
	var resp cloudLoginResp
	device, deviceName := thisDevice()
	err = cloudCall(st.Server, "POST", "/login/key", "", map[string]string{"public_key": pubHex, "nonce": nonce, "signature": sig,
		"device": device, "device_name": deviceName}, &resp)
	if cloudStatus(err) == http.StatusNotFound {
		c.mu.Lock()
		c.gone = true
		c.mu.Unlock()
		return nil, errors.New("this account isn't online any more — it was removed from chriscord.app (unverified accounts are removed after a week)")
	}
	if err != nil {
		return nil, err
	}
	key, err := UnwrapAccountKey(IdentityWrapKey(acct.PrivateKey.Seed()), resp.WrappedKey, pubHex, WrapIdentity)
	if err != nil {
		return nil, err
	}
	s := &cloudSession{slug: acct.Slug, server: st.Server, pubHex: pubHex, token: resp.Token, accountKey: key, account: resp.Account}
	c.mu.Lock()
	c.sess, c.gone = s, false
	c.mu.Unlock()
	return s, nil
}

// call makes an authenticated call, signing in again once if the session
// has expired.
func (c *cloudClient) call(acct *Account, st *cloudState, method, path string, body, out interface{}) error {
	sess, err := c.session(acct, st)
	if err != nil {
		return err
	}
	err = cloudCall(st.Server, method, path, sess.token, body, out)
	// An expired session (not a refused signature) is worth one more try:
	// the service checks the session before using up the request's nonce.
	if cloudStatus(err) == http.StatusUnauthorized {
		if msg := err.Error(); !strings.Contains(msg, "sign in") && !strings.Contains(msg, "sign-in") {
			return err
		}
		c.mu.Lock()
		c.sess = nil
		c.mu.Unlock()
		if sess, err = c.session(acct, st); err != nil {
			return err
		}
		err = cloudCall(st.Server, method, path, sess.token, body, out)
	}
	return err
}

// Sync fetches the vault, merges it with this computer's copy, writes
// whatever changed here, and uploads if anything changed there.
func (c *cloudClient) Sync() error {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	return c.syncLocked()
}

func (c *cloudClient) syncLocked() (err error) {
	acct := c.unlocked()
	if acct == nil {
		return errors.New("not signed in")
	}
	st, err := readCloudState(acct.Slug)
	if err != nil {
		return nil // not an online account
	}
	c.mu.Lock()
	c.syncing = true
	c.mu.Unlock()
	c.emit("cloud:status", nil)
	defer func() {
		c.mu.Lock()
		c.syncing = false
		if err != nil {
			c.lastErr = err.Error()
		} else {
			c.lastErr = ""
		}
		c.mu.Unlock()
		c.emit("cloud:status", nil)
	}()

	for attempt := 0; attempt < 4; attempt++ {
		sess, err := c.session(acct, st)
		if err != nil {
			return err
		}
		// The account's details first: a rename elsewhere shows up here.
		var view cloudAccount
		err = cloudCall(st.Server, "GET", "/account", sess.token, nil, &view)
		if err == nil {
			c.mu.Lock()
			sess.account = view
			c.mu.Unlock()
		}
		var version int64
		var blob []byte
		if err == nil {
			version, blob, err = cloudGetVault(st.Server, sess.token)
		}
		if cloudStatus(err) == http.StatusUnauthorized {
			c.mu.Lock()
			c.sess = nil
			c.mu.Unlock()
			continue
		}
		if err != nil {
			return err
		}
		if version < st.VaultVersion {
			return fmt.Errorf("chriscord.app sent an older copy of your data (version %d, this computer has seen %d) — not syncing", version, st.VaultVersion)
		}
		c.app.mu.Lock()
		if c.app.account == nil || c.app.account.Slug != acct.Slug {
			c.app.mu.Unlock()
			return errors.New("signed out")
		}
		local := localSnapshot(c.app.account, c.app.servers)
		c.app.mu.Unlock()

		remote := &vaultSnapshot{Doc: vaultDoc{Servers: []SavedServer{}}}
		if blob != nil {
			plain, err := OpenVault(sess.accountKey, sess.pubHex, version, blob)
			if err != nil {
				return err
			}
			if remote, err = unmarshalVault(plain); err != nil {
				return err
			}
			if remote.Doc.Seed != local.Doc.Seed {
				return errors.New("the synced copy belongs to a different account key — not syncing")
			}
		}
		res := mergeVault(local, remote, st.Base)
		c.applyLocal(acct.Slug, res)
		c.applyUsername(acct.Slug, c.accountOf(sess).Username)

		newVersion := version
		if blob == nil || !sameVault(res.merged, remote) {
			plain, err := res.merged.marshal()
			if err != nil {
				return err
			}
			sealed, err := SealVault(sess.accountKey, sess.pubHex, version+1, plain)
			if err != nil {
				return err
			}
			v, conflict, err := cloudPutVault(st.Server, sess.token, version, sealed)
			if err != nil {
				return err
			}
			if conflict {
				st.VaultVersion = version
				continue // another device got there first: merge with theirs
			}
			newVersion = v
		}
		st.VaultVersion, st.LastSync, st.Base = newVersion, time.Now().UTC(), baseOf(res.merged)
		st.Username = c.accountOf(sess).Username
		if err := writeCloudState(acct.Slug, st); err != nil {
			return err
		}
		if res.profile || res.pfp || res.settings || res.servers {
			log.Printf("cloud: synced (from elsewhere: profile=%v picture=%v appearance=%v servers=%v)", res.profile, res.pfp, res.settings, res.servers)
			c.emit("cloud:synced", map[string]bool{"profile": res.profile || res.pfp, "settings": res.settings, "servers": res.servers})
		}
		return nil
	}
	return errors.New("your data kept changing on another device — try again in a moment")
}

// applyLocal writes the parts of a merge that came from elsewhere.
func (c *cloudClient) applyLocal(slug string, res mergeResult) {
	if !(res.profile || res.pfp || res.settings || res.servers) {
		return
	}
	c.applying.Store(true)
	defer c.applying.Store(false)
	dir := filepath.Join(accountsDir(), slug)
	m := res.merged
	p := m.Doc.Profile
	if res.profile {
		if len(m.Banner) > 0 {
			os.WriteFile(bannerPath(dir), m.Banner, 0600)
		} else {
			os.Remove(bannerPath(dir))
		}
		updateProfileMeta(dir, p.Bio, p.Tint, p.Nickname, p.ProfileUpdatedAt)
	}
	if res.pfp {
		os.WriteFile(filepath.Join(dir, "pfp.png"), m.Pfp, 0600)
		updatePfpTimestamp(dir, p.PfpUpdatedAt)
	}
	if res.settings {
		os.WriteFile(filepath.Join(dir, clientSettingsFile), m.Doc.Settings, 0600)
		// Keep the vault's time, so this doesn't count as a newer local edit.
		if p := filepath.Join(dir, clientSettingsFile); m.Doc.SettingsAt > 0 {
			t := time.Unix(m.Doc.SettingsAt, 0)
			os.Chtimes(p, t, t)
		}
	}
	c.app.mu.Lock()
	if a := c.app.account; a != nil && a.Slug == slug {
		if res.profile {
			a.Bio, a.Tint, a.Nickname, a.ProfileUpdatedAt = p.Bio, p.Tint, p.Nickname, p.ProfileUpdatedAt
		}
		if res.pfp {
			a.HasAvatar, a.AvatarPath, a.PfpUpdatedAt = true, filepath.Join(dir, "pfp.png"), p.PfpUpdatedAt
		}
	}
	if res.servers {
		c.app.servers = append([]SavedServer{}, m.Doc.Servers...)
		saveServers(c.app.servers)
	}
	c.app.mu.Unlock()
	// Tell the server you're on, if any, so it fetches the new ones.
	if res.pfp {
		c.app.sendPfpInfo(p.PfpUpdatedAt)
	}
	if res.profile {
		c.app.sendProfileInfo(p.ProfileUpdatedAt)
	}
}

// applyUsername keeps the local name in step with the online one, so new
// servers you join see it. Servers you already joined keep the name you
// joined them with (connect retries with it).
func (c *cloudClient) applyUsername(slug, username string) {
	if username == "" {
		return
	}
	c.app.mu.Lock()
	defer c.app.mu.Unlock()
	a := c.app.account
	if a == nil || a.Slug != slug || a.Username == username {
		return
	}
	if err := setAccountUsername(filepath.Join(accountsDir(), slug), username); err != nil {
		log.Printf("cloud: couldn't save the new username here: %v", err)
		return
	}
	a.Username = username
}

// setAccountUsername changes just the username in account.json.
func setAccountUsername(dir, username string) error {
	m, err := readAccountMeta(dir)
	if err != nil {
		m = &accountMeta{CreatedAt: time.Now().UTC()}
	}
	m.Username = username
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "account.json"), data, 0600)
}

// ── Making an account online, and the ways back in ─────────────────────────

type cloudSecrets struct {
	kdf             CloudKDF
	authKey         []byte
	wrappedPassword string
}

// passwordSecrets stretches a new password and wraps accountKey under it.
func passwordSecrets(password, pubHex string, accountKey []byte) (*cloudSecrets, error) {
	if len([]rune(password)) < cloudMinPassword {
		return nil, fmt.Errorf("use at least %d characters for your password", cloudMinPassword)
	}
	kdf, err := NewCloudKDF()
	if err != nil {
		return nil, err
	}
	auth, wrap, err := kdf.PasswordKeys(password)
	if err != nil {
		return nil, err
	}
	wrapped, err := WrapAccountKey(wrap, accountKey, pubHex, WrapPassword)
	if err != nil {
		return nil, err
	}
	return &cloudSecrets{kdf: kdf, authKey: auth, wrappedPassword: wrapped}, nil
}

// newRecovery makes a recovery key and wraps accountKey under it.
func newRecovery(pubHex string, accountKey []byte) (shown, authHex, wrapped string, err error) {
	raw, shown, err := NewRecoveryKey()
	if err != nil {
		return "", "", "", err
	}
	auth, wrap := RecoveryKeys(raw)
	wrapped, err = WrapAccountKey(wrap, accountKey, pubHex, WrapRecovery)
	return shown, hex.EncodeToString(auth), wrapped, err
}

// Link makes the unlocked account online. Returns the recovery key, to be
// shown once.
func (c *cloudClient) Link(username, email, password string) (string, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	acct := c.unlocked()
	if acct == nil {
		return "", errors.New("not signed in")
	}
	if isOnlineAccount(acct.Slug) {
		return "", errors.New("this account is already online")
	}
	username = strings.TrimSpace(username)
	if err := ValidCloudUsername(username); err != nil {
		return "", err
	}
	email, err := NormalizeEmail(email)
	if err != nil {
		return "", err
	}
	server := cloudServerURL()
	pubHex := hex.EncodeToString(acct.PublicKey)
	accountKey := make([]byte, 32)
	if _, err := rand.Read(accountKey); err != nil {
		return "", err
	}
	pw, err := passwordSecrets(password, pubHex, accountKey)
	if err != nil {
		return "", err
	}
	recShown, recAuth, recWrapped, err := newRecovery(pubHex, accountKey)
	if err != nil {
		return "", err
	}
	idWrapped, err := WrapAccountKey(IdentityWrapKey(acct.PrivateKey.Seed()), accountKey, pubHex, WrapIdentity)
	if err != nil {
		return "", err
	}
	authHex := hex.EncodeToString(pw.authKey)
	fields := []string{username, email, pubHex, pw.kdf.String(), authHex, recAuth, pw.wrappedPassword, recWrapped, idWrapped}
	nonce, sig, err := cloudSign(server, acct.PrivateKey, "register", fields...)
	if err != nil {
		return "", err
	}
	claim := hex.EncodeToString(ed25519.Sign(acct.PrivateKey, CloudSigMessage("claim", nonce, pubHex, username)))
	var resp struct {
		Token   string       `json:"token"`
		Account cloudAccount `json:"account"`
	}
	device, deviceName := thisDevice()
	err = cloudCall(server, "POST", "/register", "", map[string]interface{}{
		"username": username, "email": email, "public_key": pubHex, "nonce": nonce, "signature": sig, "claim_signature": claim,
		"kdf": pw.kdf, "auth_key": authHex, "recovery_auth": recAuth,
		"wrapped_password": pw.wrappedPassword, "wrapped_recovery": recWrapped, "wrapped_identity": idWrapped,
		"device": device, "device_name": deviceName,
	}, &resp)
	if err != nil {
		return "", err
	}
	if err := writeCloudState(acct.Slug, &cloudState{Server: server, Username: resp.Account.Username, LinkedAt: time.Now().UTC()}); err != nil {
		return "", err
	}
	c.mu.Lock()
	c.sess = &cloudSession{slug: acct.Slug, server: server, pubHex: pubHex, token: resp.Token, accountKey: accountKey, account: resp.Account}
	c.gone, c.linked = false, acct.Slug
	c.mu.Unlock()
	log.Printf("cloud: %s is now online as %s", acct.Slug, resp.Account.Username)
	if err := c.syncLocked(); err != nil {
		log.Printf("cloud: first upload failed (will retry): %v", err)
	}
	c.startFeed(acct.Slug)
	return recShown, nil
}

// openRemote fetches and opens the vault with accountKey, returning it and
// the key pair inside, checked against the account's public key.
func openRemote(server, token string, acc cloudAccount, accountKey []byte) (*vaultSnapshot, ed25519.PrivateKey, error) {
	version, blob, err := cloudGetVault(server, token)
	if err != nil {
		return nil, nil, err
	}
	if blob == nil {
		return nil, nil, errors.New("this account hasn't finished its first sync — sign in on the computer it was made on once more, then try again")
	}
	plain, err := OpenVault(accountKey, acc.PublicKey, version, blob)
	if err != nil {
		return nil, nil, err
	}
	v, err := unmarshalVault(plain)
	if err != nil {
		return nil, nil, err
	}
	priv, err := v.privateKey()
	if err != nil {
		return nil, nil, err
	}
	if hex.EncodeToString(priv.Public().(ed25519.PublicKey)) != acc.PublicKey {
		return nil, nil, errors.New("the synced account key doesn't match this account — not using it")
	}
	return v, priv, nil
}

// adopt makes an account fetched from chriscord.app the one in use here,
// writing it to disk first if this computer hasn't had it. passphrase
// protects the key on this computer; rekey replaces an existing local
// passphrase with it (used after recovery, when the old one may be lost
// too).
func (c *cloudClient) adopt(server string, sess *cloudSession, priv ed25519.PrivateKey, passphrase string, rekey bool) (*Account, error) {
	pub := priv.Public().(ed25519.PublicKey)
	slug := fingerprintOf(pub)
	dir := filepath.Join(accountsDir(), slug)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	_, statErr := os.Stat(filepath.Join(dir, "identity.json"))
	if statErr != nil || rekey {
		if err := writeEncryptedIdentity(dir, pub, priv.Seed(), passphrase); err != nil {
			return nil, err
		}
		if rekey {
			forgetRemembered(slug)
		}
	}
	if _, err := readAccountMeta(dir); err != nil {
		if err := writeAccountMeta(dir, sess.account.Username); err != nil {
			return nil, err
		}
	}
	st, err := readCloudState(slug)
	if err != nil || st.Server != server {
		st = &cloudState{Server: server, LinkedAt: time.Now().UTC()}
	}
	st.Username = sess.account.Username
	if err := writeCloudState(slug, st); err != nil {
		return nil, err
	}
	if err := setActiveSlug(slug); err != nil {
		return nil, err
	}
	acct := accountFromKeys(slug, pub, priv)
	c.app.mu.Lock()
	c.app.account = acct
	c.app.servers = loadServers()
	c.app.mu.Unlock()
	// The password (or recovery) session only fetched the vault; syncing
	// signs in again with the account key, the only kind of session that
	// can upload.
	go cloudCall(server, "POST", "/logout", sess.token, map[string]string{}, nil)
	c.mu.Lock()
	c.sess, c.gone, c.lastErr, c.linked = nil, false, "", slug
	c.mu.Unlock()
	if err := c.syncLocked(); err != nil {
		log.Printf("cloud: sync after sign-in failed: %v", err)
	}
	c.startFeed(slug)
	return acct, nil
}

// SignIn brings an online account to this computer with its password.
// The password also becomes this computer's passphrase for it.
func (c *cloudClient) SignIn(username, password string) (*Account, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if c.unlocked() != nil {
		return nil, errors.New("sign out first")
	}
	username = strings.TrimSpace(username)
	server := cloudServerURL()
	kdf, err := cloudPrelogin(server, username)
	if err != nil {
		return nil, err
	}
	auth, wrap, err := kdf.PasswordKeys(password)
	if err != nil {
		return nil, err
	}
	var resp cloudLoginResp
	device, deviceName := thisDevice()
	if err := cloudCall(server, "POST", "/login", "", map[string]string{"username": username, "auth_key": hex.EncodeToString(auth),
		"device": device, "device_name": deviceName}, &resp); err != nil {
		return nil, err
	}
	if resp.Approval != "" {
		// First time on this computer: wait for the email's link.
		if resp, err = c.waitApproval(server, resp, device); err != nil {
			return nil, err
		}
	}
	accountKey, err := UnwrapAccountKey(wrap, resp.WrappedKey, resp.Account.PublicKey, WrapPassword)
	if err != nil {
		return nil, err
	}
	_, priv, err := openRemote(server, resp.Token, resp.Account, accountKey)
	if err != nil {
		return nil, err
	}
	sess := &cloudSession{server: server, pubHex: resp.Account.PublicKey, token: resp.Token, accountKey: accountKey, account: resp.Account}
	return c.adopt(server, sess, priv, password, false)
}

// CloudApproval is what the page shows while a sign-in waits for its email.
type CloudApproval struct {
	Email     string `json:"email"`
	ExpiresIn int    `json:"expires_in"`
}

// waitApproval waits for a new-computer sign-in to be confirmed from the
// email chriscord.app sent (or turned down, or the time to run out, or
// CancelSignIn). The page is told so it can say where to look.
func (c *cloudClient) waitApproval(server string, first cloudLoginResp, device string) (cloudLoginResp, error) {
	cancel := make(chan struct{})
	c.mu.Lock()
	c.approvalCancel = cancel
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.approvalCancel == cancel {
			c.approvalCancel = nil
		}
		c.mu.Unlock()
		c.emit("cloud:approval", nil)
	}()
	c.emit("cloud:approval", CloudApproval{Email: first.Email, ExpiresIn: first.ExpiresIn})
	deadline := time.Now().Add(time.Duration(first.ExpiresIn+30) * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-cancel:
			return cloudLoginResp{}, errors.New("sign-in cancelled")
		case <-time.After(3 * time.Second):
		}
		var resp cloudLoginResp
		err := cloudCall(server, "POST", "/login/approval", "", map[string]string{"approval": first.Approval, "device": device}, &resp)
		if err != nil {
			if cloudStatus(err) == 0 {
				continue // a network blip: keep waiting
			}
			return cloudLoginResp{}, err
		}
		if resp.Token != "" {
			return resp, nil
		}
	}
	return cloudLoginResp{}, errors.New("the email wasn't confirmed in time — sign in again for a new one")
}

// CancelSignIn stops waiting for a new-computer confirmation.
func (c *cloudClient) CancelSignIn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.approvalCancel != nil {
		close(c.approvalCancel)
		c.approvalCancel = nil
	}
}

// CloudDevice is a computer the online account has been used on.
type CloudDevice struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	AddedAt  int64  `json:"added_at"`
	LastSeen int64  `json:"last_seen"`
}

// Devices lists the computers the online account knows.
func (c *cloudClient) Devices() ([]CloudDevice, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	acct, st, _, err := c.online()
	if err != nil {
		return nil, err
	}
	var out struct {
		Devices []CloudDevice `json:"devices"`
	}
	if err := c.call(acct, st, "GET", "/account/devices", nil, &out); err != nil {
		return nil, err
	}
	if out.Devices == nil {
		out.Devices = []CloudDevice{}
	}
	return out.Devices, nil
}

// RemoveDevice forgets a computer: a password sign-in there has to be
// confirmed by email again.
func (c *cloudClient) RemoveDevice(id int64) error {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	acct, st, _, err := c.online()
	if err != nil {
		return err
	}
	return c.call(acct, st, "POST", "/account/devices/remove", map[string]int64{"id": id}, nil)
}

// Recover signs in with the recovery key, sets a new password, and makes
// a new recovery key (returned, to be shown once). The new password also
// becomes this computer's passphrase for the account.
func (c *cloudClient) Recover(username, recoveryKey, newPassword string) (*Account, string, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if c.unlocked() != nil {
		return nil, "", errors.New("sign out first")
	}
	if len([]rune(newPassword)) < cloudMinPassword {
		return nil, "", fmt.Errorf("use at least %d characters for your new password", cloudMinPassword)
	}
	raw, err := ParseRecoveryKey(recoveryKey)
	if err != nil {
		return nil, "", err
	}
	recAuth, recWrap := RecoveryKeys(raw)
	server := cloudServerURL()
	var resp cloudLoginResp
	device, deviceName := thisDevice()
	if err := cloudCall(server, "POST", "/login/recovery", "", map[string]string{"username": strings.TrimSpace(username), "recovery_auth": hex.EncodeToString(recAuth),
		"device": device, "device_name": deviceName}, &resp); err != nil {
		return nil, "", err
	}
	accountKey, err := UnwrapAccountKey(recWrap, resp.WrappedKey, resp.Account.PublicKey, WrapRecovery)
	if err != nil {
		return nil, "", err
	}
	_, priv, err := openRemote(server, resp.Token, resp.Account, accountKey)
	if err != nil {
		return nil, "", err
	}
	pubHex := resp.Account.PublicKey

	// New password, approved by the account key, proven by the recovery key.
	pw, err := passwordSecrets(newPassword, pubHex, accountKey)
	if err != nil {
		return nil, "", err
	}
	authHex := hex.EncodeToString(pw.authKey)
	nonce, sig, err := cloudSign(server, priv, "password", pubHex, pw.kdf.String(), authHex, pw.wrappedPassword)
	if err != nil {
		return nil, "", err
	}
	if err := cloudCall(server, "POST", "/account/password", resp.Token, map[string]interface{}{
		"kdf": pw.kdf, "auth_key": authHex, "wrapped_password": pw.wrappedPassword,
		"recovery_auth": hex.EncodeToString(recAuth), "nonce": nonce, "signature": sig,
	}, nil); err != nil {
		return nil, "", err
	}
	// The old recovery key has been used: replace it.
	shown, err := c.replaceRecovery(server, resp.Token, priv, pubHex, accountKey, authHex)
	if err != nil {
		log.Printf("cloud: recovered, but making a new recovery key failed: %v", err)
		shown = ""
	}
	sess := &cloudSession{server: server, pubHex: pubHex, token: resp.Token, accountKey: accountKey, account: resp.Account}
	acct, err := c.adopt(server, sess, priv, newPassword, true)
	return acct, shown, err
}

func (c *cloudClient) replaceRecovery(server, token string, priv ed25519.PrivateKey, pubHex string, accountKey []byte, authHex string) (string, error) {
	shown, recAuth, recWrapped, err := newRecovery(pubHex, accountKey)
	if err != nil {
		return "", err
	}
	nonce, sig, err := cloudSign(server, priv, "recovery", pubHex, recAuth, recWrapped)
	if err != nil {
		return "", err
	}
	err = cloudCall(server, "POST", "/account/recovery", token, map[string]string{
		"auth_key": authHex, "recovery_auth": recAuth, "wrapped_recovery": recWrapped, "nonce": nonce, "signature": sig,
	}, nil)
	return shown, err
}

// ── Changes to an online account (signed in) ───────────────────────────────

// online returns the unlocked account, its cloud state and a session.
func (c *cloudClient) online() (*Account, *cloudState, *cloudSession, error) {
	acct := c.unlocked()
	if acct == nil {
		return nil, nil, nil, errors.New("not signed in")
	}
	st, err := readCloudState(acct.Slug)
	if err != nil {
		return nil, nil, nil, errors.New("this account isn't online")
	}
	sess, err := c.session(acct, st)
	if err != nil {
		return nil, nil, nil, err
	}
	return acct, st, sess, nil
}

// currentAuth derives the current password's login key.
func currentAuth(server, username, password string) (string, error) {
	kdf, err := cloudPrelogin(server, username)
	if err != nil {
		return "", err
	}
	auth, _, err := kdf.PasswordKeys(password)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(auth), nil
}

func (c *cloudClient) ChangeUsername(name string) error {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	name = strings.TrimSpace(name)
	if err := ValidCloudUsername(name); err != nil {
		return err
	}
	acct, st, sess, err := c.online()
	if err != nil {
		return err
	}
	nonce, sig, err := cloudSign(st.Server, acct.PrivateKey, "claim", sess.pubHex, name)
	if err != nil {
		return err
	}
	var view cloudAccount
	if err := c.call(acct, st, "POST", "/account/username", map[string]string{"username": name, "nonce": nonce, "signature": sig}, &view); err != nil {
		return err
	}
	c.mu.Lock()
	if c.sess != nil {
		c.sess.account = view
	}
	c.mu.Unlock()
	c.applyUsername(acct.Slug, view.Username)
	st.Username = view.Username
	writeCloudState(acct.Slug, st)
	return nil
}

func (c *cloudClient) ChangePassword(current, next string) error {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	acct, st, sess, err := c.online()
	if err != nil {
		return err
	}
	oldAuth, err := currentAuth(st.Server, c.accountOf(sess).Username, current)
	if err != nil {
		return err
	}
	pw, err := passwordSecrets(next, sess.pubHex, sess.accountKey)
	if err != nil {
		return err
	}
	authHex := hex.EncodeToString(pw.authKey)
	nonce, sig, err := cloudSign(st.Server, acct.PrivateKey, "password", sess.pubHex, pw.kdf.String(), authHex, pw.wrappedPassword)
	if err != nil {
		return err
	}
	return c.call(acct, st, "POST", "/account/password", map[string]interface{}{
		"kdf": pw.kdf, "auth_key": authHex, "wrapped_password": pw.wrappedPassword,
		"old_auth_key": oldAuth, "nonce": nonce, "signature": sig,
	}, nil)
}

func (c *cloudClient) NewRecoveryKey(password string) (string, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	acct, st, sess, err := c.online()
	if err != nil {
		return "", err
	}
	authHex, err := currentAuth(st.Server, c.accountOf(sess).Username, password)
	if err != nil {
		return "", err
	}
	return c.replaceRecovery(st.Server, sess.token, acct.PrivateKey, sess.pubHex, sess.accountKey, authHex)
}

func (c *cloudClient) ChangeEmail(email string) error {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	email, err := NormalizeEmail(email)
	if err != nil {
		return err
	}
	acct, st, sess, err := c.online()
	if err != nil {
		return err
	}
	nonce, sig, err := cloudSign(st.Server, acct.PrivateKey, "email", sess.pubHex, email)
	if err != nil {
		return err
	}
	return c.call(acct, st, "POST", "/account/email", map[string]string{"email": email, "nonce": nonce, "signature": sig}, nil)
}

func (c *cloudClient) ResendVerification() error {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	acct, st, _, err := c.online()
	if err != nil {
		return err
	}
	return c.call(acct, st, "POST", "/account/verify/resend", map[string]string{}, nil)
}

// Unlink deletes the online account; the account on this computer stays
// as it is, offline.
func (c *cloudClient) Unlink(password string) error {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	acct, st, sess, err := c.online()
	if err != nil {
		return err
	}
	authHex, err := currentAuth(st.Server, c.accountOf(sess).Username, password)
	if err != nil {
		return err
	}
	nonce, sig, err := cloudSign(st.Server, acct.PrivateKey, "delete", sess.pubHex)
	if err != nil {
		return err
	}
	if err := c.call(acct, st, "POST", "/account/delete", map[string]string{"auth_key": authHex, "nonce": nonce, "signature": sig}, nil); err != nil {
		return err
	}
	c.forget(acct.Slug)
	return nil
}

// forget makes the account offline on this computer only (used when the
// service no longer has it).
func (c *cloudClient) forget(slug string) {
	os.Remove(cloudStatePath(slug))
	c.mu.Lock()
	c.sess, c.gone, c.lastErr, c.linked = nil, false, "", ""
	c.mu.Unlock()
	c.stopFeed()
	c.emit("cloud:status", nil)
}

// CloudStatusView is what Settings → Account shows.
type CloudStatusView struct {
	Online        bool   `json:"online"` // this account is linked to chriscord.app
	Server        string `json:"server"`
	Username      string `json:"username"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	VerifyBy      int64  `json:"verify_by"`
	LastSync      int64  `json:"last_sync"`
	VaultSize     int64  `json:"vault_size"`
	Syncing       bool   `json:"syncing"`
	Error         string `json:"error"`
	Gone          bool   `json:"gone"`        // the service doesn't have it any more
	LogSeq        int64  `json:"log_seq"`     // how far this app has checked the directory's public log
	LogProblem    string `json:"log_problem"` // why that log can't be trusted, if it can't
	LocalUsername string `json:"local_username"`
}

func (c *cloudClient) Status(refresh bool) CloudStatusView {
	acct := c.unlocked()
	v := CloudStatusView{Server: cloudServerURL()}
	if acct == nil {
		return v
	}
	v.LocalUsername = acct.Username
	st, err := readCloudState(acct.Slug)
	if err != nil {
		return v
	}
	v.Online, v.Server, v.Username = true, st.Server, st.Username
	if !st.LastSync.IsZero() {
		v.LastSync = st.LastSync.Unix()
	}
	if refresh {
		var view cloudAccount
		if err := c.call(acct, st, "GET", "/account", nil, &view); err == nil {
			c.mu.Lock()
			if c.sess != nil {
				c.sess.account = view
			}
			c.mu.Unlock()
		} else {
			c.mu.Lock()
			c.lastErr = err.Error()
			c.mu.Unlock()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.sess; s != nil && s.slug == acct.Slug {
		a := s.account
		v.Username, v.Email, v.EmailVerified, v.VerifyBy, v.VaultSize = a.Username, a.Email, a.EmailVerified, a.VerifyBy, a.VaultSize
	}
	v.Syncing, v.Error, v.Gone = c.syncing, c.lastErr, c.gone
	v.LogSeq, v.LogProblem = c.keys.log.Seq(), c.keys.log.Problem()
	return v
}

// ── Bound to the frontend ──────────────────────────────────────────────────

// CloudStatus asks chriscord.app for the account's details;
// CloudStatusQuick only reports what this app already knows.
func (a *App) CloudStatus() CloudStatusView      { return a.cloud.Status(true) }
func (a *App) CloudStatusQuick() CloudStatusView { return a.cloud.Status(false) }

// CloudLink makes the unlocked account online; returns the recovery key.
func (a *App) CloudLink(username, email, password string) (string, error) {
	return a.cloud.Link(username, email, password)
}

// CloudSignIn signs in to an online account on this computer (from the
// sign-in screen).
func (a *App) CloudSignIn(username, password string) (*AccountView, error) {
	acct, err := a.cloud.SignIn(username, password)
	if err != nil {
		return nil, err
	}
	return acct.View(), nil
}

// CloudRecoverResult is the account signed in to, and its new recovery key.
type CloudRecoverResult struct {
	Account     *AccountView `json:"account"`
	RecoveryKey string       `json:"recovery_key"`
}

func (a *App) CloudRecover(username, recoveryKey, newPassword string) (*CloudRecoverResult, error) {
	acct, shown, err := a.cloud.Recover(username, recoveryKey, newPassword)
	if err != nil {
		return nil, err
	}
	return &CloudRecoverResult{Account: acct.View(), RecoveryKey: shown}, nil
}

func (a *App) CloudSyncNow() error                   { return a.cloud.Sync() }
func (a *App) CloudChangeUsername(name string) error { return a.cloud.ChangeUsername(name) }
func (a *App) CloudChangePassword(current, next string) error {
	return a.cloud.ChangePassword(current, next)
}
func (a *App) CloudNewRecoveryKey(password string) (string, error) {
	return a.cloud.NewRecoveryKey(password)
}
func (a *App) CloudChangeEmail(email string) error  { return a.cloud.ChangeEmail(email) }
func (a *App) CloudResendVerification() error       { return a.cloud.ResendVerification() }
func (a *App) CloudCancelSignIn()                   { a.cloud.CancelSignIn() }
func (a *App) CloudDevices() ([]CloudDevice, error) { return a.cloud.Devices() }
func (a *App) CloudRemoveDevice(id int64) error     { return a.cloud.RemoveDevice(id) }
func (a *App) CloudUnlink(password string) error    { return a.cloud.Unlink(password) }

// CloudForget makes this account offline on this computer without asking
// chriscord.app (for when it's already gone from there).
func (a *App) CloudForget() error {
	acct := a.cloud.unlocked()
	if acct == nil {
		return errors.New("not signed in")
	}
	a.cloud.forget(acct.Slug)
	return nil
}

// registeredAs reads the name out of a server's "This key is registered as
// 'name'. Use that username." (chriscord-server auth.rs).
func registeredAs(msg string) string {
	const prefix = "This key is registered as '"
	i := strings.Index(msg, prefix)
	if i < 0 {
		return ""
	}
	rest := msg[i+len(prefix):]
	j := strings.Index(rest, "'. ")
	if j < 0 {
		j = strings.LastIndex(rest, "'")
	}
	if j <= 0 {
		return ""
	}
	return rest[:j]
}

// ── The live change feed ───────────────────────────────────────────────────
//
// While an online account is unlocked, the app keeps one connection open
// to chriscord.app's event stream (GET /vault/events). When another
// computer uploads, the service says "the vault is at version N" and this
// app syncs — so changes show up within a second or two, without
// checking on a timer. The events carry nothing readable. If the
// connection drops, it comes back with a growing wait (2s up to 2 min),
// and the first event on every connection is the current version, so
// nothing is missed while it was down.

// feedHTTP has no overall timeout: the stream stays open.
var feedHTTP = &http.Client{}

func (c *cloudClient) startFeed(slug string) {
	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	if c.feedStop != nil {
		c.feedStop()
	}
	c.feedStop = cancel
	c.mu.Unlock()
	go c.runFeed(ctx, slug)
}

func (c *cloudClient) stopFeed() {
	c.mu.Lock()
	if c.feedStop != nil {
		c.feedStop()
		c.feedStop = nil
	}
	c.mu.Unlock()
}

func (c *cloudClient) runFeed(ctx context.Context, slug string) {
	if s := c.keys.server(); s != "" {
		c.keys.log.sync(s, true) // bring the directory log up to date (translog.go)
	}
	wait := 2 * time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := c.feedOnce(ctx, slug)
		if ctx.Err() != nil {
			return
		}
		c.mu.Lock()
		gone := c.gone
		c.mu.Unlock()
		if gone {
			return // deleted online: nothing more will come
		}
		if time.Since(started) > time.Minute {
			wait = 2 * time.Second // it was up a good while: just reconnect
		}
		if err != nil {
			log.Printf("cloud: change feed: %v (again in %s)", err, wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait *= 2; wait > 2*time.Minute {
			wait = 2 * time.Minute
		}
	}
}

// feedOnce holds one connection to the event stream until it ends.
func (c *cloudClient) feedOnce(ctx context.Context, slug string) error {
	acct := c.unlocked()
	if acct == nil || acct.Slug != slug {
		return errors.New("signed out")
	}
	st, err := readCloudState(slug)
	if err != nil {
		return errors.New("not online")
	}
	sess, err := c.session(acct, st)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", st.Server+"/api/v1/vault/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+sess.token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := feedHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		c.mu.Lock()
		if c.sess == sess {
			c.sess = nil // expired: sign in again next time round
		}
		c.mu.Unlock()
		return errors.New("session expired")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the service answered %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 4096), 64<<10)
	var event, data string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(line[len("data:"):])
		case line == "":
			if event != "" {
				c.onFeedEvent(slug, event, data)
			}
			event, data = "", ""
		}
	}
	return sc.Err()
}

func (c *cloudClient) onFeedEvent(slug, event, data string) {
	switch event {
	case "vault":
		var v struct {
			Version int64 `json:"version"`
		}
		json.Unmarshal([]byte(data), &v)
		if st, err := readCloudState(slug); err == nil && v.Version <= st.VaultVersion {
			return // our own upload, or nothing new
		}
		c.syncInBackground()
	case "account":
		c.syncInBackground() // picks up a rename and refreshes Settings → Account
	case "deleted":
		c.mu.Lock()
		c.gone = true
		c.mu.Unlock()
		c.emit("cloud:status", nil)
	}
}

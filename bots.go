package main

// bots.go — making bots, and adding them to servers.
//
// Settings → Bots: a bot is made here, by your account. It gets its own
// new key and a certificate your key signs ("this key is my bot", with its
// name and description). The app shows its token once — the bot's key and
// certificate in one string, for whoever runs the bot — and an "Add to
// server" link, which carries only the certificate and what it asks for.
// The bot's key isn't kept here; what's kept (accounts/<you>/bots.json) is
// the certificate, so the app can show the link again and revoke the bot.
//
// Revoking signs a revocation with your key and hands it to every server
// you've saved: the ones that have the bot drop it and refuse it from then
// on.
//
// Server → Bots: someone with Manage Bots adds a bot from its link,
// choosing what it may do and which channels it sees, and can change or
// remove bots later. The server checks everything again (bots.rs).

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const botsFile = "bots.json"

// BotTag is what the server says about a bot in its member list.
type BotTag struct {
	MakerKey    string `json:"maker_key"`
	Maker       string `json:"maker"` // the maker's name on the server, if they're a member
	Name        string `json:"name"`  // its own name (what it's called there may differ)
	Description string `json:"description"`
}

// MyBot is a bot made by this account.
type MyBot struct {
	Cert        BotCert `json:"cert"`
	Permissions uint64  `json:"permissions"` // what its Add to server link asks for
	RevokedAt   int64   `json:"revoked_at,omitempty"`
	// Filled in for the page.
	AddLink     string `json:"add_link,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

var botsMu sync.Mutex

func (a *App) botsPath() (string, ed25519.PrivateKey, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.account == nil {
		return "", nil, errors.New("no account unlocked")
	}
	return filepath.Join(accountsDir(), a.account.Slug, botsFile), a.account.PrivateKey, nil
}

func readBots(path string) ([]MyBot, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []MyBot{}, nil
	}
	if err != nil {
		return nil, err
	}
	var bots []MyBot
	if err := json.Unmarshal(data, &bots); err != nil {
		return nil, fmt.Errorf("%s is damaged: %w", path, err)
	}
	return bots, nil
}

func writeBots(path string, bots []MyBot) error {
	for i := range bots {
		bots[i].AddLink, bots[i].Fingerprint = "", ""
	}
	data, err := json.MarshalIndent(bots, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func dressBot(b MyBot) MyBot {
	b.AddLink = BotAddLink(&b.Cert, b.Permissions)
	b.Fingerprint = groupedFingerprint(b.Cert.BotKey)
	return b
}

// ListMyBots lists the bots this account has made, newest first.
func (a *App) ListMyBots() ([]MyBot, error) {
	path, _, err := a.botsPath()
	if err != nil {
		return nil, err
	}
	botsMu.Lock()
	defer botsMu.Unlock()
	bots, err := readBots(path)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(bots, func(i, j int) bool { return bots[i].Cert.Created > bots[j].Cert.Created })
	for i := range bots {
		bots[i] = dressBot(bots[i])
	}
	return bots, nil
}

// NewBotResult is a freshly made bot, with its token (shown once).
type NewBotResult struct {
	Bot   MyBot  `json:"bot"`
	Token string `json:"token"`
}

// CreateBot makes a bot: a new key, its certificate signed by this
// account, and its token. permissions is what its link asks for.
func (a *App) CreateBot(name, description string, permissions uint64) (*NewBotResult, error) {
	path, maker, err := a.botsPath()
	if err != nil {
		return nil, err
	}
	name, description = strings.TrimSpace(name), strings.TrimSpace(description)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	cert, err := NewBotCert(maker, pub, name, description, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	token, err := EncodeBotToken(key, cert)
	if err != nil {
		return nil, err
	}
	clear(key) // not kept: the token is the only copy
	botsMu.Lock()
	defer botsMu.Unlock()
	bots, err := readBots(path)
	if err != nil {
		return nil, err
	}
	b := MyBot{Cert: *cert, Permissions: permissions &^ botNever}
	bots = append(bots, b)
	if err := writeBots(path, bots); err != nil {
		return nil, err
	}
	return &NewBotResult{Bot: dressBot(b), Token: token}, nil
}

// Never given to a bot (the server refuses them anyway): Administrator and
// Manage Bots.
const botNever = 1<<8 | 1<<24

// RevokeResult says which servers were told.
type RevokeResult struct {
	Dropped []string `json:"dropped"` // had the bot, and dropped it
	Told    int      `json:"told"`    // servers reached
	Failed  []string `json:"failed"`  // couldn't be reached (they'll refuse it once told)
}

// RevokeBot retires a bot for good: a revocation signed by this account,
// given to every saved server.
func (a *App) RevokeBot(botKey string) (*RevokeResult, error) {
	path, maker, err := a.botsPath()
	if err != nil {
		return nil, err
	}
	botKey = strings.ToLower(botKey)
	botsMu.Lock()
	bots, err := readBots(path)
	if err != nil {
		botsMu.Unlock()
		return nil, err
	}
	found := false
	for i := range bots {
		if bots[i].Cert.BotKey == botKey {
			found = true
			if bots[i].RevokedAt == 0 {
				bots[i].RevokedAt = time.Now().Unix()
			}
		}
	}
	if !found {
		botsMu.Unlock()
		return nil, errors.New("that isn't one of your bots")
	}
	err = writeBots(path, bots)
	botsMu.Unlock()
	if err != nil {
		return nil, err
	}
	rev := NewBotRevocation(maker, botKey, time.Now().Unix())
	a.mu.Lock()
	var domains []string
	for _, s := range a.servers {
		domains = append(domains, s.Domain)
	}
	a.mu.Unlock()
	res := &RevokeResult{Dropped: []string{}, Failed: []string{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, d := range domains {
		wg.Add(1)
		go func(d string) {
			defer wg.Done()
			var r struct {
				Here bool `json:"here"`
			}
			err := postJSON(normaliseHTTP(d)+"/api/bots/revoke", rev, &r)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				res.Told++
				if r.Here {
					res.Dropped = append(res.Dropped, d)
				}
			case strings.Contains(err.Error(), "HTTP 404") || strings.Contains(err.Error(), "HTTP 405"):
				res.Told++ // a server without bots: nothing to drop
			default:
				res.Failed = append(res.Failed, d)
			}
		}(d)
	}
	wg.Wait()
	sort.Strings(res.Dropped)
	sort.Strings(res.Failed)
	return res, nil
}

// ForgetBot takes a revoked bot off the list.
func (a *App) ForgetBot(botKey string) error {
	path, _, err := a.botsPath()
	if err != nil {
		return err
	}
	botsMu.Lock()
	defer botsMu.Unlock()
	bots, err := readBots(path)
	if err != nil {
		return err
	}
	out := bots[:0]
	for _, b := range bots {
		if b.Cert.BotKey == strings.ToLower(botKey) {
			if b.RevokedAt == 0 {
				return errors.New("revoke the bot first")
			}
			continue
		}
		out = append(out, b)
	}
	return writeBots(path, out)
}

// ── On a server ──────────────────────────────────────────────────────────────

// BotLinkInfo is an Add to server link, read and checked.
type BotLinkInfo struct {
	Cert        BotCert `json:"cert"`
	Permissions uint64  `json:"permissions"`
	Fingerprint string  `json:"fingerprint"`       // the bot's key
	MakerFP     string  `json:"maker_fingerprint"` // its maker's key
	MakerHere   string  `json:"maker_here"`        // the maker's name on this server, if they're a member you've seen
	MakerName   string  `json:"maker_name"`        // the maker's chriscord.app name, when verified
	MakerCheck  string  `json:"maker_check"`       // verified | unlisted | problem | error | off
	MakerNote   string  `json:"maker_note"`
	Mine        bool    `json:"mine"` // you made it
}

// ReadBotLink checks an Add to server link and says who made the bot.
func (a *App) ReadBotLink(link string) (*BotLinkInfo, error) {
	r, err := ParseBotAddLink(link)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	domain := a.domain
	mine := a.account != nil && hex.EncodeToString(a.account.PublicKey) == r.Cert.MakerKey
	a.mu.Unlock()
	info := &BotLinkInfo{Cert: r.Cert, Permissions: r.Permissions &^ botNever, Fingerprint: groupedFingerprint(r.Cert.BotKey),
		MakerFP: groupedFingerprint(r.Cert.MakerKey), Mine: mine, MakerCheck: "off"}
	if domain != "" {
		info.MakerHere = pinnedUserOf(domain, r.Cert.MakerKey)
	}
	if a.cloud != nil && a.cloud.keys.server() != "" {
		v := a.cloud.keys.verdict(r.Cert.MakerKey, keyCheckWaitUI)
		info.MakerCheck, info.MakerName, info.MakerNote = v.Status, v.Name, v.Note
		if v.Status == "problem" {
			info.MakerNote = v.Problem
		}
	}
	return info, nil
}

// MakerIdentity is what the directory says about a bot's maker (for "Made
// by" on its profile card).
func (a *App) MakerIdentity(makerKey string) (*BotLinkInfo, error) {
	makerKey = strings.ToLower(makerKey)
	if !isHexKey(makerKey) {
		return nil, errors.New("not a key")
	}
	a.mu.Lock()
	domain := a.domain
	mine := a.account != nil && hex.EncodeToString(a.account.PublicKey) == makerKey
	a.mu.Unlock()
	info := &BotLinkInfo{MakerFP: groupedFingerprint(makerKey), Mine: mine, MakerCheck: "off"}
	if domain != "" {
		info.MakerHere = pinnedUserOf(domain, makerKey)
	}
	if a.cloud != nil && a.cloud.keys.server() != "" {
		v := a.cloud.keys.verdict(makerKey, keyCheckWaitUI)
		info.MakerCheck, info.MakerName, info.MakerNote = v.Status, v.Name, v.Note
		if v.Status == "problem" {
			info.MakerNote = v.Problem
		}
	}
	return info, nil
}

// pinnedUserOf is who on this server has the key, as far as this app has
// seen ("" if nobody it's seen).
func pinnedUserOf(domain, key string) string {
	pinsMu.Lock()
	defer pinsMu.Unlock()
	pins := map[string]map[string]string{}
	if data, err := os.ReadFile(filepath.Join(chriscordConfigDir(), e2eePinsFile)); err == nil {
		_ = json.Unmarshal(data, &pins)
	}
	for user, k := range pins[domain] {
		if strings.EqualFold(k, key) {
			return user
		}
	}
	return ""
}

// serverBotCall sends a request about bots to the server you're on.
func (a *App) serverBotCall(method, path string, body interface{}) ([]byte, error) {
	a.mu.Lock()
	domain, token := a.domain, a.token
	a.mu.Unlock()
	if domain == "" || token == "" {
		return nil, errors.New("not connected")
	}
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, normaliseHTTP(domain)+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Session-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network error: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusNotFound && method == "GET" {
		return nil, errors.New("this server doesn't have bots yet (it needs updating)")
	}
	if resp.StatusCode >= 400 {
		var e map[string]string
		if json.Unmarshal(data, &e) == nil && e["error"] != "" {
			return nil, errors.New(e["error"])
		}
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return data, nil
}

// ServerBots is the server's bot list (JSON, as the server sends it).
func (a *App) ServerBots() (string, error) {
	data, err := a.serverBotCall("GET", "/api/bots", nil)
	return string(data), err
}

// AddBot adds a bot to the server you're on, from its link. username is
// what it's called here ("" for its own name).
func (a *App) AddBot(link string, permissions uint64, channels []string, username string) (string, error) {
	r, err := ParseBotAddLink(link)
	if err != nil {
		return "", err
	}
	if channels == nil {
		channels = []string{}
	}
	body := map[string]interface{}{"cert": r.Cert, "permissions": permissions &^ botNever, "channels": channels}
	if u := strings.TrimSpace(username); u != "" {
		body["username"] = u
	}
	data, err := a.serverBotCall("POST", "/api/bots", body)
	if err != nil {
		return "", err
	}
	var out struct {
		Username string `json:"username"`
	}
	json.Unmarshal(data, &out)
	return out.Username, nil
}

// UpdateBot changes what a bot on this server may do and see.
func (a *App) UpdateBot(botKey string, permissions uint64, channels []string) error {
	if channels == nil {
		channels = []string{}
	}
	_, err := a.serverBotCall("PUT", "/api/bots/"+url.PathEscape(strings.ToLower(botKey)),
		map[string]interface{}{"permissions": permissions &^ botNever, "channels": channels})
	return err
}

// RemoveBot takes a bot off this server.
func (a *App) RemoveBot(botKey string) error {
	_, err := a.serverBotCall("DELETE", "/api/bots/"+url.PathEscape(strings.ToLower(botKey)), nil)
	return err
}

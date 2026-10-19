package main

// lanes_app.go — this app's end of plugin lanes (lane.go): for a plugin
// that takes them, its commands and panel clicks go to it sealed, and its
// answers and panels come back sealed, so the server only passes them
// along. The page doesn't need to know: PluginCommand and PluginAction use
// a lane when the plugin has one, and what comes back is handed on as the
// same "plugin:event"s (plugin_reply, plugin_panel) as before.
//
// Which key a lane is sealed for comes from the server's plugin list (the
// plugin's identity key, and its encryption key signed by it). On a server
// with a signed member list, the identity key has to be on it as a plugin:
// otherwise the server could put its own key there and read everything.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	lanePingEvery = 15 * time.Minute
	// A command sent just before the plugin forgot its lane is sent again on
	// the new one, if it's this recent.
	laneResendWithin = 30 * time.Second
)

// lanePlugin is what the server's plugin list says about one plugin.
type lanePlugin struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Lanes     bool    `json:"lanes"`
	Connected bool    `json:"connected"`
	PublicKey string  `json:"public_key"`
	EncKey    *string `json:"enc_key"`
	EncSig    *string `json:"enc_sig"`
}

// appLane is one open lane, to one plugin.
type appLane struct {
	plugin string
	key    string // the plugin's identity key it was opened for
	k      *laneKeys
	last   []byte // the last command or click sent, for a resend
	lastAt time.Time
}

type laneClient struct {
	a  *App
	mu sync.Mutex
	// What the server's list said, by plugin id.
	plugins map[string]lanePlugin
	lanes   map[string]*appLane // by plugin id
	bySID   map[string]*appLane
	// Panels that came on lanes: plugin id → channel ("" everyone) → panel.
	panels map[string]map[string]json.RawMessage
	// Which channel each command was sent from (for its answer).
	asked  map[string]string
	pinger bool
	// Plugins this person blocked: no lane, nothing shown (SetBlockedPlugins).
	blocked map[string]bool
}

func newLaneClient(a *App) *laneClient {
	return &laneClient{a: a, plugins: map[string]lanePlugin{}, lanes: map[string]*appLane{}, bySID: map[string]*appLane{},
		panels: map[string]map[string]json.RawMessage{}, asked: map[string]string{}}
}

// laneClient: made with the App (newLaneClient), so no lock to get it —
// connect holds a.mu.
func (a *App) laneClient() *laneClient { return a.lanes }

// reset: a new connection (or another server). Lanes are opened again as
// they're needed; the plugin list is loaded again.
func (lc *laneClient) reset() {
	lc.mu.Lock()
	lc.plugins = map[string]lanePlugin{}
	lc.lanes = map[string]*appLane{}
	lc.bySID = map[string]*appLane{}
	lc.panels = map[string]map[string]json.RawMessage{}
	lc.asked = map[string]string{}
	start := !lc.pinger
	lc.pinger = true
	lc.mu.Unlock()
	if start {
		go lc.pingLoop()
	}
}

// begin: just connected — load the list and open lanes to the plugins
// that take them (their panels come on them).
func (lc *laneClient) begin() {
	lc.reset()
	lc.a.mu.Lock()
	domain := lc.a.domain
	lc.a.mu.Unlock()
	blocked := loadBlocked(domain)
	lc.mu.Lock()
	lc.blocked = blocked
	lc.mu.Unlock()
	if data, err := lc.a.serverBotCall("GET", "/api/plugins", nil); err == nil {
		lc.listed(data)
	}
}

// listed: the server's plugin list (JSON) — noted, lanes opened to the
// plugins that take them and are running, and a plugin's panels from its
// lane put in, as if the server had them. Returns the list to show.
func (lc *laneClient) listed(data []byte) []byte {
	var d struct {
		Plugins []map[string]json.RawMessage `json:"plugins"`
	}
	if json.Unmarshal(data, &d) != nil {
		return data
	}
	var open []string
	lc.mu.Lock()
	seen := map[string]bool{}
	for _, raw := range d.Plugins {
		b, _ := json.Marshal(raw)
		var p lanePlugin
		if json.Unmarshal(b, &p) != nil || p.ID == "" {
			continue
		}
		seen[p.ID] = true
		old, had := lc.plugins[p.ID]
		lc.plugins[p.ID] = p
		if !p.Lanes {
			lc.dropLocked(p.ID)
			continue
		}
		// It restarted, or its keys changed: its lanes are gone.
		if had && (!old.Connected && p.Connected || !strings.EqualFold(old.PublicKey, p.PublicKey) || encOf(old) != encOf(p)) {
			lc.dropLocked(p.ID)
		}
		if p.Connected && lc.lanes[p.ID] == nil && !lc.blocked[p.ID] {
			open = append(open, p.ID)
		}
		var panels []map[string]interface{}
		for board, panel := range lc.panels[p.ID] {
			panels = append(panels, map[string]interface{}{"board_id": board, "panel": panel})
		}
		pj, _ := json.Marshal(panels)
		raw["panels"] = pj
		raw["lanes_ok"] = json.RawMessage("true")
	}
	for id := range lc.plugins {
		if !seen[id] {
			delete(lc.plugins, id)
			lc.dropLocked(id)
		}
	}
	lc.mu.Unlock()
	for _, id := range open {
		go func(id string) {
			if _, err := lc.lane(id); err != nil {
				log.Printf("lanes: %s: %v", id, err)
			}
		}(id)
	}
	out, err := json.Marshal(d)
	if err != nil {
		return data
	}
	return out
}

func encOf(p lanePlugin) string {
	if p.EncKey == nil {
		return ""
	}
	return *p.EncKey
}

// dropLocked forgets a plugin's lane (and the panels that came on it).
func (lc *laneClient) dropLocked(id string) {
	if l := lc.lanes[id]; l != nil {
		delete(lc.bySID, l.k.SID)
	}
	delete(lc.lanes, id)
	delete(lc.panels, id)
}

// ── Blocking a plugin (for yourself) ─────────────────────────────────────────

const blockedPluginsFile = "chriscord_blocked_plugins.json"

var blockedPluginsMu sync.Mutex

// loadBlocked: the plugins blocked on domain.
func loadBlocked(domain string) map[string]bool {
	blockedPluginsMu.Lock()
	defer blockedPluginsMu.Unlock()
	all := map[string][]string{}
	if data, err := os.ReadFile(filepath.Join(chriscordConfigDir(), blockedPluginsFile)); err == nil {
		_ = json.Unmarshal(data, &all)
	}
	out := map[string]bool{}
	for _, id := range all[domain] {
		out[id] = true
	}
	return out
}

func saveBlocked(domain string, set map[string]bool) {
	blockedPluginsMu.Lock()
	defer blockedPluginsMu.Unlock()
	path := filepath.Join(chriscordConfigDir(), blockedPluginsFile)
	all := map[string][]string{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &all)
	}
	var ids []string
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		delete(all, domain)
	} else {
		all[domain] = ids
	}
	if data, err := json.MarshalIndent(all, "", "  "); err == nil {
		os.MkdirAll(filepath.Dir(path), 0700)
		os.WriteFile(path, data, 0600)
	}
}

// BlockedPlugins: the plugins you've blocked on this server.
func (a *App) BlockedPlugins() []string {
	lc := a.lanes
	lc.mu.Lock()
	defer lc.mu.Unlock()
	out := []string{}
	for id := range lc.blocked {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// SetPluginBlocked blocks (or unblocks) a plugin for you on this server:
// no lane to it (it doesn't hear from your app at all), and the page hides
// its panel, notices and messages and mutes it in calls. Remembered.
func (a *App) SetPluginBlocked(id string, on bool) {
	a.mu.Lock()
	domain := a.domain
	a.mu.Unlock()
	lc := a.lanes
	lc.mu.Lock()
	if lc.blocked == nil {
		lc.blocked = map[string]bool{}
	}
	if on {
		lc.blocked[id] = true
		lc.dropLocked(id)
	} else {
		delete(lc.blocked, id)
	}
	p := lc.plugins[id]
	set := map[string]bool{}
	for k := range lc.blocked {
		set[k] = true
	}
	lc.mu.Unlock()
	if domain != "" {
		saveBlocked(domain, set)
	}
	if !on && p.Lanes && p.Connected {
		go lc.lane(id)
	}
}

// uses: whether a plugin takes lanes (as far as the list said).
func (lc *laneClient) uses(id string) bool {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	return lc.plugins[id].Lanes
}

// lane: the open lane to a plugin, opening one if there isn't one.
func (lc *laneClient) lane(id string) (*appLane, error) {
	lc.mu.Lock()
	if l := lc.lanes[id]; l != nil {
		lc.mu.Unlock()
		return l, nil
	}
	p, ok := lc.plugins[id]
	lc.mu.Unlock()
	if !ok || !p.Lanes {
		return nil, errors.New("that plugin doesn't take encrypted lanes")
	}
	lc.mu.Lock()
	blocked := lc.blocked[id]
	lc.mu.Unlock()
	if blocked {
		return nil, errors.New("you've blocked " + p.Name + " — unblock it in its menu to use it")
	}
	if p.EncKey == nil || p.EncSig == nil {
		// The list was loaded before it published its key (it does just
		// after it starts): look again.
		if fresh, ok := lc.refreshPlugin(id); ok {
			p = fresh
		}
	}
	enc, err := lc.a.laneKeyFor(p)
	if err != nil {
		return nil, err
	}
	id2, ok := lc.a.text.identity()
	if !ok {
		return nil, errors.New("not signed in")
	}
	k, eph, hello, err := laneOpenFromApp(p.PublicKey, enc, id2.idPriv, id2.me)
	if err != nil {
		return nil, err
	}
	l := &appLane{plugin: id, key: strings.ToLower(p.PublicKey), k: k}
	lc.mu.Lock()
	if o := lc.lanes[id]; o != nil { // opened meanwhile
		lc.mu.Unlock()
		return o, nil
	}
	lc.lanes[id] = l
	lc.bySID[k.SID] = l
	lc.mu.Unlock()
	if err := lc.a.sendWSChecked(map[string]interface{}{"type": "plugin_lane", "plugin": id, "sid": k.SID, "n": 0, "eph": eph, "data": hello}); err != nil {
		lc.mu.Lock()
		if lc.lanes[id] == l {
			lc.dropLocked(id)
		}
		lc.mu.Unlock()
		return nil, err
	}
	return l, nil
}

// refreshPlugin loads the server's plugin list again and notes what it
// says about every plugin (keys, running), without opening anything.
// Returns plugin id's entry.
func (lc *laneClient) refreshPlugin(id string) (lanePlugin, bool) {
	data, err := lc.a.serverBotCall("GET", "/api/plugins", nil)
	if err != nil {
		return lanePlugin{}, false
	}
	var d struct {
		Plugins []lanePlugin `json:"plugins"`
	}
	if json.Unmarshal(data, &d) != nil {
		return lanePlugin{}, false
	}
	var out lanePlugin
	found := false
	lc.mu.Lock()
	for _, p := range d.Plugins {
		if p.ID == "" {
			continue
		}
		if old, had := lc.plugins[p.ID]; had && (!strings.EqualFold(old.PublicKey, p.PublicKey) || (encOf(old) != "" && encOf(old) != encOf(p))) {
			lc.dropLocked(p.ID) // its keys changed: its lane is gone
		}
		lc.plugins[p.ID] = p
		if p.ID == id {
			out, found = p, true
		}
	}
	lc.mu.Unlock()
	return out, found
}

// keysChanged: someone published an encryption key — perhaps a plugin that
// had just started. Lanes to running plugins that still lack one are
// opened now.
func (lc *laneClient) keysChanged() {
	lc.mu.Lock()
	waiting := false
	for id, p := range lc.plugins {
		if p.Lanes && p.Connected && lc.lanes[id] == nil && !lc.blocked[id] {
			waiting = true
		}
	}
	lc.mu.Unlock()
	if !waiting {
		return
	}
	lc.refreshPlugin("")
	lc.mu.Lock()
	var open []string
	for id, p := range lc.plugins {
		if p.Lanes && p.Connected && p.EncKey != nil && lc.lanes[id] == nil && !lc.blocked[id] {
			open = append(open, id)
		}
	}
	lc.mu.Unlock()
	for _, id := range open {
		if _, err := lc.lane(id); err != nil {
			log.Printf("lanes: %s: %v", id, err)
		}
	}
}

// laneKeyFor checks a plugin's keys: its encryption key signed by its
// identity key, and (with a signed member list) that key a plugin on it.
func (a *App) laneKeyFor(p lanePlugin) ([32]byte, error) {
	var enc [32]byte
	if p.EncKey == nil || p.EncSig == nil {
		return enc, errors.New(p.Name + " hasn't published its encryption key yet — try again in a moment")
	}
	id, err1 := hex.DecodeString(p.PublicKey)
	e, err2 := hex.DecodeString(*p.EncKey)
	sig, err3 := hex.DecodeString(*p.EncSig)
	if err1 != nil || err2 != nil || err3 != nil || len(id) != ed25519.PublicKeySize || len(e) != 32 ||
		!ed25519.Verify(id, encKeyMessage(e), sig) {
		return enc, errors.New(p.Name + "'s encryption key isn't signed by it — not sending it anything")
	}
	kind, state := a.rosterKindOf(p.PublicKey)
	for wait := 0; state == rosterLoading && wait < 40; wait++ { // just connected: give it a moment
		time.Sleep(250 * time.Millisecond)
		kind, state = a.rosterKindOf(p.PublicKey)
	}
	switch state {
	case rosterActive:
		if kind != kindPlugin {
			return enc, errors.New(p.Name + " isn't on this server's signed member list as a plugin — not sending it anything")
		}
	case rosterNone:
		return enc, errors.New("this server doesn't have its signed member list yet — nothing goes to plugins until the owner sets it up")
	case rosterLoading:
		return enc, errors.New("still checking this server's signed member list — try again in a moment")
	default:
		return enc, errors.New("this server's signed member list doesn't check out — not sending plugins anything")
	}
	copy(enc[:], e)
	return enc, nil
}

// send seals v on the plugin's lane (opening one if needed) and sends it.
func (lc *laneClient) send(id string, v interface{}) error {
	plain, err := json.Marshal(v)
	if err != nil {
		return err
	}
	l, err := lc.lane(id)
	if err != nil {
		return err
	}
	return lc.sendOn(l, plain, true)
}

func (lc *laneClient) sendOn(l *appLane, plain []byte, keep bool) error {
	lc.mu.Lock()
	n, data := l.k.seal(laneToPlugin, plain)
	if keep {
		l.last, l.lastAt = plain, time.Now()
	}
	lc.mu.Unlock()
	return lc.a.sendWSChecked(map[string]interface{}{"type": "plugin_lane", "plugin": l.plugin, "sid": l.k.SID, "n": n, "data": data})
}

// pingLoop keeps lanes from being forgotten while nothing's sent on them
// (the plugin forgets ones idle for 2 hours).
func (lc *laneClient) pingLoop() {
	t := time.NewTicker(lanePingEvery)
	defer t.Stop()
	for range t.C {
		lc.mu.Lock()
		var open []*appLane
		for _, l := range lc.lanes {
			open = append(open, l)
		}
		lc.mu.Unlock()
		for _, l := range open {
			_ = lc.sendOn(l, []byte(`{"t":"ping"}`), false)
		}
	}
}

// laneMessage is a sealed lane message from the server.
type laneMessage struct {
	Type   string `json:"type"`
	Plugin string `json:"plugin"`
	SID    string `json:"sid"`
	N      uint64 `json:"n"`
	Data   string `json:"data"`
}

// What a plugin sends on a lane.
type laneFromPluginMsg struct {
	T       string          `json:"t"`
	To      string          `json:"to"`
	Content string          `json:"content"`
	Text    string          `json:"text"`
	Error   bool            `json:"error"`
	Board   string          `json:"board"`
	Panel   json.RawMessage `json:"panel"`
	Public  bool            `json:"public"` // a notice for everyone in Board (Announce)
	ID      string          `json:"id"`
	At      int64           `json:"at"`
}

// incoming: plugin_lane / plugin_lane_gone from the server.
func (lc *laneClient) incoming(raw []byte) {
	var m laneMessage
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	lc.mu.Lock()
	l := lc.bySID[m.SID]
	if l == nil || l.plugin != m.Plugin {
		lc.mu.Unlock()
		return
	}
	if m.Type == "plugin_lane_gone" {
		// It forgot (restarted, say) or refused it: open another, and send
		// the last thing again if it was only just sent.
		resend := l.last
		if time.Since(l.lastAt) > laneResendWithin {
			resend = nil
		}
		boards := lc.panels[l.plugin]
		lc.dropLocked(l.plugin)
		lc.mu.Unlock()
		for b := range boards {
			lc.emitPanel(l.plugin, b, nil)
		}
		go func() {
			nl, err := lc.lane(l.plugin)
			if err != nil {
				lc.emitReply(l.plugin, "", err.Error(), true, "")
				return
			}
			if resend != nil {
				_ = lc.sendOn(nl, resend, true)
			}
		}()
		return
	}
	missedBefore := l.k.missed
	plain, err := l.k.open(laneFromPlugin, m.N, m.Data)
	missed := l.k.missed - missedBefore
	blocked := lc.blocked[l.plugin]
	lc.mu.Unlock()
	if err != nil {
		log.Printf("lanes: from %s: %v", m.Plugin, err)
		return
	}
	if blocked {
		return
	}
	if missed > 0 {
		// The counter jumped: the server dropped some (it can't read,
		// change or replay them, but it can hold them back).
		lc.emitReply(l.plugin, "", fmt.Sprintf("%d message%s from this plugin didn’t arrive — the server may be holding some back.", missed, map[bool]string{true: "", false: "s"}[missed == 1]), true, "")
	}
	var v laneFromPluginMsg
	if json.Unmarshal(plain, &v) != nil {
		return
	}
	switch v.T {
	case "reply":
		lc.mu.Lock()
		board := lc.asked[v.To]
		lc.mu.Unlock()
		lc.emitReply(l.plugin, v.To, v.Content, v.Error, board)
	case "notice":
		text := v.Text
		if text == "" {
			text = v.Content
		}
		if r := []rune(text); len(r) > 2000 {
			text = string(r[:2000])
		}
		if v.Public && v.Board != "" && v.ID != "" && len(v.ID) <= 64 && len(v.Board) <= 200 {
			// For everyone in the channel: shown there, marked as the
			// plugin's — not a chat message, and not kept in its history.
			lc.emit(map[string]interface{}{"type": "plugin_notice", "plugin": l.plugin, "board_id": v.Board, "id": v.ID, "text": text, "at": v.At})
			return
		}
		lc.emitReply(l.plugin, "", text, v.Error, v.Board)
	case "panel":
		if len(v.Board) > 200 || len(v.Panel) > 64<<10 {
			return
		}
		var panel json.RawMessage
		if len(v.Panel) > 0 && string(v.Panel) != "null" && v.Panel[0] == '{' {
			panel = v.Panel
		}
		lc.mu.Lock()
		if lc.lanes[l.plugin] != l {
			lc.mu.Unlock()
			return
		}
		if lc.panels[l.plugin] == nil {
			lc.panels[l.plugin] = map[string]json.RawMessage{}
		}
		if panel == nil {
			delete(lc.panels[l.plugin], v.Board)
		} else {
			lc.panels[l.plugin][v.Board] = panel
		}
		lc.mu.Unlock()
		lc.emitPanel(l.plugin, v.Board, panel)
	}
}

func (lc *laneClient) emitPanel(plugin, board string, panel json.RawMessage) {
	m := map[string]interface{}{"type": "plugin_panel", "plugin": plugin, "lane": true, "panel": nil}
	if board != "" {
		m["board_id"] = board
	}
	if panel != nil {
		m["panel"] = panel
	}
	lc.emit(m)
}

func (lc *laneClient) emitReply(plugin, id, text string, isErr bool, board string) {
	if len(text) > 2000 {
		text = text[:2000]
	}
	m := map[string]interface{}{"type": "plugin_reply", "plugin": plugin, "id": id, "content": text, "error": isErr, "lane": true}
	if board != "" {
		m["board_id"] = board
	}
	lc.emit(m)
}

func (lc *laneClient) emit(m map[string]interface{}) {
	b, _ := json.Marshal(m)
	if lc.a.ctx != nil {
		runtime.EventsEmit(lc.a.ctx, "plugin:event", string(b))
	}
}

// command and action: what PluginCommand and PluginAction send on a lane.
func (lc *laneClient) command(plugin, command string, args map[string]string, board string) error {
	id := randomLaneID()
	lc.mu.Lock()
	if len(lc.asked) > 500 {
		lc.asked = map[string]string{}
	}
	lc.asked[id] = board
	lc.mu.Unlock()
	return lc.send(plugin, map[string]interface{}{"t": "command", "id": id, "command": command, "args": args, "board": board})
}

func (lc *laneClient) action(plugin, action, value, board string) error {
	id := randomLaneID()
	lc.mu.Lock()
	lc.asked[id] = board
	lc.mu.Unlock()
	return lc.send(plugin, map[string]interface{}{"t": "action", "id": id, "action": action, "value": value, "board": board})
}

func randomLaneID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

package main

// games.go — "Playing …" by your name.
//
// Every so often (gameCheckEvery) this looks at the programs you have open
// and works out whether one is a game (games_windows.go): only programs
// that are installed as games — through Steam, Epic, GOG, the Xbox app, EA,
// Ubisoft, Riot, Battle.net, or a short list of well-known ones — never a
// browser or anything else. When that changes it tells the server
// (set_activity), which shows it by your name to the people on that server
// until you stop playing, turn it off, or disconnect.
//
// On by default, like Discord; Settings > Activity turns it off (kept in
// window.json, with the other app-wide settings).

import (
	"errors"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const gameCheckEvery = 15 * time.Second

var gameState struct {
	mu      sync.Mutex
	current string    // the game being reported now ("" for none)
	since   time.Time // when it started
	started bool
}

// shareGames: whether to report your game (on unless turned off).
func shareGames() bool {
	p := loadWindowPrefs()
	return gameDetectionSupported && (p.ShareGame == nil || *p.ShareGame)
}

// GameActivitySetting is what Settings shows.
type GameActivitySetting struct {
	Supported bool   `json:"supported"`
	Enabled   bool   `json:"enabled"`
	Current   string `json:"current"` // what's being reported right now
}

func (a *App) GetGameActivity() GameActivitySetting {
	gameState.mu.Lock()
	cur := gameState.current
	gameState.mu.Unlock()
	return GameActivitySetting{Supported: gameDetectionSupported, Enabled: shareGames(), Current: cur}
}

func (a *App) SetShareGameActivity(on bool) error {
	err := updateWindowPrefs(func(p *windowPrefs) { p.ShareGame = &on })
	go a.checkGame() // apply it now: clear it, or look straight away
	return err
}

// startGameWatcher runs for as long as the app does.
func (a *App) startGameWatcher() {
	gameState.mu.Lock()
	if gameState.started || !gameDetectionSupported {
		gameState.mu.Unlock()
		return
	}
	gameState.started = true
	gameState.mu.Unlock()
	go func() {
		time.Sleep(5 * time.Second) // let the window and connection settle first
		for {
			a.checkGame()
			time.Sleep(gameCheckEvery)
		}
	}()
}

var gameCheckMu sync.Mutex

func (a *App) checkGame() {
	gameCheckMu.Lock()
	defer gameCheckMu.Unlock()
	name := ""
	gameState.mu.Lock()
	cur := gameState.current
	gameState.mu.Unlock()
	if shareGames() {
		name = detectGame(cur)
	}
	if name == cur {
		return
	}
	gameState.mu.Lock()
	gameState.current = name
	gameState.since = time.Now()
	gameState.mu.Unlock()
	if encPresence.Load() {
		a.presenceChanged() // inside the next status blob (presence.go)
	} else {
		a.sendWS(map[string]interface{}{"type": "set_activity", "game": name})
	}
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "game:changed", name)
	}
}

// announceGame tells a server just (re)connected to what you're playing
// (one without encrypted statuses; otherwise it's in the status blob).
func (a *App) announceGame() {
	gameState.mu.Lock()
	cur := gameState.current
	gameState.mu.Unlock()
	if cur != "" {
		a.sendWS(map[string]interface{}{"type": "set_activity", "game": cur})
	}
}

// ── Your own games ────────────────────────────────────────────────────────────
//
// Any program can be added as a game — one that isn't found on its own, or
// to call it something else. While it's open, that's what "Playing …"
// shows. Matched by the program's full path.

// CustomGame is a program you've added, and what to call it.
type CustomGame struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// RunningProgram is a program with a window open (to pick one to add).
type RunningProgram struct {
	Path  string `json:"path"`
	Name  string `json:"name"`
	Title string `json:"title"` // its window's title
}

// customGameNames: lowercase path → name, for detectGame.
func customGameNames() map[string]string {
	out := map[string]string{}
	for _, g := range loadWindowPrefs().CustomGames {
		out[strings.ToLower(g.Path)] = g.Name
	}
	return out
}

func (a *App) GetCustomGames() []CustomGame {
	gs := loadWindowPrefs().CustomGames
	if gs == nil {
		gs = []CustomGame{}
	}
	return gs
}

// OpenPrograms lists the programs with a window open, to add one.
func (a *App) OpenPrograms() []RunningProgram {
	ps := openPrograms()
	if ps == nil {
		ps = []RunningProgram{}
	}
	return ps
}

// PickGameProgram asks for a program file to add.
func (a *App) PickGameProgram() (string, error) {
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title:   "Choose the game's program",
		Filters: []runtime.FileFilter{{DisplayName: "Programs", Pattern: "*.exe"}},
	})
}

// cleanGameName: trimmed, no control characters, at most 128 characters.
func cleanGameName(name string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(name) {
		if unicode.IsControl(r) {
			continue
		}
		if n == 128 {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// AddCustomGame adds a program (name "" → what the program calls itself),
// or renames it if it's already there.
func (a *App) AddCustomGame(path, name string) ([]CustomGame, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("choose a program first")
	}
	if name = cleanGameName(name); name == "" {
		name = cleanGameName(programName(path))
	}
	err := updateWindowPrefs(func(p *windowPrefs) {
		for i, g := range p.CustomGames {
			if strings.EqualFold(g.Path, path) {
				p.CustomGames[i].Name = name
				return
			}
		}
		p.CustomGames = append(p.CustomGames, CustomGame{Path: path, Name: name})
	})
	go a.checkGame()
	return a.GetCustomGames(), err
}

func (a *App) RemoveCustomGame(path string) ([]CustomGame, error) {
	err := updateWindowPrefs(func(p *windowPrefs) {
		out := p.CustomGames[:0]
		for _, g := range p.CustomGames {
			if !strings.EqualFold(g.Path, path) {
				out = append(out, g)
			}
		}
		p.CustomGames = out
	})
	go a.checkGame()
	return a.GetCustomGames(), err
}

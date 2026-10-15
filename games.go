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
	"sync"
	"time"

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

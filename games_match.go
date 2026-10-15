package main

// games_match.go — is this program a game, and which? (See games.go.)
//
// Only programs installed as games count, so a browser or any other app
// never shows up:
//   - Steam: under a library's steamapps\common\<folder>, named from that
//     library's appmanifest files (Steam's own tools — Steamworks, SteamVR,
//     Proton, Wallpaper Engine — are left out).
//   - Epic: inside an install listed in the Epic launcher's manifests and
//     marked as a game there.
//   - GOG: a folder (or one above it) with GOG's goggame-*.info file.
//   - The Xbox app, EA app, Ubisoft Connect, Riot, itch: their game folders,
//     named after the folder.
//   - A short list of well-known games by program name (Battle.net games,
//     Minecraft, Roblox, …), for installs outside those.
// Helpers that run alongside games — crash reporters, anti-cheat,
// launchers, updaters, overlays — never count.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var gameHelperExe = regexp.MustCompile(`(?i)(crashhandler|crashreport|crashpad|crash_report|crashsender|^unins\d*\.exe$|setup|redist|dxsetup|easyanticheat|battleye|beservice|bsservice|launcher|updater|webhelper|helper\.exe$|reporter|cefprocess|overlay|vrserver|vrmonitor|vrcompositor|qtwebengine|cefsharp|installer|patcher|bootstrapper|anticheat)`)

// Programs that are games wherever they're installed (lowercase name →
// what to call it).
var knownGameExes = map[string]string{
	"robloxplayerbeta.exe":              "Roblox",
	"osu!.exe":                          "osu!",
	"minecraft.windows.exe":             "Minecraft",
	"overwatch.exe":                     "Overwatch 2",
	"wow.exe":                           "World of Warcraft",
	"wowclassic.exe":                    "World of Warcraft Classic",
	"diablo iv.exe":                     "Diablo IV",
	"diablo iii64.exe":                  "Diablo III",
	"hearthstone.exe":                   "Hearthstone",
	"sc2_x64.exe":                       "StarCraft II",
	"valorant-win64-shipping.exe":       "VALORANT",
	"league of legends.exe":             "League of Legends",
	"cod.exe":                           "Call of Duty",
	"fortniteclient-win64-shipping.exe": "Fortnite",
	"r5apex.exe":                        "Apex Legends",
	"cs2.exe":                           "Counter-Strike 2",
	"dota2.exe":                         "Dota 2",
	"gta5.exe":                          "Grand Theft Auto V",
	"rocketleague.exe":                  "Rocket League",
	"eldenring.exe":                     "Elden Ring",
}

// Store folders whose next folder is a game, named after it.
var gameFolderMarkers = []string{
	`\xboxgames\`,
	`\ea games\`,
	`\ubisoft game launcher\games\`,
	`\riot games\`,
	`\gog galaxy\games\`,
	`\itch\apps\`,
	`\epic games\`, // when the Epic manifests can't be read
}

// Not games, though installed like them (lowercase; a name starting with
// one of these is left out).
var notGames = []string{
	"steamworks", "steamvr", "steam controller configs", "steam linux runtime", "proton",
	"wallpaper engine", "riot client", "launcher", "directxredist", "vcredist",
}

func isNotGame(name string) bool {
	n := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(name, "_", " ")))
	if n == "" {
		return true
	}
	for _, x := range notGames {
		if strings.HasPrefix(n, x) {
			return true
		}
	}
	return false
}

// identifyGame: the game this program is ("" if it isn't one). path is
// its full path, title its window's title.
func identifyGame(path, title string) string {
	path = strings.ReplaceAll(path, "/", `\`)
	lp := strings.ToLower(path)
	exe := lp[strings.LastIndex(lp, `\`)+1:]
	if exe == "javaw.exe" || exe == "java.exe" {
		if strings.HasPrefix(strings.ToLower(title), "minecraft") {
			return "Minecraft"
		}
		return ""
	}
	if gameHelperExe.MatchString(exe) {
		return ""
	}
	if name, ok := knownGameExes[exe]; ok {
		return name
	}
	if i := strings.Index(lp, `\steamapps\common\`); i >= 0 {
		dir := folderAt(path, i+len(`\steamapps\common\`))
		if dir == "" {
			return ""
		}
		name := steamGameName(path[:i]+`\steamapps`, dir)
		if isNotGame(name) || isNotGame(dir) {
			return ""
		}
		return name
	}
	if name, ok := epicGameName(lp); ok {
		return name // "" for an install that isn't a game
	}
	if name := gogGameName(path); name != "" {
		return name
	}
	for _, m := range gameFolderMarkers {
		if i := strings.Index(lp, m); i >= 0 {
			if dir := folderAt(path, i+len(m)); dir != "" && !isNotGame(dir) {
				return dir
			}
			return ""
		}
	}
	return ""
}

// folderAt: the folder name starting at byte i of path, if a folder (not
// the program itself) starts there.
func folderAt(path string, i int) string {
	if i >= len(path) {
		return ""
	}
	rest := path[i:]
	j := strings.Index(rest, `\`)
	if j <= 0 {
		return ""
	}
	return rest[:j]
}

// ── Steam ─────────────────────────────────────────────────────────────────────

var steamCache struct {
	mu   sync.Mutex
	libs map[string]steamLib // lowercase steamapps path →
}

type steamLib struct {
	read  time.Time
	names map[string]string // lowercase install folder → game name
}

var (
	acfInstallDir = regexp.MustCompile(`"installdir"\s+"([^"]*)"`)
	acfName       = regexp.MustCompile(`"name"\s+"([^"]*)"`)
)

func steamGameName(steamapps, dir string) string {
	key := strings.ToLower(steamapps)
	steamCache.mu.Lock()
	defer steamCache.mu.Unlock()
	if steamCache.libs == nil {
		steamCache.libs = map[string]steamLib{}
	}
	lib, ok := steamCache.libs[key]
	if !ok || time.Since(lib.read) > 2*time.Minute {
		lib = steamLib{read: time.Now(), names: map[string]string{}}
		files, _ := filepath.Glob(filepath.Join(steamapps, "appmanifest_*.acf"))
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			d, n := acfInstallDir.FindSubmatch(data), acfName.FindSubmatch(data)
			if d != nil && n != nil {
				lib.names[strings.ToLower(string(d[1]))] = string(n[1])
			}
		}
		steamCache.libs[key] = lib
	}
	if n := lib.names[strings.ToLower(dir)]; n != "" {
		return n
	}
	return dir
}

// ── Epic ──────────────────────────────────────────────────────────────────────

type epicItem struct {
	DisplayName     string   `json:"DisplayName"`
	InstallLocation string   `json:"InstallLocation"`
	AppCategories   []string `json:"AppCategories"`
}

var epicCache struct {
	mu    sync.Mutex
	read  time.Time
	items []epicItem
}

// epicGameName: ok when the program is inside an Epic install; name is ""
// if that install isn't a game (Unreal Engine, say).
func epicGameName(lowerPath string) (name string, ok bool) {
	epicCache.mu.Lock()
	defer epicCache.mu.Unlock()
	if time.Since(epicCache.read) > 2*time.Minute {
		epicCache.read = time.Now()
		epicCache.items = nil
		if pd := os.Getenv("ProgramData"); pd != "" {
			files, _ := filepath.Glob(filepath.Join(pd, "Epic", "EpicGamesLauncher", "Data", "Manifests", "*.item"))
			for _, f := range files {
				data, err := os.ReadFile(f)
				if err != nil {
					continue
				}
				var it epicItem
				if json.Unmarshal(data, &it) == nil && it.InstallLocation != "" {
					epicCache.items = append(epicCache.items, it)
				}
			}
		}
	}
	for _, it := range epicCache.items {
		loc := strings.ToLower(strings.TrimRight(strings.ReplaceAll(it.InstallLocation, "/", `\`), `\`)) + `\`
		if !strings.HasPrefix(lowerPath, loc) {
			continue
		}
		game := false
		for _, c := range it.AppCategories {
			if strings.EqualFold(c, "games") {
				game = true
			}
		}
		if !game || isNotGame(it.DisplayName) {
			return "", true
		}
		return it.DisplayName, true
	}
	return "", false
}

// ── GOG ───────────────────────────────────────────────────────────────────────

var gogCache sync.Map // folder → name ("" for none)

func gogGameName(path string) string {
	dir := path[:strings.LastIndex(path, `\`)+1]
	for i := 0; i < 4 && len(dir) > 3; i++ { // the program's folder and up to 3 above, not a drive root
		dir = strings.TrimRight(dir, `\`)
		if v, ok := gogCache.Load(strings.ToLower(dir)); ok {
			if n := v.(string); n != "" {
				return n
			}
		} else {
			name := ""
			files, _ := filepath.Glob(dir + `\goggame-*.info`)
			for _, f := range files {
				var info struct {
					Name string `json:"name"`
				}
				if data, err := os.ReadFile(f); err == nil && json.Unmarshal(data, &info) == nil && info.Name != "" {
					name = info.Name
					break
				}
			}
			gogCache.Store(strings.ToLower(dir), name)
			if name != "" {
				return name
			}
		}
		k := strings.LastIndex(dir, `\`)
		if k < 0 {
			break
		}
		dir = dir[:k+1]
	}
	return ""
}

// chooseGame picks one of the games running: the one in front, else the one
// already being shown (so it doesn't flip between two), else the first.
func chooseGame(found []string, foreground, current string) string {
	if foreground != "" {
		return foreground
	}
	for _, g := range found {
		if g == current {
			return g
		}
	}
	if len(found) > 0 {
		return found[0]
	}
	return ""
}

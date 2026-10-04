package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// Custom emoji. The list comes from GET /api/emojis; images are served to
// the page at /cc-emoji/<id> by serveEmojiAsset (wired into Wails' asset
// server in main.go), which keeps a disk cache. An emoji's image never
// changes once uploaded, so a cached file is good forever and messages
// can show hundreds of them without passing data URLs around.

type CustomEmoji struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Animated  bool   `json:"animated"`
	Hidden    bool   `json:"hidden"`
	CreatedBy string `json:"created_by"`
}

// GetEmojis lists this server's custom emoji (hidden ones flagged).
func (a *App) GetEmojis() ([]CustomEmoji, error) {
	var list []CustomEmoji
	if err := a.doGET("/api/emojis", &list); err != nil {
		return nil, err
	}
	return list, nil
}

// EmojiFile is an image picked to become an emoji: where it is, a
// suggested name (from the file name, made unique), and a preview the
// "Add Emoji" window shows while you pick names.
type EmojiFile struct {
	Path    string `json:"path"`
	File    string `json:"file"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Preview string `json:"preview"`
}

// PickEmojiFiles opens a file dialog (several files allowed) and returns
// the picked images with suggested names; nothing is uploaded yet.
// `pending` are names already chosen for other files waiting to upload,
// so suggestions don't clash with them either.
func (a *App) PickEmojiFiles(pending []string) ([]EmojiFile, error) {
	paths, err := runtime.OpenMultipleFilesDialog(a.ctx, runtime.OpenDialogOptions{
		Title:   "Choose emoji images",
		Filters: []runtime.FileFilter{{DisplayName: "Images (PNG, JPEG, GIF, WebP)", Pattern: "*.png;*.jpg;*.jpeg;*.gif;*.webp"}},
	})
	if err != nil || len(paths) == 0 {
		return nil, err
	}
	taken := map[string]bool{}
	for _, n := range pending {
		taken[strings.ToLower(n)] = true
	}
	if list, err := a.GetEmojis(); err == nil {
		for _, e := range list {
			if !e.Hidden {
				taken[strings.ToLower(e.Name)] = true
			}
		}
	}
	var out []EmojiFile
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		mime := pfpMime(data)
		switch mime {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
		default:
			continue // not an image the server takes
		}
		name := freeEmojiName(emojiNameFromFile(p), taken)
		taken[strings.ToLower(name)] = true
		out = append(out, EmojiFile{
			Path: p, File: filepath.Base(p), Name: name, Size: int64(len(data)),
			Preview: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data),
		})
	}
	return out, nil
}

// UploadEmojiFile uploads one picked image as an emoji called `name`.
func (a *App) UploadEmojiFile(path, name string) (*CustomEmoji, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return a.uploadEmoji(name, data)
}

// emojiNameFromFile turns "Party Parrot (2).gif" into "Party_Parrot_2".
func emojiNameFromFile(path string) string {
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	var b strings.Builder
	lastUnderscore := true
	for _, c := range stem {
		ok := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !ok {
			c = '_'
		}
		if c == '_' && lastUnderscore {
			continue
		}
		b.WriteRune(c)
		lastUnderscore = c == '_'
	}
	name := strings.Trim(b.String(), "_")
	if len(name) > 28 {
		name = strings.TrimRight(name[:28], "_")
	}
	if len(name) < 2 {
		name = "emoji"
	}
	return name
}

func freeEmojiName(name string, taken map[string]bool) string {
	if !taken[strings.ToLower(name)] {
		return name
	}
	for i := 2; ; i++ {
		n := fmt.Sprintf("%s_%d", name, i)
		if !taken[strings.ToLower(n)] {
			return n
		}
	}
}

func (a *App) uploadEmoji(name string, data []byte) (*CustomEmoji, error) {
	a.mu.Lock()
	domain, token := a.domain, a.token
	a.mu.Unlock()
	req, err := http.NewRequest("POST", normaliseHTTP(domain)+"/api/emojis?name="+url.QueryEscape(name), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Session-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network error: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e map[string]string
		json.NewDecoder(resp.Body).Decode(&e)
		if msg, ok := e["error"]; ok {
			return nil, fmt.Errorf("%s", msg)
		}
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var em CustomEmoji
	if err := json.NewDecoder(resp.Body).Decode(&em); err != nil {
		return nil, err
	}
	return &em, nil
}

// UpdateEmoji renames an emoji and/or hides it from the picker.
func (a *App) UpdateEmoji(id, name string, hidden bool) error {
	return a.sendJSON("PATCH", "/api/emojis/"+url.PathEscape(id), map[string]interface{}{"name": name, "hidden": hidden})
}

// DeleteEmoji removes an emoji for good.
func (a *App) DeleteEmoji(id string) error {
	return a.sendJSON("DELETE", "/api/emojis/"+url.PathEscape(id), map[string]interface{}{})
}

func emojiCacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "chriscord", "emoji")
}

func validEmojiID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for _, c := range id {
		if !(c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// serveEmojiAsset answers the page's requests for /cc-emoji/<id>: from
// the disk cache, or else fetched from the connected server and cached.
func (a *App) serveEmojiAsset(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutPrefix(r.URL.Path, "/cc-emoji/")
	if !ok || !validEmojiID(id) {
		http.NotFound(w, r)
		return
	}
	id = strings.ToLower(id)
	path := filepath.Join(emojiCacheDir(), id)
	data, err := os.ReadFile(path)
	if err != nil {
		data, err = a.fetchAuthed("/api/emojis/" + id)
		if err != nil || len(data) == 0 {
			http.NotFound(w, r)
			return
		}
		if os.MkdirAll(emojiCacheDir(), 0700) == nil {
			tmp := path + ".part"
			if os.WriteFile(tmp, data, 0600) == nil {
				os.Rename(tmp, path)
			}
		}
	}
	mime := pfpMime(data)
	if mime == "" {
		mime = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	io.Copy(w, bytes.NewReader(data))
}

// emitEmojisUpdated is called for the server's emojis_updated broadcast.
func (a *App) emitEmojisUpdated() { runtime.EventsEmit(a.ctx, "server:emojis") }

package main

import (
	"bytes"
	"path"
	"time"
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

// EmojiFile is an image waiting in the "Add Emoji / Stickers" window:
// from a file (Path set — uploaded straight from disk) or from a link
// (Path empty — the frontend uploads its Preview data). The frontend
// suggests a Name from File.
type EmojiFile struct {
	Path    string `json:"path"`
	File    string `json:"file"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Preview string `json:"preview"`
}

// PickImageFiles opens a file dialog (several files allowed) for emoji or
// sticker images and returns the chosen paths.
func (a *App) PickImageFiles(title string) ([]string, error) {
	return runtime.OpenMultipleFilesDialog(a.ctx, runtime.OpenDialogOptions{
		Title:   title,
		Filters: []runtime.FileFilter{{DisplayName: "Images (PNG, JPEG, GIF, WebP)", Pattern: "*.png;*.jpg;*.jpeg;*.gif;*.webp"}},
	})
}

func imageDataURL(data []byte) (string, bool) {
	switch mime := pfpMime(data); mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), true
	}
	return "", false
}

// ReadImageFiles loads picked or dropped files for the Add window,
// skipping anything that isn't a PNG, JPEG, GIF or WebP.
func (a *App) ReadImageFiles(paths []string) []EmojiFile {
	var out []EmojiFile
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if preview, ok := imageDataURL(data); ok {
			out = append(out, EmojiFile{Path: p, File: filepath.Base(p), Size: int64(len(data)), Preview: preview})
		}
	}
	return out
}

// ImageFromURL downloads an image someone dragged in from a web page
// (e.g. an emoji out of Discord in a browser). The page itself can't
// fetch other sites, so Go does it.
func (a *App) ImageFromURL(raw string) (*EmojiFile, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("not a web link")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (chriscord)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("couldn't download it: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("couldn't download it (HTTP %d)", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	if err != nil {
		return nil, err
	}
	preview, ok := imageDataURL(data)
	if !ok {
		return nil, fmt.Errorf("that link isn't a PNG, JPEG, GIF or WebP image")
	}
	file := path.Base(u.Path)
	if file == "." || file == "/" {
		file = "image"
	}
	return &EmojiFile{File: file, Size: int64(len(data)), Preview: preview}, nil
}

// decodeDataURL turns "data:image/…;base64,…" back into bytes.
func decodeDataURL(dataURL string) ([]byte, error) {
	comma := strings.Index(dataURL, ",")
	if !strings.HasPrefix(dataURL, "data:") || comma < 0 || !strings.HasSuffix(dataURL[:comma], ";base64") {
		return nil, fmt.Errorf("expected a base64 data URL")
	}
	return base64.StdEncoding.DecodeString(dataURL[comma+1:])
}

// UploadEmojiFile uploads an image file from disk as an emoji.
func (a *App) UploadEmojiFile(path, name string) (*CustomEmoji, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return a.uploadEmoji(name, data)
}

// UploadEmojiData uploads a pasted or downloaded image (a data URL).
func (a *App) UploadEmojiData(name, dataURL string) (*CustomEmoji, error) {
	data, err := decodeDataURL(dataURL)
	if err != nil {
		return nil, err
	}
	return a.uploadEmoji(name, data)
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

// serveEmojiAsset answers the page's requests for /cc-emoji/<id> and
// /cc-sticker/<id>: from the disk cache, or else fetched from the
// connected server and cached. Both kinds never change once uploaded.
func (a *App) serveEmojiAsset(w http.ResponseWriter, r *http.Request) {
	kinds := []struct{ prefix, api, dir string }{
		{"/cc-emoji/", "/api/emojis/", "emoji"},
		{"/cc-sticker/", "/api/stickers/", "stickers"},
	}
	for _, k := range kinds {
		id, ok := strings.CutPrefix(r.URL.Path, k.prefix)
		if !ok {
			continue
		}
		if !validEmojiID(id) {
			break
		}
		a.serveCachedImage(w, r, strings.ToLower(id), k.api, filepath.Join(filepath.Dir(emojiCacheDir()), k.dir))
		return
	}
	http.NotFound(w, r)
}

func (a *App) serveCachedImage(w http.ResponseWriter, r *http.Request, id, api, dir string) {
	path := filepath.Join(dir, id)
	data, err := os.ReadFile(path)
	if err != nil {
		data, err = a.fetchAuthed(api + id)
		if err != nil || len(data) == 0 {
			http.NotFound(w, r)
			return
		}
		if os.MkdirAll(dir, 0700) == nil {
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

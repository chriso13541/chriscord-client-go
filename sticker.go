package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// Custom stickers — the same shape as custom emoji (emoji.go). Images are
// served to the page at /cc-sticker/<id> by serveEmojiAsset.

type Sticker struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Animated    bool   `json:"animated"`
	Hidden      bool   `json:"hidden"`
	CreatedBy   string `json:"created_by"`
}

// GetStickers lists this server's stickers (hidden ones flagged).
func (a *App) GetStickers() ([]Sticker, error) {
	var list []Sticker
	if err := a.doGET("/api/stickers", &list); err != nil {
		return nil, err
	}
	return list, nil
}

// PickStickerFiles opens a file dialog and returns the picked images with
// a suggested name each (from the file name); nothing is uploaded yet.
func (a *App) PickStickerFiles() ([]EmojiFile, error) {
	paths, err := runtime.OpenMultipleFilesDialog(a.ctx, runtime.OpenDialogOptions{
		Title:   "Choose sticker images",
		Filters: []runtime.FileFilter{{DisplayName: "Images (PNG, JPEG, GIF, WebP)", Pattern: "*.png;*.jpg;*.jpeg;*.gif;*.webp"}},
	})
	if err != nil || len(paths) == 0 {
		return nil, err
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
			continue
		}
		out = append(out, EmojiFile{
			Path: p, File: filepath.Base(p), Name: stickerNameFromFile(p), Size: int64(len(data)),
			Preview: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data),
		})
	}
	return out, nil
}

// stickerNameFromFile keeps the file name readable ("Cat Wave" stays
// "Cat Wave"), dropping only what the server refuses: < > : and control
// characters. 2–30 characters.
func stickerNameFromFile(path string) string {
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	stem = strings.NewReplacer("_", " ", "-", " ").Replace(stem)
	var b strings.Builder
	for _, c := range stem {
		if c == '<' || c == '>' || c == ':' || c < 0x20 || c == 0x7f {
			continue
		}
		b.WriteRune(c)
	}
	name := strings.Join(strings.Fields(b.String()), " ")
	if r := []rune(name); len(r) > 30 {
		name = strings.TrimSpace(string(r[:30]))
	}
	if len([]rune(name)) < 2 {
		name = "sticker"
	}
	return name
}

// UploadStickerFile uploads one picked image as a sticker.
func (a *App) UploadStickerFile(path, name, description string) (*Sticker, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	domain, token := a.domain, a.token
	a.mu.Unlock()
	q := url.Values{"name": {name}, "description": {description}}
	req, err := http.NewRequest("POST", normaliseHTTP(domain)+"/api/stickers?"+q.Encode(), bytes.NewReader(data))
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
	var st Sticker
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

// UpdateSticker renames a sticker, changes its description, or hides it.
func (a *App) UpdateSticker(id, name, description string, hidden bool) error {
	return a.sendJSON("PATCH", "/api/stickers/"+url.PathEscape(id),
		map[string]interface{}{"name": name, "description": description, "hidden": hidden})
}

// DeleteSticker removes a sticker for good.
func (a *App) DeleteSticker(id string) error {
	return a.sendJSON("DELETE", "/api/stickers/"+url.PathEscape(id), map[string]interface{}{})
}

// emitStickersUpdated is called for the server's stickers_updated broadcast.
func (a *App) emitStickersUpdated() { runtime.EventsEmit(a.ctx, "server:stickers") }

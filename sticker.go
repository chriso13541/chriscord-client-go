package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"

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

// UploadStickerFile uploads an image file from disk as a sticker.
func (a *App) UploadStickerFile(path, name, description string) (*Sticker, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return a.uploadSticker(name, description, data)
}

// UploadStickerData uploads a pasted or downloaded image (a data URL).
func (a *App) UploadStickerData(name, description, dataURL string) (*Sticker, error) {
	data, err := decodeDataURL(dataURL)
	if err != nil {
		return nil, err
	}
	return a.uploadSticker(name, description, data)
}

func (a *App) uploadSticker(name, description string, data []byte) (*Sticker, error) {
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

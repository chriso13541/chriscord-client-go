package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// Rearranging categories and channels — the server only accepts these from
// the server owner.

// MoveRooms sets the category order, top first.
func (a *App) MoveRooms(ids []string) error {
	return a.sendJSON("PUT", "/api/layout/rooms", map[string]interface{}{"ids": ids})
}

// MoveBoard puts a channel in roomID, just above the channel `before`
// ("" = at the bottom). Use its current category to just reorder it.
func (a *App) MoveBoard(boardID, roomID, before string) error {
	body := map[string]interface{}{"room_id": roomID}
	if before != "" {
		body["before"] = before
	}
	return a.sendJSON("PUT", "/api/layout/boards/"+url.PathEscape(boardID), body)
}

// sendJSON sends a JSON body with the session token and turns the server's
// {"error": "..."} into a Go error.
func (a *App) sendJSON(method, path string, body interface{}) error {
	a.mu.Lock()
	domain, token := a.domain, a.token
	a.mu.Unlock()
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(method, normaliseHTTP(domain)+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("network error: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e map[string]string
		json.NewDecoder(resp.Body).Decode(&e)
		if msg, ok := e["error"]; ok {
			return fmt.Errorf("%s", msg)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

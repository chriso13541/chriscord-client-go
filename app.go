package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App holds all application state and exposes methods to the frontend.
// Every exported method is automatically available in JS as:
//
//	await window.go.main.App.MethodName(args...)
type App struct {
	ctx context.Context

	// Connection state
	mu       sync.Mutex       // guards ws and token/domain/username
	writeMu  sync.Mutex       // gorilla ws writes are not concurrent-safe
	ws       *websocket.Conn
	token    string
	domain   string
	username string

	// Persisted server list
	servers []SavedServer
}

func NewApp() *App {
	return &App{}
}

// startup is called by Wails when the app initialises.
func (a *App) startup(ctx context.Context) {
	a.ctx    = ctx
	a.servers = loadServers()
}

// ── URL helpers ───────────────────────────────────────────────────────────────

func normaliseHTTP(domain string) string {
	d := strings.TrimRight(domain, "/")
	if !strings.HasPrefix(d, "http://") && !strings.HasPrefix(d, "https://") {
		return "http://" + d
	}
	return d
}

func httpToWS(httpURL string) string {
	u := strings.Replace(httpURL, "https://", "wss://", 1)
	return strings.Replace(u, "http://", "ws://", 1)
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

func (a *App) doGET(path string, out interface{}) error {
	url := normaliseHTTP(a.domain) + path
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Session-Token", a.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("network error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		var e map[string]string
		json.Unmarshal(body, &e)
		if msg, ok := e["error"]; ok {
			return fmt.Errorf("%s", msg)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func postJSON(url string, body, out interface{}) error {
	data, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
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
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// ── Exposed to frontend ───────────────────────────────────────────────────────

// GetServers returns the saved server list.
func (a *App) GetServers() []SavedServer {
	if a.servers == nil {
		return []SavedServer{}
	}
	return a.servers
}

// GetServerInfo fetches the public /api/info from a server without connecting.
func (a *App) GetServerInfo(domain string) (*ServerInfo, error) {
	url := normaliseHTTP(domain) + "/api/info"
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("cannot reach server: %w", err)
	}
	defer resp.Body.Close()

	var info ServerInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	return &info, nil
}

// Connect joins a server, saves it, and opens a WebSocket connection.
func (a *App) Connect(domain, serverKey, username string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Close any existing connection
	if a.ws != nil {
		a.ws.Close()
		a.ws = nil
	}

	base := normaliseHTTP(domain)

	// Join the server — get a session token
	joinBody := map[string]interface{}{
		"username": username,
	}
	if serverKey != "" {
		joinBody["server_key"] = serverKey
	}

	var joinResp JoinResponse
	if err := postJSON(base+"/api/join", joinBody, &joinResp); err != nil {
		return err
	}

	a.domain   = domain
	a.token    = joinResp.Token
	a.username = joinResp.Username

	// Fetch server name for display
	displayName := domain
	if info, err := a.GetServerInfo(domain); err == nil {
		displayName = info.Name
	}

	// Save/update this server in the list
	a.servers = upsertServer(a.servers, SavedServer{
		Domain:       domain,
		ServerKey:    serverKey,
		DisplayName:  displayName,
		LastUsername: a.username,
	})
	saveServers(a.servers)

	// Open WebSocket
	wsURL := httpToWS(base) + "/ws?token=" + a.token
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("websocket failed: %w", err)
	}
	a.ws = conn

	// Read loop in background
	go a.wsReader(conn)

	return nil
}

// wsReader runs in its own goroutine and forwards server messages to the frontend.
func (a *App) wsReader(conn *websocket.Conn) {
	defer func() {
		a.mu.Lock()
		if a.ws == conn {
			a.ws = nil
		}
		a.mu.Unlock()
		runtime.EventsEmit(a.ctx, "ws:disconnected")
	}()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}

		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(raw, &envelope); err != nil {
			continue
		}

		var msgType string
		json.Unmarshal(envelope["type"], &msgType)

		switch msgType {
		case "message":
			// Single new message — forward the "data" field
			var msg ChatMessage
			if err := json.Unmarshal(envelope["data"], &msg); err == nil {
				runtime.EventsEmit(a.ctx, "chat:message", msg)
			}
		case "history":
			// Full message history — forward the "messages" array
			var msgs []ChatMessage
			if err := json.Unmarshal(envelope["messages"], &msgs); err == nil {
				runtime.EventsEmit(a.ctx, "chat:history", msgs)
			}
		}
	}
}

// GetRooms returns the list of rooms for the current server.
func (a *App) GetRooms() ([]Room, error) {
	var rooms []Room
	if err := a.doGET("/api/rooms", &rooms); err != nil {
		return nil, err
	}
	return rooms, nil
}

// GetBoards returns boards for a given room.
func (a *App) GetBoards(roomID string) ([]Board, error) {
	var boards []Board
	if err := a.doGET("/api/rooms/"+roomID+"/boards", &boards); err != nil {
		return nil, err
	}
	return boards, nil
}

// SubscribeBoard tells the server we want messages for this board.
// The server will respond with a history payload followed by live messages.
func (a *App) SubscribeBoard(boardID string) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()

	a.mu.Lock()
	conn := a.ws
	a.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("not connected")
	}

	msg, _ := json.Marshal(map[string]string{
		"type":     "subscribe",
		"board_id": boardID,
	})
	return conn.WriteMessage(websocket.TextMessage, msg)
}

// SendMessage sends a chat message to the currently subscribed board.
func (a *App) SendMessage(boardID, content string) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()

	a.mu.Lock()
	conn := a.ws
	a.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("not connected")
	}

	msg, _ := json.Marshal(map[string]string{
		"type":     "message",
		"board_id": boardID,
		"content":  content,
	})
	return conn.WriteMessage(websocket.TextMessage, msg)
}

// GetUsername returns the current logged-in username.
func (a *App) GetUsername() string {
	return a.username
}

// Disconnect closes the WebSocket and clears session state.
func (a *App) Disconnect() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.ws != nil {
		a.ws.Close()
		a.ws = nil
	}
	a.token    = ""
	a.domain   = ""
	a.username = ""
}

// RemoveServer deletes a saved server from the list.
func (a *App) RemoveServer(domain string) []SavedServer {
	a.servers = removeServer(a.servers, domain)
	saveServers(a.servers)
	return a.servers
}

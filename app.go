package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

type App struct {
	ctx     context.Context
	mu      sync.Mutex
	writeMu sync.Mutex
	ws      *websocket.Conn
	token   string
	domain  string
	username string
	servers []SavedServer
}

func NewApp() *App { return &App{} }

func (a *App) startup(ctx context.Context) {
	a.ctx     = ctx
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
	req, err := http.NewRequest("GET", normaliseHTTP(a.domain)+path, nil)
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
		var e map[string]string
		json.NewDecoder(resp.Body).Decode(&e)
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

func (a *App) GetServers() []SavedServer {
	if a.servers == nil {
		return []SavedServer{}
	}
	return a.servers
}

func (a *App) GetServerInfo(domain string) (*ServerInfo, error) {
	resp, err := http.Get(normaliseHTTP(domain) + "/api/info")
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

func (a *App) Connect(domain, serverKey, username string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.ws != nil {
		a.ws.Close()
		a.ws = nil
	}

	base := normaliseHTTP(domain)
	joinBody := map[string]interface{}{"username": username}
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

	displayName := domain
	if info, err := a.GetServerInfo(domain); err == nil {
		displayName = info.Name
	}

	a.servers = upsertServer(a.servers, SavedServer{
		Domain: domain, ServerKey: serverKey,
		DisplayName: displayName, LastUsername: a.username,
	})
	saveServers(a.servers)

	wsURL := httpToWS(base) + "/ws?token=" + a.token
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("websocket failed: %w", err)
	}
	a.ws = conn
	go a.wsReader(conn)
	return nil
}

// serverMsg covers every packet the server can send.
type serverMsg struct {
	Type     string        `json:"type"`
	BoardID  string        `json:"board_id"`
	Data     *ChatMessage  `json:"data"`
	Messages []ChatMessage `json:"messages"`
	// users packet
	Online  []string `json:"online"`
	All     []string `json:"all"`
}

type historyEvent struct {
	BoardID  string        `json:"board_id"`
	Messages []ChatMessage `json:"messages"`
}

type usersEvent struct {
	Online []string `json:"online"`
	All    []string `json:"all"`
}

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
		var msg serverMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "message":
			if msg.Data != nil {
				runtime.EventsEmit(a.ctx, "chat:message", *msg.Data)
			}
		case "history":
			runtime.EventsEmit(a.ctx, "chat:history", historyEvent{
				BoardID:  msg.BoardID,
				Messages: msg.Messages,
			})
		case "users":
			runtime.EventsEmit(a.ctx, "chat:users", usersEvent{
				Online: msg.Online,
				All:    msg.All,
			})
		}
	}
}

func (a *App) GetRooms() ([]Room, error) {
	var rooms []Room
	if err := a.doGET("/api/rooms", &rooms); err != nil {
		return nil, err
	}
	return rooms, nil
}

func (a *App) GetBoards(roomID string) ([]Board, error) {
	var boards []Board
	if err := a.doGET("/api/rooms/"+roomID+"/boards", &boards); err != nil {
		return nil, err
	}
	return boards, nil
}

func (a *App) SubscribeBoard(boardID string) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	a.mu.Lock()
	conn := a.ws
	a.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("not connected")
	}
	msg, _ := json.Marshal(map[string]string{"type": "subscribe", "board_id": boardID})
	return conn.WriteMessage(websocket.TextMessage, msg)
}

func (a *App) SendMessage(boardID, content string, attachURL, attachName, attachMime *string) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	a.mu.Lock()
	conn := a.ws
	a.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("not connected")
	}
	payload := map[string]interface{}{
		"type":     "message",
		"board_id": boardID,
		"content":  content,
	}
	if attachURL != nil {
		payload["attachment_url"]  = *attachURL
		payload["attachment_name"] = *attachName
		payload["attachment_mime"] = *attachMime
	}
	msg, _ := json.Marshal(payload)
	return conn.WriteMessage(websocket.TextMessage, msg)
}

// PickFile opens a native file dialog and returns the chosen path.
func (a *App) PickFile() (string, error) {
	path, err := runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Attach a file",
		Filters: []runtime.FileFilter{
			{DisplayName: "All Files", Pattern: "*"},
			{DisplayName: "Images",    Pattern: "*.png;*.jpg;*.jpeg;*.gif;*.webp"},
			{DisplayName: "Video",     Pattern: "*.mp4;*.webm;*.mov;*.mkv;*.avi"},
			{DisplayName: "Audio",     Pattern: "*.mp3;*.ogg;*.wav;*.flac;*.m4a"},
			{DisplayName: "Documents", Pattern: "*.pdf;*.doc;*.docx;*.txt;*.zip"},
		},
	})
	if err != nil || path == "" {
		return "", err
	}
	return path, nil
}

// UploadFile streams a file from disk to the server without loading it all into memory.
// This handles large videos without OOM.
func (a *App) UploadFile(filePath string) (*UploadResult, error) {
	a.mu.Lock()
	domain := a.domain
	token  := a.token
	a.mu.Unlock()

	if domain == "" {
		return nil, fmt.Errorf("not connected")
	}

	// Open the file for streaming — do NOT read it all into memory.
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("could not open file: %w", err)
	}
	defer f.Close()

	filename := filepath.Base(filePath)

	// Build the multipart body as a pipe so we never buffer the whole file.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		fw, err := mw.CreateFormFile("file", filename)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(fw, f); err != nil {
			pw.CloseWithError(err)
			return
		}
		mw.Close()
		pw.Close()
	}()

	req, err := http.NewRequest("POST", normaliseHTTP(domain)+"/api/upload", pr)
	if err != nil {
		pr.CloseWithError(err)
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Session-Token", token)

	// Use a client with a generous timeout for large files (10 min).
	client := &http.Client{Timeout: 10 * 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload failed: %w", err)
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

	var result UploadResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetFileURL returns the full URL for a server-relative file path.
func (a *App) GetFileURL(path string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return normaliseHTTP(a.domain) + path
}

func (a *App) GetUsername() string { return a.username }

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

func (a *App) RemoveServer(domain string) []SavedServer {
	a.servers = removeServer(a.servers, domain)
	saveServers(a.servers)
	return a.servers
}

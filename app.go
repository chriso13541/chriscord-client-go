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
	ctx      context.Context
	mu       sync.Mutex
	writeMu  sync.Mutex
	ws       *websocket.Conn
	token    string
	domain   string
	username string
	servers  []SavedServer
}

func NewApp() *App { return &App{} }

func (a *App) startup(ctx context.Context) {
	a.ctx    = ctx
	a.servers = loadServers()
}

func normaliseHTTP(domain string) string {
	d := strings.TrimRight(domain, "/")
	if !strings.HasPrefix(d, "http://") && !strings.HasPrefix(d, "https://") {
		return "http://" + d
	}
	return d
}
func httpToWS(u string) string {
	u = strings.Replace(u, "https://", "wss://", 1)
	return strings.Replace(u, "http://", "ws://", 1)
}

func (a *App) doGET(path string, out interface{}) error {
	req, err := http.NewRequest("GET", normaliseHTTP(a.domain)+path, nil)
	if err != nil { return err }
	req.Header.Set("X-Session-Token", a.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil { return fmt.Errorf("network error: %w", err) }
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e map[string]string
		json.NewDecoder(resp.Body).Decode(&e)
		if msg, ok := e["error"]; ok { return fmt.Errorf("%s", msg) }
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func postJSON(url string, body, out interface{}) error {
	data, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil { return fmt.Errorf("network error: %w", err) }
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e map[string]string
		json.NewDecoder(resp.Body).Decode(&e)
		if msg, ok := e["error"]; ok { return fmt.Errorf("%s", msg) }
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if out != nil { return json.NewDecoder(resp.Body).Decode(out) }
	return nil
}

// ── Exposed to frontend ───────────────────────────────────────────────────────

func (a *App) GetServers() []SavedServer {
	if a.servers == nil { return []SavedServer{} }
	return a.servers
}

func (a *App) GetServerInfo(domain string) (*ServerInfo, error) {
	resp, err := http.Get(normaliseHTTP(domain) + "/api/info")
	if err != nil { return nil, fmt.Errorf("cannot reach server: %w", err) }
	defer resp.Body.Close()
	var info ServerInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil { return nil, err }
	return &info, nil
}

func (a *App) Connect(domain, serverKey, username string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ws != nil { a.ws.Close(); a.ws = nil }

	base := normaliseHTTP(domain)
	joinBody := map[string]interface{}{"username": username}
	if serverKey != "" { joinBody["server_key"] = serverKey }
	var joinResp JoinResponse
	if err := postJSON(base+"/api/join", joinBody, &joinResp); err != nil { return err }

	a.domain   = domain
	a.token    = joinResp.Token
	a.username = joinResp.Username

	displayName := domain
	if info, err := a.GetServerInfo(domain); err == nil { displayName = info.Name }
	a.servers = upsertServer(a.servers, SavedServer{
		Domain: domain, ServerKey: serverKey, DisplayName: displayName, LastUsername: a.username,
	})
	saveServers(a.servers)

	wsURL := httpToWS(base) + "/ws?token=" + a.token
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil { return fmt.Errorf("websocket failed: %w", err) }
	a.ws = conn
	go a.wsReader(conn)
	return nil
}

type serverMsg struct {
	Type     string        `json:"type"`
	BoardID  string        `json:"board_id"`
	Data     *ChatMessage  `json:"data"`
	Messages []ChatMessage `json:"messages"`
	Online   []string      `json:"online"`
	All      []string      `json:"all"`
	ID       string        `json:"id"`
	Content  string        `json:"content"`
}

type historyEvent struct {
	BoardID  string        `json:"board_id"`
	Messages []ChatMessage `json:"messages"`
}
type usersEvent  struct { Online []string `json:"online"`; All []string `json:"all"` }
type editEvent   struct { ID string `json:"id"`; BoardID string `json:"board_id"`; Content string `json:"content"` }
type deleteEvent struct { ID string `json:"id"`; BoardID string `json:"board_id"` }

func (a *App) wsReader(conn *websocket.Conn) {
	defer func() {
		a.mu.Lock(); if a.ws == conn { a.ws = nil }; a.mu.Unlock()
		runtime.EventsEmit(a.ctx, "ws:disconnected")
	}()
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil { return }
		var msg serverMsg
		if err := json.Unmarshal(raw, &msg); err != nil { continue }
		switch msg.Type {
		case "message":
			if msg.Data != nil { runtime.EventsEmit(a.ctx, "chat:message", *msg.Data) }
		case "history":
			runtime.EventsEmit(a.ctx, "chat:history", historyEvent{BoardID: msg.BoardID, Messages: msg.Messages})
		case "users":
			runtime.EventsEmit(a.ctx, "chat:users", usersEvent{Online: msg.Online, All: msg.All})
		case "message_edit":
			runtime.EventsEmit(a.ctx, "chat:edit", editEvent{ID: msg.ID, BoardID: msg.BoardID, Content: msg.Content})
		case "message_delete":
			runtime.EventsEmit(a.ctx, "chat:delete", deleteEvent{ID: msg.ID, BoardID: msg.BoardID})
		}
	}
}

func (a *App) GetRooms() ([]Room, error) {
	var rooms []Room
	return rooms, a.doGET("/api/rooms", &rooms)
}

func (a *App) GetBoards(roomID string) ([]Board, error) {
	var boards []Board
	return boards, a.doGET("/api/rooms/"+roomID+"/boards", &boards)
}

func (a *App) SubscribeBoard(boardID string) error {
	a.writeMu.Lock(); defer a.writeMu.Unlock()
	a.mu.Lock(); conn := a.ws; a.mu.Unlock()
	if conn == nil { return fmt.Errorf("not connected") }
	msg, _ := json.Marshal(map[string]string{"type": "subscribe", "board_id": boardID})
	return conn.WriteMessage(websocket.TextMessage, msg)
}

func (a *App) SendMessage(boardID, content string, attachments []Attachment) error {
	a.writeMu.Lock(); defer a.writeMu.Unlock()
	a.mu.Lock(); conn := a.ws; a.mu.Unlock()
	if conn == nil { return fmt.Errorf("not connected") }
	if attachments == nil { attachments = []Attachment{} }
	payload, _ := json.Marshal(map[string]interface{}{
		"type":        "message",
		"board_id":    boardID,
		"content":     content,
		"attachments": attachments,
	})
	return conn.WriteMessage(websocket.TextMessage, payload)
}

func (a *App) DeleteMessage(msgID string) error {
	a.mu.Lock(); domain := a.domain; token := a.token; a.mu.Unlock()
	req, err := http.NewRequest("DELETE", normaliseHTTP(domain)+"/api/messages/"+msgID, nil)
	if err != nil { return err }
	req.Header.Set("X-Session-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil { return fmt.Errorf("network error: %w", err) }
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e map[string]string; json.NewDecoder(resp.Body).Decode(&e)
		if msg, ok := e["error"]; ok { return fmt.Errorf("%s", msg) }
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func (a *App) EditMessage(msgID, content string) error {
	a.mu.Lock(); domain := a.domain; token := a.token; a.mu.Unlock()
	data, _ := json.Marshal(map[string]string{"content": content})
	req, err := http.NewRequest("PATCH", normaliseHTTP(domain)+"/api/messages/"+msgID, bytes.NewReader(data))
	if err != nil { return err }
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil { return fmt.Errorf("network error: %w", err) }
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e map[string]string; json.NewDecoder(resp.Body).Decode(&e)
		if msg, ok := e["error"]; ok { return fmt.Errorf("%s", msg) }
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// DropFiles is called by the frontend when files are dropped via drag & drop.
// Since Wails v2's OnFileDrop requires a newer version, the frontend uses
// the browser drag API which gives us File objects without native paths.
// Instead of paths, the frontend calls this with the filenames and we open
// a file picker pre-filtered — but actually we just expose PickFiles which
// the user can use. For true drag & drop we expose this no-op for forward compat.
func (a *App) DropFiles(paths []string) error {
	return nil
}

// PickFiles opens a native multi-file dialog.
func (a *App) PickFiles() ([]string, error) {
	paths, err := runtime.OpenMultipleFilesDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Attach files",
		Filters: []runtime.FileFilter{
			{DisplayName: "All Files",   Pattern: "*"},
			{DisplayName: "Images",      Pattern: "*.png;*.jpg;*.jpeg;*.gif;*.webp"},
			{DisplayName: "Video",       Pattern: "*.mp4;*.webm;*.mov;*.mkv;*.avi"},
			{DisplayName: "Audio",       Pattern: "*.mp3;*.ogg;*.wav;*.flac;*.m4a"},
			{DisplayName: "Documents",   Pattern: "*.pdf;*.doc;*.docx;*.txt;*.zip"},
		},
	})
	if err != nil { return nil, err }
	return paths, nil
}

// UploadFile streams a file to the server without loading it all into memory.
func (a *App) UploadFile(filePath string) (*UploadResult, error) {
	a.mu.Lock(); domain := a.domain; token := a.token; a.mu.Unlock()
	if domain == "" { return nil, fmt.Errorf("not connected") }

	f, err := os.Open(filePath)
	if err != nil { return nil, fmt.Errorf("could not open file: %w", err) }
	defer f.Close()

	filename := filepath.Base(filePath)
	pr, pw   := io.Pipe()
	mw       := multipart.NewWriter(pw)

	go func() {
		fw, err := mw.CreateFormFile("file", filename)
		if err != nil { pw.CloseWithError(err); return }
		if _, err := io.Copy(fw, f); err != nil { pw.CloseWithError(err); return }
		mw.Close(); pw.Close()
	}()

	req, err := http.NewRequest("POST", normaliseHTTP(domain)+"/api/upload", pr)
	if err != nil { pr.CloseWithError(err); return nil, err }
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Session-Token", token)

	client := &http.Client{Timeout: 10 * 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil { return nil, fmt.Errorf("upload failed: %w", err) }
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var e map[string]string; json.NewDecoder(resp.Body).Decode(&e)
		if msg, ok := e["error"]; ok { return nil, fmt.Errorf("%s", msg) }
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var result UploadResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil { return nil, err }
	return &result, nil
}

// GetFileURL returns the full URL for a server-relative path.
func (a *App) GetFileURL(path string) string {
	a.mu.Lock(); defer a.mu.Unlock()
	return normaliseHTTP(a.domain) + path
}

// FetchLinkPreview fetches OG data for a URL via the server proxy.
func (a *App) FetchLinkPreview(url string) (*LinkPreview, error) {
	a.mu.Lock(); domain := a.domain; token := a.token; a.mu.Unlock()
	if domain == "" { return nil, fmt.Errorf("not connected") }

	req, err := http.NewRequest("GET",
		normaliseHTTP(domain)+"/api/preview?url="+url, nil)
	if err != nil { return nil, err }
	req.Header.Set("X-Session-Token", token)

	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil { return nil, err }
	defer resp.Body.Close()
	if resp.StatusCode >= 400 { return nil, fmt.Errorf("preview unavailable") }

	var p LinkPreview
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil { return nil, err }
	return &p, nil
}

func (a *App) GetUsername() string { return a.username }

func (a *App) Disconnect() {
	a.mu.Lock(); defer a.mu.Unlock()
	if a.ws != nil { a.ws.Close(); a.ws = nil }
	a.token = ""; a.domain = ""; a.username = ""
}

func (a *App) RemoveServer(domain string) []SavedServer {
	a.servers = removeServer(a.servers, domain)
	saveServers(a.servers)
	return a.servers
}

package main

// SavedServer is a homelab server entry persisted to disk.
type SavedServer struct {
	Domain       string `json:"domain"`
	ServerKey    string `json:"server_key"`
	DisplayName  string `json:"display_name"`
	LastUsername string `json:"last_username"`
}

// ServerInfo is the public info returned by GET /api/info.
type ServerInfo struct {
	Name        string `json:"name"`
	RequiresKey bool   `json:"requires_key"`
}

// JoinResponse is returned by POST /api/join.
type JoinResponse struct {
	Token    string `json:"token"`
	Username string `json:"username"`
}

// Room is a logical group of boards (like a Discord server).
type Room struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsPrivate bool   `json:"is_private"`
}

// Board is a text channel inside a room.
type Board struct {
	ID     string `json:"id"`
	RoomID string `json:"room_id"`
	Name   string `json:"name"`
}

// ChatMessage is a single chat message.
type ChatMessage struct {
	ID        string `json:"id"`
	BoardID   string `json:"board_id"`
	Username  string `json:"username"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

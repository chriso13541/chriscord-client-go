package main

type SavedServer struct {
	Domain       string `json:"domain"`
	ServerKey    string `json:"server_key"`
	DisplayName  string `json:"display_name"`
	LastUsername string `json:"last_username"`
}

type ServerInfo struct {
	Name        string `json:"name"`
	RequiresKey bool   `json:"requires_key"`
}

type JoinResponse struct {
	Token    string `json:"token"`
	Username string `json:"username"`
}

type Room struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsPrivate bool   `json:"is_private"`
}

type Board struct {
	ID     string `json:"id"`
	RoomID string `json:"room_id"`
	Name   string `json:"name"`
}

// Attachment is a single file attached to a message.
type Attachment struct {
	URL  string `json:"url"`
	Name string `json:"name"`
	Mime string `json:"mime"`
}

type ChatMessage struct {
	ID          string       `json:"id"`
	BoardID     string       `json:"board_id"`
	Username    string       `json:"username"`
	Content     string       `json:"content"`
	Attachments []Attachment `json:"attachments"`
	Edited      bool         `json:"edited"`
	CreatedAt   string       `json:"created_at"`
}

type UploadResult struct {
	URL      string `json:"url"`
	Filename string `json:"filename"`
	Mime     string `json:"mime"`
}

type LinkPreview struct {
	URL         string  `json:"url"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
	Image       *string `json:"image"`
	SiteName    *string `json:"site_name"`
}

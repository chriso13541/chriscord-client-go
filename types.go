package main

// SavedServer is the on-disk record for one host we've joined. Identity now
// lives at the account level (see account.go), not here — every host uses
// whichever account is currently unlocked, so this just tracks connection
// details: the join key, a display name, and the username we last joined as.
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

type ChallengeResponse struct {
	Nonce string `json:"nonce"`
}

type JoinResponse struct {
	Token       string `json:"token"`
	Username    string `json:"username"`
	Fingerprint string `json:"fingerprint"`
}

// AccountView is what's safe to expose to the frontend for the active
// account — never the public key, and nothing from identity.json.
type AccountView struct {
	Username    string `json:"username"`
	Fingerprint string `json:"fingerprint"`
	HasAvatar   bool   `json:"has_avatar"`
}

// AccountSummary describes a locally saved account without unlocking it.
// account.json is stored in plaintext (it's just a display name), so this
// is available before any passphrase is entered — enough to list accounts
// for a future switcher.
type AccountSummary struct {
	Slug      string `json:"slug"`
	Username  string `json:"username"`
	HasAvatar bool   `json:"has_avatar"`
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

// SearchResult is a ChatMessage plus which board (and its room) it came
// from — only meaningful for server-wide search, which spans every
// channel. The embedded ChatMessage's fields are promoted into the same
// JSON object by encoding/json, matching the server's #[serde(flatten)].
type SearchResult struct {
	ChatMessage
	BoardName string `json:"board_name"`
	RoomID    string `json:"room_id"`
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

package main

import "encoding/base64"

// ClipboardFiles: pictures or files on the clipboard, for pasting as
// attachments (Linux: clipboard_linux.go).
type ClipboardFiles struct {
	Paths []string `json:"paths"` // copied files
	Image []byte   `json:"-"`
	Mime  string   `json:"mime"` // the picture's type
	Data  string   `json:"data"` // the picture, base64
}

// ClipboardAttachments: what's on the clipboard that pastes as attachments
// — files copied in the file manager, or a copied picture. Used on Linux,
// where the web view's paste event never carries them. Empty elsewhere.
func (a *App) ClipboardAttachments() (ClipboardFiles, error) {
	out, err := readClipboardAttachments()
	if err != nil {
		return ClipboardFiles{}, err
	}
	if len(out.Image) > 0 {
		out.Data = base64.StdEncoding.EncodeToString(out.Image)
	}
	return out, nil
}

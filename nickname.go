package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

// Nicknames. Everyone has a username (fixed, how the server and mentions
// know them) and can have two nicknames on top:
//
//   - a global nickname, part of the profile (account.json, synced to each
//     server with the bio — see SaveProfile), shown on every server;
//   - a server nickname, stored by one server only (SetServerNickname),
//     shown there instead of the global one.
//
// Each server sends both maps with its member list ("users"); the page
// shows the server nickname, else the global one, else the username.

// maxNicknameChars matches the server's MAX_NICKNAME_CHARS.
const maxNicknameChars = 32

// cleanNickname trims a nickname and collapses runs of whitespace, the way
// the server stores it, and refuses one that's too long or has characters
// that could make it look blank or like someone else (control characters,
// zero-width spaces, direction overrides…). "" means none.
func cleanNickname(raw string) (string, error) {
	nick := strings.Join(strings.Fields(raw), " ")
	if utf8.RuneCountInString(nick) > maxNicknameChars {
		return "", fmt.Errorf("nicknames can be at most %d characters", maxNicknameChars)
	}
	for _, r := range nick {
		if unicode.IsControl(r) || invisibleRune(r) {
			return "", fmt.Errorf("that nickname has characters that can't be used")
		}
	}
	return nick, nil
}

// invisibleRune: formatting characters that draw nothing (the same set the
// server refuses).
func invisibleRune(r rune) bool {
	switch {
	case r >= 0x200B && r <= 0x200F, r >= 0x202A && r <= 0x202E, r >= 0x2060 && r <= 0x206F:
		return true
	}
	switch r {
	case 0xFEFF, 0x00AD, 0x3164, 0x115F, 0x1160:
		return true
	}
	return false
}

// SetServerNickname sets (or, with "", clears) your nickname on the server
// you're connected to. The server answers by sending everyone the member
// list again (chat:users), with the new name in it; a refusal comes back as
// an action_denied message.
func (a *App) SetServerNickname(nickname string) error {
	nickname, err := cleanNickname(nickname)
	if err != nil {
		return err
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if a.ws == nil {
		return fmt.Errorf("not connected to a server")
	}
	msg, _ := json.Marshal(map[string]interface{}{"type": "set_nickname", "nickname": nickname})
	return a.ws.WriteMessage(websocket.TextMessage, msg)
}

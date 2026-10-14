package main

// moderation.go — acting on other members, where your roles allow it. The
// server checks the permission and that the other person's highest role is
// lower than yours, and refuses otherwise (an action_denied message).

import (
	"encoding/json"
	"fmt"

	"github.com/gorilla/websocket"
)

// ServerMuteMember server-mutes (or unmutes) someone in voice: nobody hears
// them until it's lifted — the server stops passing their microphone on.
// Needs Mute Members.
func (a *App) ServerMuteMember(user string, muted bool) error {
	return a.sendVoiceJSON(map[string]interface{}{"type": "voice_server_mute", "target": user, "muted": muted})
}

// ServerDeafenMember server-deafens (or undeafens) someone: they hear no one
// in voice until it's lifted. Needs Deafen Members.
func (a *App) ServerDeafenMember(user string, deafened bool) error {
	return a.sendVoiceJSON(map[string]interface{}{"type": "voice_server_deafen", "target": user, "deafened": deafened})
}

// MoveMember moves someone in a call to another voice channel (boardID), or
// disconnects them from voice ("" ). Needs Move Members.
func (a *App) MoveMember(user, boardID string) error {
	return a.sendVoiceJSON(map[string]interface{}{"type": "voice_move", "target": user, "board_id": boardID})
}

// SetMemberNickname sets (or, with "", clears) someone else's nickname on
// this server. Needs Manage Nicknames.
func (a *App) SetMemberNickname(user, nickname string) error {
	nickname, err := cleanNickname(nickname)
	if err != nil {
		return err
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if a.ws == nil {
		return fmt.Errorf("not connected to a server")
	}
	msg, _ := json.Marshal(map[string]interface{}{"type": "set_member_nickname", "target": user, "nickname": nickname})
	return a.ws.WriteMessage(websocket.TextMessage, msg)
}

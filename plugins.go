package main

// plugins.go — server plugins in the app (the server's plugins.rs).
//
// A plugin is a program the server runs, here as a member of its own. The
// app shows each one's panel (drawn from the description the plugin sends:
// plugins never run code in the app) and lets you type its commands in the
// message box ("music::play …"). Commands and clicks go to the plugin
// through the server, on this connection, rather than as chat messages:
// sealed for the plugin when it takes lanes (lanes_app.go), else as they
// are (the plugin runs on the server, so the server sees them anyway).

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// ServerPlugins is the server's plugins for members (JSON, as the server
// sends it): their commands, and the panels you can see.
func (a *App) ServerPlugins() (string, error) {
	data, err := a.serverBotCall("GET", "/api/plugins", nil)
	if err != nil {
		return "", err
	}
	return string(a.laneClient().listed(data)), nil // lanes_app.go
}

// PluginCommand sends a command typed in the message box to a plugin: args
// maps each argument's name to what was typed. Its answer arrives as a
// "plugin:event" (plugin_reply).
func (a *App) PluginCommand(plugin, command string, args map[string]string, boardID string) error {
	if plugin == "" || command == "" {
		return errors.New("which plugin and command?")
	}
	if args == nil {
		args = map[string]string{}
	}
	if lc := a.laneClient(); lc.uses(plugin) {
		return lc.command(plugin, command, args, boardID) // sealed (lanes_app.go)
	}
	return a.sendWSChecked(map[string]interface{}{"type": "plugin_command", "plugin": plugin, "command": command,
		"data": args, "board_id": boardID})
}

// PluginAction sends a click in a plugin's panel (with what was typed in
// its box, for an input). boardID is the channel the panel is about ("" for
// one shown to everyone).
func (a *App) PluginAction(plugin, action, value, boardID string) error {
	if plugin == "" || action == "" {
		return errors.New("which plugin and action?")
	}
	if lc := a.laneClient(); lc.uses(plugin) {
		return lc.action(plugin, action, value, boardID)
	}
	m := map[string]interface{}{"type": "plugin_action", "plugin": plugin, "action": action}
	if value != "" {
		m["value"] = value
	}
	if boardID != "" {
		m["board_id"] = boardID
	}
	return a.sendWSChecked(m)
}

// sendWSChecked is sendWS, saying when it couldn't send.
func (a *App) sendWSChecked(v interface{}) error {
	msg, err := json.Marshal(v)
	if err != nil {
		return err
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if a.ws == nil {
		return errors.New("not connected to the server right now")
	}
	return a.ws.WriteMessage(websocket.TextMessage, msg)
}

// pluginEvent passes a plugin message from the server on to the page, as
// it came (plugin_panel, plugin_reply, plugins_updated). Reports whether
// raw was one.
func (a *App) pluginEvent(raw []byte) bool {
	if !bytes.Contains(raw, []byte(`"plugin`)) {
		return false
	}
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &head) != nil || !strings.HasPrefix(head.Type, "plugin") {
		return false
	}
	switch head.Type {
	case "plugin_panel", "plugin_reply", "plugins_updated":
		runtime.EventsEmit(a.ctx, "plugin:event", string(raw))
	case "plugin_lane", "plugin_lane_gone":
		a.laneClient().incoming(raw)
	}
	return true
}

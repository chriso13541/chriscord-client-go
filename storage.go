package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const serversFileName = "chriscord_servers.json"

func serversFilePath() string {
	return filepath.Join(chriscordConfigDir(), serversFileName)
}

func loadServers() []SavedServer {
	data, err := os.ReadFile(serversFilePath())
	if err != nil {
		// Fall back to the legacy location — a bare filename, which Go
		// resolves relative to the current working directory (i.e. next to
		// the executable). That's where this lived before server data
		// moved into the proper per-user config directory alongside
		// accounts. If found there, migrate it forward and remove the old
		// copy, so nobody's saved server list just disappears and nothing
		// chriscord-related is left sitting next to the exe.
		legacy, legacyErr := os.ReadFile(serversFileName)
		if legacyErr != nil {
			return []SavedServer{}
		}
		data = legacy
		if os.MkdirAll(chriscordConfigDir(), 0700) == nil {
			if os.WriteFile(serversFilePath(), legacy, 0644) == nil {
				os.Remove(serversFileName)
			}
		}
	}
	var servers []SavedServer
	if err := json.Unmarshal(data, &servers); err != nil {
		return []SavedServer{}
	}
	return servers
}

func saveServers(servers []SavedServer) {
	data, err := json.MarshalIndent(servers, "", "  ")
	if err != nil {
		return
	}
	os.MkdirAll(chriscordConfigDir(), 0700) // ensure the directory exists before writing into it
	os.WriteFile(serversFilePath(), data, 0644)
}

func upsertServer(servers []SavedServer, s SavedServer) []SavedServer {
	for i, existing := range servers {
		if existing.Domain == s.Domain {
			servers[i] = s
			return servers
		}
	}
	return append(servers, s)
}

func removeServer(servers []SavedServer, domain string) []SavedServer {
	out := servers[:0]
	for _, s := range servers {
		if s.Domain != domain {
			out = append(out, s)
		}
	}
	return out
}

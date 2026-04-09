package main

import (
	"encoding/json"
	"os"
)

const serversFile = "chriscord_servers.json"

func loadServers() []SavedServer {
	data, err := os.ReadFile(serversFile)
	if err != nil {
		return []SavedServer{}
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
	os.WriteFile(serversFile, data, 0644)
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

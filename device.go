package main

// device.go — this computer, as chriscord.app knows it.
//
// Each copy of the app makes a random device secret the first time it's
// needed and keeps it in chriscord_device.json. It's sent when signing in
// to an online account (only its hash is kept on chriscord.app), so a
// password sign-in from a computer the account hasn't been used on is
// confirmed by email first (cloud.go, waitApproval). It isn't an identity
// key and signs nothing.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

const deviceFile = "chriscord_device.json"

var deviceMu sync.Mutex

// thisDevice is this computer's device secret, and what to call it in the
// confirmation email ("DESKTOP-1234 (Windows)").
func thisDevice() (secret, name string) {
	deviceMu.Lock()
	defer deviceMu.Unlock()
	path := filepath.Join(chriscordConfigDir(), deviceFile)
	var f struct {
		Secret string `json:"secret"`
	}
	if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &f) == nil && len(f.Secret) == 64 {
		if _, err := hex.DecodeString(f.Secret); err == nil {
			return f.Secret, deviceName()
		}
	}
	b := make([]byte, 32)
	rand.Read(b)
	f.Secret = hex.EncodeToString(b)
	os.MkdirAll(chriscordConfigDir(), 0700)
	if data, err := json.Marshal(f); err == nil {
		os.WriteFile(path, data, 0600)
	}
	return f.Secret, deviceName()
}

func deviceName() string {
	host, _ := os.Hostname()
	host = strings.TrimSpace(host)
	osName := map[string]string{"windows": "Windows", "darwin": "Mac", "linux": "Linux"}[runtime.GOOS]
	if osName == "" {
		osName = runtime.GOOS
	}
	if host == "" {
		return "A " + osName + " computer"
	}
	return host + " (" + osName + ")"
}

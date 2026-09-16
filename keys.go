package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
)

// KeyPair holds the user's Ed25519 identity.
// The private key never leaves the device.
type KeyPair struct {
	PublicKey  string `json:"public_key"`  // hex-encoded 32 bytes
	PrivateKey string `json:"private_key"` // hex-encoded 64 bytes (seed+pubkey)
}

const keysFile = "chriscord_keys.json"

// LoadOrGenerateKeys loads the keypair from disk, generating a new one on first run.
func LoadOrGenerateKeys() (*KeyPair, error) {
	if data, err := os.ReadFile(keysFile); err == nil {
		var kp KeyPair
		if err := json.Unmarshal(data, &kp); err == nil &&
			len(kp.PublicKey) == 64 && len(kp.PrivateKey) == 128 {
			return &kp, nil
		}
	}

	// Generate fresh Ed25519 keypair
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}

	kp := &KeyPair{
		PublicKey:  hex.EncodeToString(pubKey),
		PrivateKey: hex.EncodeToString(privKey),
	}

	// Save with restricted permissions — private key stays local
	data, _ := json.MarshalIndent(kp, "", "  ")
	os.WriteFile(keysFile, data, 0600)

	return kp, nil
}

// SignNonce signs the hex-decoded nonce with the private key.
// Returns the hex-encoded signature.
func SignNonce(privKeyHex, nonceHex string) (string, error) {
	privBytes, err := hex.DecodeString(privKeyHex)
	if err != nil {
		return "", err
	}
	nonceBytes, err := hex.DecodeString(nonceHex)
	if err != nil {
		return "", err
	}

	privKey := ed25519.PrivateKey(privBytes)
	sig := ed25519.Sign(privKey, nonceBytes)
	return hex.EncodeToString(sig), nil
}

// Fingerprint returns a short human-readable identity for display.
// Format: "abcd1234...efgh5678" — first 8 + last 8 hex chars of public key.
func Fingerprint(pubKeyHex string) string {
	if len(pubKeyHex) < 16 {
		return pubKeyHex
	}
	return pubKeyHex[:8] + "…" + pubKeyHex[len(pubKeyHex)-8:]
}

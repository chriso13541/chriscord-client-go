package main

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The account key browser: lists what the export zip holds, the background
// kept in client-settings.json comes out as the JPEG it is, and nothing is
// shown or saved without an unlocked account.
func TestAccountKeyContents(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AppData", t.TempDir())
	dir := filepath.Join(accountsDir(), "chris")
	os.MkdirAll(dir, 0700)
	jpg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 1, 2, 3, 4, 0xFF, 0xD9}
	settings := `{"appearance":{"image":"data:image/jpeg;base64,` + base64.StdEncoding.EncodeToString(jpg) + `","blur":4},"spellcheck":true}`
	os.WriteFile(filepath.Join(dir, "identity.json"), []byte(`{"enc":"secret"}`), 0600)
	os.WriteFile(filepath.Join(dir, "account.json"), []byte(`{"username":"chris"}`), 0600)
	os.WriteFile(filepath.Join(dir, clientSettingsFile), []byte(settings), 0600)

	a := &App{}
	if _, err := a.AccountKeyContents(); err == nil {
		t.Fatal("listed an account key with no account unlocked")
	}
	if _, err := a.SaveAccountKeyFile("background.jpg"); err == nil {
		t.Fatal("saved a file with no account unlocked")
	}

	a.account = &Account{Slug: "chris"}
	files, err := a.AccountKeyContents()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]AccountFile{}
	var order []string
	for _, f := range files {
		byName[f.Name] = f
		order = append(order, f.Name)
	}
	if strings.Join(order, ",") != "identity.json,account.json,client-settings.json,background.jpg" {
		t.Fatalf("files %v", order)
	}
	if k := byName["identity.json"]; k.Kind != "key" || k.Preview != "" {
		t.Fatalf("the private key file was previewed: %+v", k)
	}
	bg := byName["background.jpg"]
	if bg.Kind != "image" || bg.From != clientSettingsFile || bg.Size != len(jpg) || !strings.HasPrefix(bg.Preview, "data:image/jpeg;base64,") {
		t.Fatalf("background %+v", bg)
	}
	if s := byName[clientSettingsFile].Preview; strings.Contains(s, "base64") || !strings.Contains(s, "[background.jpg]") {
		t.Fatalf("settings preview should name the image, not inline it: %s", s)
	}
	es, _ := accountEntries("chris")
	for _, e := range es {
		if e.Name == "background.jpg" && !bytes.Equal(e.data, jpg) {
			t.Fatal("the background's bytes changed")
		}
	}
	if _, err := a.SaveAccountKeyFile("identity.json"); err == nil {
		t.Fatal("identity.json saved on its own")
	}
	if _, err := a.SaveAccountKeyFile("../../etc/passwd"); err == nil {
		t.Fatal("saved something that isn't in the account key")
	}
	if _, err := accountEntries(".."); err == nil {
		t.Fatal("a slug outside the accounts folder was read")
	}
}

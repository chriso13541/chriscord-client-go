package main

// accountfiles.go — the account key browser (on the server list): what's
// in your account key (the .zip "Export Account Key" saves), each file
// viewable and savable on its own. Images kept inside client-settings.json
// (your background) are shown as files of their own, so they can be saved
// as the picture they are.
//
// Only for the account that's unlocked, and only its own folder: nothing
// here reads another account's files or anything outside the folder.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// AccountFile is one file in the account key, as the browser shows it.
type AccountFile struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`           // "image", "text" or "key"
	Size    int    `json:"size"`           // bytes
	From    string `json:"from,omitempty"` // kept inside this file (an image in client-settings.json)
	Note    string `json:"note,omitempty"`
	Preview string `json:"preview,omitempty"` // a data URL for an image; the text for text
}

type accountEntry struct {
	AccountFile
	data []byte
	mime string
}

// accountExportFiles: the files buildAccountExport puts in the zip (the
// same list), in the same order.
var accountExportFiles = []string{"identity.json", "account.json", "pfp.png", "banner.png", clientSettingsFile}

// previewTextMax: longer text is shown cut off (saving gives all of it).
const previewTextMax = 64 << 10

// accountEntries reads what the account key for slug holds.
func accountEntries(slug string) ([]accountEntry, error) {
	if slug == "" || strings.ContainsAny(slug, `/\`) || slug == "." || slug == ".." {
		return nil, fmt.Errorf("no account unlocked")
	}
	dir := filepath.Join(accountsDir(), slug)
	var out []accountEntry
	for _, name := range accountExportFiles {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue // pfp.png and banner.png are often absent
		}
		switch {
		case name == "identity.json":
			out = append(out, accountEntry{AccountFile: AccountFile{Name: name, Kind: "key", Size: len(data),
				Note: "Your account's private key, encrypted with your passphrase. To move your account, use Export Account Key instead."}, data: data, mime: "application/json"})
		case strings.HasSuffix(name, ".png"):
			out = append(out, accountEntry{AccountFile: AccountFile{Name: name, Kind: "image", Size: len(data)}, data: data, mime: "image/png"})
		case name == clientSettingsFile:
			text, images := splitSettingsImages(data)
			out = append(out, textEntry(name, data, text, "Your appearance, sound and app settings."))
			for _, im := range images {
				im.From = name
				out = append(out, im)
			}
		default:
			out = append(out, textEntry(name, data, data, ""))
		}
	}
	// The saved-server list goes in the zip too (servers.json).
	if servers := loadServers(); len(servers) > 0 {
		if data, err := json.MarshalIndent(servers, "", "  "); err == nil {
			out = append(out, textEntry("servers.json", data, data, "Your saved servers, with their join keys."))
		}
	}
	return out, nil
}

func textEntry(name string, data, shown []byte, note string) accountEntry {
	var pretty []byte
	var v interface{}
	if json.Unmarshal(shown, &v) == nil {
		pretty, _ = json.MarshalIndent(v, "", "  ")
	}
	if pretty == nil {
		pretty = shown
	}
	preview := string(pretty)
	if len(preview) > previewTextMax {
		preview = preview[:previewTextMax] + "\n…"
	}
	return accountEntry{AccountFile: AccountFile{Name: name, Kind: "text", Size: len(data), Note: note, Preview: preview}, data: data, mime: "application/json"}
}

// splitSettingsImages finds the images kept in the settings JSON as data
// URLs (the background is one) and returns them as files of their own,
// with the JSON as it's shown (each image replaced by its file's name).
func splitSettingsImages(data []byte) ([]byte, []accountEntry) {
	var v interface{}
	if err := json.Unmarshal(data, &v); err != nil {
		return data, nil
	}
	var images []accountEntry
	used := map[string]int{}
	var walk func(x interface{}, path []string) interface{}
	walk = func(x interface{}, path []string) interface{} {
		switch t := x.(type) {
		case map[string]interface{}:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys) // the same names every time
			for _, k := range keys {
				t[k] = walk(t[k], append(path, k))
			}
			return t
		case []interface{}:
			for i := range t {
				t[i] = walk(t[i], append(path, fmt.Sprint(i)))
			}
			return t
		case string:
			mime, raw, ok := decodeImageDataURL(t)
			if !ok {
				return t
			}
			base := imageFileBase(path)
			used[base]++
			if n := used[base]; n > 1 {
				base = fmt.Sprintf("%s-%d", base, n)
			}
			name := base + imageExt(mime)
			images = append(images, accountEntry{AccountFile: AccountFile{Name: name, Kind: "image", Size: len(raw),
				Note: imageNote(path)}, data: raw, mime: mime})
			return "[" + name + "]"
		}
		return x
	}
	v = walk(v, nil)
	shown, err := json.Marshal(v)
	if err != nil {
		return data, images
	}
	return shown, images
}

// decodeImageDataURL: the bytes of a base64 "data:image/…" URL.
func decodeImageDataURL(s string) (mime string, raw []byte, ok bool) {
	if !strings.HasPrefix(s, "data:image/") {
		return "", nil, false
	}
	comma := strings.IndexByte(s, ',')
	if comma < 0 {
		return "", nil, false
	}
	head := s[len("data:"):comma]
	if !strings.HasSuffix(head, ";base64") {
		return "", nil, false
	}
	mime = strings.TrimSuffix(head, ";base64")
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	raw, err := base64.StdEncoding.DecodeString(s[comma+1:])
	if err != nil || len(raw) == 0 {
		return "", nil, false
	}
	return mime, raw, true
}

func imageFileBase(path []string) string {
	if len(path) >= 2 && path[len(path)-2] == "appearance" && path[len(path)-1] == "image" {
		return "background"
	}
	if len(path) == 0 {
		return "image"
	}
	var parts []string
	for _, p := range path {
		p = strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
				return r
			}
			return -1
		}, p)
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return "image"
	}
	return strings.Join(parts, "-")
}

func imageNote(path []string) string {
	if imageFileBase(path) == "background" {
		return "Your background (Settings → Appearance), as the app keeps it: resized to fit 1920×1080 when you chose it."
	}
	return ""
}

func imageExt(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/avif":
		return ".avif"
	case "image/bmp":
		return ".bmp"
	}
	return ".img"
}

func (a *App) unlockedSlug() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.account == nil {
		return ""
	}
	return a.account.Slug
}

// AccountKeyContents lists what's in your account key, with previews.
// Only while an account is unlocked.
func (a *App) AccountKeyContents() ([]AccountFile, error) {
	slug := a.unlockedSlug()
	if slug == "" {
		return nil, fmt.Errorf("no account unlocked")
	}
	es, err := accountEntries(slug)
	if err != nil {
		return nil, err
	}
	out := make([]AccountFile, 0, len(es))
	for _, e := range es {
		f := e.AccountFile
		if f.Kind == "image" {
			f.Preview = "data:" + e.mime + ";base64," + base64.StdEncoding.EncodeToString(e.data)
		}
		out = append(out, f)
	}
	return out, nil
}

// SaveAccountKeyFile saves one file from your account key where you
// choose; returns where ("" if the dialog was cancelled). identity.json
// isn't saved on its own: Export Account Key is how to move an account.
func (a *App) SaveAccountKeyFile(name string) (string, error) {
	slug := a.unlockedSlug()
	if slug == "" {
		return "", fmt.Errorf("no account unlocked")
	}
	es, err := accountEntries(slug)
	if err != nil {
		return "", err
	}
	for _, e := range es {
		if e.Name != name {
			continue
		}
		if e.Kind == "key" {
			return "", fmt.Errorf("use Export Account Key to save your account key")
		}
		ext := filepath.Ext(e.Name)
		dest, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
			Title: "Save " + e.Name, DefaultFilename: e.Name,
			Filters: []runtime.FileFilter{{DisplayName: strings.ToUpper(strings.TrimPrefix(ext, ".")) + " file", Pattern: "*" + ext}},
		})
		if err != nil || dest == "" {
			return "", err
		}
		if err := os.WriteFile(dest, e.data, 0600); err != nil {
			return "", err
		}
		return dest, nil
	}
	return "", fmt.Errorf("%s isn't in your account key", name)
}

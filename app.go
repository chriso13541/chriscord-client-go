package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gordonklaus/portaudio"
	"github.com/gorilla/websocket"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

type App struct {
	ctx              context.Context
	mu               sync.Mutex
	writeMu          sync.Mutex
	ws               *websocket.Conn
	token            string
	domain           string
	username         string
	fingerprint      string
	account          *Account
	servers          []SavedServer
	voice            *VoiceSession
	voiceMu          sync.Mutex
	voiceMuted       atomic.Bool
	voiceDeafened    atomic.Bool
	// presence is the status this client shows ("online", "idle" or
	// "invisible") — sent on connect and whenever the frontend changes it.
	// Stored in an atomic.Value so it can be read while a.mu is held.
	presence         atomic.Value
	// pendingExport holds a built, encrypted account key between
	// PrepareAccountExport and SaveAccountExport (see those).
	pendingExport    []byte
	pendingExportAs  string
}

func NewApp() *App { return &App{} }

func (a *App) startup(ctx context.Context) {
	a.ctx    = ctx
	a.servers = loadServers()
	if err := portaudio.Initialize(); err != nil {
		runtime.LogWarningf(ctx, "portaudio init failed, voice audio won't work: %v", err)
	}
}

func normaliseHTTP(domain string) string {
	d := strings.TrimRight(domain, "/")
	if !strings.HasPrefix(d, "http://") && !strings.HasPrefix(d, "https://") {
		return "http://" + d
	}
	return d
}
func httpToWS(u string) string {
	u = strings.Replace(u, "https://", "wss://", 1)
	return strings.Replace(u, "http://", "ws://", 1)
}

func (a *App) doGET(path string, out interface{}) error {
	req, err := http.NewRequest("GET", normaliseHTTP(a.domain)+path, nil)
	if err != nil { return err }
	req.Header.Set("X-Session-Token", a.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil { return fmt.Errorf("network error: %w", err) }
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e map[string]string
		json.NewDecoder(resp.Body).Decode(&e)
		if msg, ok := e["error"]; ok { return fmt.Errorf("%s", msg) }
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func postJSON(url string, body, out interface{}) error {
	data, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil { return fmt.Errorf("network error: %w", err) }
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e map[string]string
		json.NewDecoder(resp.Body).Decode(&e)
		if msg, ok := e["error"]; ok { return fmt.Errorf("%s", msg) }
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if out != nil { return json.NewDecoder(resp.Body).Decode(out) }
	return nil
}

// ── Exposed to frontend ───────────────────────────────────────────────────────

func (a *App) GetServers() []SavedServer {
	if a.servers == nil { return []SavedServer{} }
	return a.servers
}

// ── Account ──────────────────────────────────────────────────────────────

// HasAccount reports whether an account is set up locally — the frontend
// uses this at startup to decide between onboarding and the unlock prompt.
func (a *App) HasAccount() bool { return HasAnyAccount() }

// ListAccounts is available for a future account switcher; not wired into
// any UI flow yet, but doesn't unlock anything to produce its answer.
func (a *App) ListAccounts() []AccountSummary { return ListAccounts() }

func (a *App) CreateAccount(username, passphrase, pfpPath string) (*AccountView, error) {
	acct, err := CreateAccount(username, passphrase, pfpPath)
	if err != nil { return nil, err }
	a.mu.Lock(); a.account = acct; a.mu.Unlock()
	return acct.View(), nil
}

func (a *App) ImportAccount(sourcePath, passphrase string) (*AccountView, error) {
	acct, err := ImportAccount(sourcePath, passphrase)
	if err != nil { return nil, err }
	a.mu.Lock()
	a.account = acct
	// The import merged the bundle's servers into servers.json on disk.
	// Reload them — the in-memory list was read at startup, and the next
	// Connect() saves that list back, which used to write the imported
	// servers straight back out of the file.
	a.servers = loadServers()
	a.mu.Unlock()
	return acct.View(), nil
}

// UnlockAccount decrypts the active account into memory for this session.
// Called once per launch; after this, every Connect() reuses the decrypted
// key without touching disk again.
func (a *App) UnlockAccount(passphrase string) (*AccountView, error) {
	acct, err := UnlockActiveAccount(passphrase)
	if err != nil { return nil, err }
	a.mu.Lock(); a.account = acct; a.mu.Unlock()
	return acct.View(), nil
}

func (a *App) ExportAccount(passphrase string) (string, error) {
	a.mu.Lock(); slug := ""; if a.account != nil { slug = a.account.Slug }; a.mu.Unlock()
	if slug == "" { return "", fmt.Errorf("no account unlocked") }
	encrypted, finalName, err := buildAccountExport(slug, passphrase)
	if err != nil {
		return "", err
	}
	destPath, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title: "Export account key", DefaultFilename: finalName,
		Filters: []runtime.FileFilter{{DisplayName: "Account key", Pattern: "*.zip"}},
	})
	if err != nil {
		return "", err
	}
	if destPath == "" {
		return "", nil // user canceled the dialog — not an error, just nothing to save
	}
	if err := os.WriteFile(destPath, encrypted, 0600); err != nil {
		return "", err
	}
	return destPath, nil
}

// PrepareAccountExport checks the passphrase and builds the encrypted
// account key (the slow part — key derivation), keeping it in memory, and
// returns the file name it will be saved as. SaveAccountExport then asks
// where to save it. Split in two so the save dialog is opened by a mouse
// click on its own button, not straight from pressing Enter in the
// passphrase box: on Windows, typing hides the pointer, and a dialog that
// opens while it's hidden could come up with no visible cursor.
func (a *App) PrepareAccountExport(passphrase string) (string, error) {
	a.mu.Lock(); slug := ""; if a.account != nil { slug = a.account.Slug }; a.mu.Unlock()
	if slug == "" { return "", fmt.Errorf("no account unlocked") }
	encrypted, finalName, err := buildAccountExport(slug, passphrase)
	if err != nil {
		return "", err
	}
	a.mu.Lock(); a.pendingExport, a.pendingExportAs = encrypted, finalName; a.mu.Unlock()
	return finalName, nil
}

// SaveAccountExport shows the save dialog for the key built by
// PrepareAccountExport and writes it. Returns "" if the dialog was
// cancelled (the prepared key is kept, so Save can be tried again).
func (a *App) SaveAccountExport() (string, error) {
	a.mu.Lock(); data, name := a.pendingExport, a.pendingExportAs; a.mu.Unlock()
	if data == nil {
		return "", fmt.Errorf("nothing to save — enter your passphrase first")
	}
	destPath, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title: "Export account key", DefaultFilename: name,
		Filters: []runtime.FileFilter{{DisplayName: "Account key", Pattern: "*.zip"}},
	})
	if err != nil || destPath == "" {
		return "", err
	}
	if err := os.WriteFile(destPath, data, 0600); err != nil {
		return "", err
	}
	a.mu.Lock(); a.pendingExport, a.pendingExportAs = nil, ""; a.mu.Unlock()
	return destPath, nil
}

// CancelAccountExport drops a prepared, unsaved account key.
func (a *App) CancelAccountExport() {
	a.mu.Lock(); a.pendingExport, a.pendingExportAs = nil, ""; a.mu.Unlock()
}

// ── Client settings (theme etc.), stored with the account ──────────────

// clientSettingsFile holds the app's look-and-feel settings (theme,
// background, elastic scroll…) inside the account folder, so they travel
// in the exported account key and come back on import.
const clientSettingsFile = "client-settings.json"

// SaveClientSettings stores the frontend's settings JSON for this account.
func (a *App) SaveClientSettings(settingsJSON string) error {
	a.mu.Lock(); slug := ""; if a.account != nil { slug = a.account.Slug }; a.mu.Unlock()
	if slug == "" { return fmt.Errorf("no account unlocked") }
	if !json.Valid([]byte(settingsJSON)) { return fmt.Errorf("settings aren't valid JSON") }
	if len(settingsJSON) > 16<<20 { return fmt.Errorf("settings are too large") }
	return os.WriteFile(filepath.Join(accountsDir(), slug, clientSettingsFile), []byte(settingsJSON), 0600)
}

// GetClientSettings returns this account's stored settings JSON, or "".
func (a *App) GetClientSettings() string {
	a.mu.Lock(); slug := ""; if a.account != nil { slug = a.account.Slug }; a.mu.Unlock()
	if slug == "" { return "" }
	data, err := os.ReadFile(filepath.Join(accountsDir(), slug, clientSettingsFile))
	if err != nil { return "" }
	return string(data)
}

func (a *App) GetAccountInfo() *AccountView {
	a.mu.Lock(); defer a.mu.Unlock()
	return a.account.View()
}

func (a *App) PickAccountFile() (string, error) {
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title:   "Select account_key.zip",
		Filters: []runtime.FileFilter{{DisplayName: "Account key", Pattern: "*.zip"}},
	})
}

func (a *App) PickAccountFolder() (string, error) {
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{Title: "Select account folder"})
}

func (a *App) PickPfp() (string, error) {
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title:   "Select profile picture",
		Filters: []runtime.FileFilter{{DisplayName: "Images", Pattern: profileImagePattern}},
	})
}

// ReadImageAsDataURL reads an arbitrary image file — typically one just
// picked via PickPfp — and returns it as a data URL, so the frontend can
// load it into an <img>/canvas for cropping. Wails doesn't give JS direct
// filesystem access to a path picked from a native file dialog, so this
// is the bridge for that.
func (a *App) ReadImageAsDataURL(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return pfpDataURL(data), nil
}

// GetProfilePicture returns the current account's own profile picture as
// a data URL, or "" if it doesn't have one — an account without a pfp is
// a completely normal, expected state, not a failure, so this returns a
// nil error either way; a read failure is treated the same as "no pfp"
// rather than surfaced, since there's nothing actionable the caller could
// do about it besides showing the same empty state anyway.
func (a *App) GetProfilePicture() (string, error) {
	a.mu.Lock()
	path := ""
	if a.account != nil && a.account.HasAvatar {
		path = a.account.AvatarPath
	}
	a.mu.Unlock()
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("pfp is set (%s) but couldn't be read: %w", path, err)
	}
	return pfpDataURL(data), nil
}

// maxPfpBytes caps a saved/uploaded pfp. The cropper's output is far
// smaller than this (1024px max); it mainly guards against an oversized
// file copied in raw at account creation. Matches the server's own cap.
const maxPfpBytes = 8 << 20

// pfpMime sniffs the real image type from the bytes themselves rather than
// trusting a file name — pfp.png keeps its name inside account bundles for
// compatibility with existing exports, but may hold a JPEG now.
func pfpMime(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	case len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	case len(data) > 26 && bytes.HasPrefix(data, []byte("BM")):
		return "image/bmp"
	case len(data) >= 12 && string(data[4:8]) == "ftyp" && (string(data[8:12]) == "avif" || string(data[8:12]) == "avis"):
		return "image/avif"
	}
	return ""
}

// isProfileImage reports whether data is a type allowed for profile
// pictures and banners — the common still formats plus animated GIF,
// WebP and APNG (animated files are kept byte-for-byte so they keep
// moving). Matches the server's is_profile_image.
func isProfileImage(data []byte) bool { return pfpMime(data) != "" }

// profileImagePattern is the file-dialog filter for pfps and banners.
const profileImagePattern = "*.png;*.apng;*.jpg;*.jpeg;*.gif;*.webp;*.bmp;*.avif"

func pfpDataURL(data []byte) string {
	mime := pfpMime(data)
	if mime == "" {
		mime = "image/png"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// SaveProfilePicture decodes an image data URL — the cropped result from the
// frontend's own canvas — and saves it as the current account's pfp.png,
// overwriting any existing one.
func (a *App) SaveProfilePicture(dataURL string) error {
	a.mu.Lock()
	slug := ""
	if a.account != nil {
		slug = a.account.Slug
	}
	a.mu.Unlock()
	if slug == "" {
		return fmt.Errorf("no account unlocked")
	}
	comma := strings.Index(dataURL, ",")
	if !strings.HasPrefix(dataURL, "data:image/") || comma < 0 || !strings.HasSuffix(dataURL[:comma], ";base64") {
		return fmt.Errorf("expected a base64 image data URL")
	}
	data, err := base64.StdEncoding.DecodeString(dataURL[comma+1:])
	if err != nil {
		return fmt.Errorf("invalid image data: %w", err)
	}
	if !isProfileImage(data) {
		return fmt.Errorf("profile pictures can be PNG, JPEG, GIF, WebP, BMP or AVIF")
	}
	if len(data) > maxPfpBytes {
		return fmt.Errorf("profile picture is too large (%d MB max)", maxPfpBytes>>20)
	}
	dir := filepath.Join(accountsDir(), slug)
	dest := filepath.Join(dir, "pfp.png")
	if err := os.WriteFile(dest, data, 0600); err != nil {
		return err
	}
	updatedAt := time.Now().Unix()
	_ = updatePfpTimestamp(dir, updatedAt) // best-effort — the pfp itself is already saved regardless
	a.mu.Lock()
	if a.account != nil {
		a.account.HasAvatar = true
		a.account.AvatarPath = dest
		a.account.PfpUpdatedAt = updatedAt
	}
	a.mu.Unlock()
	a.sendPfpInfo(updatedAt) // tell the server right away, rather than waiting for the next reconnect
	return nil
}

// sendPfpInfo reports this account's own pfp timestamp to the server, so
// it can tell whether its cached copy (if any) is still current — sent
// once on every connect, and again immediately whenever the picture
// changes while already connected. Best-effort: silently does nothing if
// not currently connected, matching the other send helpers in this file.
func (a *App) sendPfpInfo(updatedAt int64) {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if a.ws == nil {
		return
	}
	msg, _ := json.Marshal(map[string]interface{}{"type": "pfp_info", "pfp_updated_at": updatedAt})
	_ = a.ws.WriteMessage(websocket.TextMessage, msg)
}

// handlePfpRequest reads this account's own current pfp and uploads it —
// called when the server reports it doesn't have a current cached copy
// (see sendPfpInfo above for the other half of this handshake).
func (a *App) handlePfpRequest() {
	a.mu.Lock()
	path := ""
	updatedAt := int64(0)
	if a.account != nil && a.account.HasAvatar {
		path = a.account.AvatarPath
		updatedAt = a.account.PfpUpdatedAt
	}
	a.mu.Unlock()
	if path == "" {
		return // nothing to upload — the server asked before we had anything, or we don't have a pfp at all
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("pfp: failed to read own pfp for upload: %v", err)
		return
	}
	if len(data) > maxPfpBytes {
		log.Printf("pfp: own pfp is %d bytes, over the %d byte limit — re-save it from settings to shrink it", len(data), maxPfpBytes)
		return
	}
	log.Printf("pfp: uploading %d bytes read from %s", len(data), path)
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if a.ws == nil {
		return
	}
	msg, _ := json.Marshal(map[string]interface{}{
		"type": "pfp_upload", "pfp_data": base64.StdEncoding.EncodeToString(data), "pfp_updated_at": updatedAt,
	})
	_ = a.ws.WriteMessage(websocket.TextMessage, msg)
}

// ── Profile card (bio + banner) ─────────────────────────────────────────

// maxBioChars matches the server's MAX_BIO_CHARS.
const maxBioChars = 500

// OwnProfile is what the profile settings page edits.
type OwnProfile struct {
	Bio    string `json:"bio"`
	Banner string `json:"banner"` // data URL, "" = no banner (colour from the pfp instead)
}

// GetOwnProfile returns this account's own bio and banner.
func (a *App) GetOwnProfile() (*OwnProfile, error) {
	a.mu.Lock()
	if a.account == nil {
		a.mu.Unlock()
		return nil, fmt.Errorf("no account unlocked")
	}
	dir := filepath.Join(accountsDir(), a.account.Slug)
	p := &OwnProfile{Bio: a.account.Bio}
	a.mu.Unlock()
	if data, err := os.ReadFile(bannerPath(dir)); err == nil {
		p.Banner = pfpDataURL(data)
	}
	return p, nil
}

// SaveProfile stores a new bio and banner for this account and pushes
// them to the connected server. banner is the complete desired state: a
// data URL to set, or "" for no banner.
func (a *App) SaveProfile(bio, banner string) error {
	bio = strings.TrimSpace(bio)
	if n := len([]rune(bio)); n > maxBioChars {
		return fmt.Errorf("bio is %d characters — %d max", n, maxBioChars)
	}
	a.mu.Lock()
	slug := ""
	if a.account != nil {
		slug = a.account.Slug
	}
	a.mu.Unlock()
	if slug == "" {
		return fmt.Errorf("no account unlocked")
	}
	dir := filepath.Join(accountsDir(), slug)
	if banner == "" {
		if err := os.Remove(bannerPath(dir)); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else {
		comma := strings.Index(banner, ",")
		if !strings.HasPrefix(banner, "data:image/") || comma < 0 || !strings.HasSuffix(banner[:comma], ";base64") {
			return fmt.Errorf("expected a base64 image data URL for the banner")
		}
		data, err := base64.StdEncoding.DecodeString(banner[comma+1:])
		if err != nil {
			return fmt.Errorf("invalid banner data: %w", err)
		}
		if !isProfileImage(data) {
			return fmt.Errorf("banners can be PNG, JPEG, GIF, WebP, BMP or AVIF")
		}
		if len(data) > maxPfpBytes {
			return fmt.Errorf("banner is too large (%d MB max)", maxPfpBytes>>20)
		}
		if err := os.WriteFile(bannerPath(dir), data, 0600); err != nil {
			return err
		}
	}
	updatedAt := time.Now().Unix()
	if err := updateProfileMeta(dir, bio, updatedAt); err != nil {
		return err
	}
	a.mu.Lock()
	if a.account != nil {
		a.account.Bio = bio
		a.account.ProfileUpdatedAt = updatedAt
	}
	a.mu.Unlock()
	a.sendProfileInfo(updatedAt)
	return nil
}

// sendProfileInfo reports this account's profile timestamp so the server
// can ask for an upload if its cached copy is stale — the profile
// equivalent of sendPfpInfo.
func (a *App) sendProfileInfo(updatedAt int64) {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if a.ws == nil || updatedAt == 0 {
		return
	}
	msg, _ := json.Marshal(map[string]interface{}{"type": "profile_info", "profile_updated_at": updatedAt})
	_ = a.ws.WriteMessage(websocket.TextMessage, msg)
}

// handleProfileRequest uploads this account's bio and banner.
func (a *App) handleProfileRequest() {
	a.mu.Lock()
	if a.account == nil || a.account.ProfileUpdatedAt == 0 {
		a.mu.Unlock()
		return
	}
	dir := filepath.Join(accountsDir(), a.account.Slug)
	bio, updatedAt := a.account.Bio, a.account.ProfileUpdatedAt
	a.mu.Unlock()
	banner := ""
	if data, err := os.ReadFile(bannerPath(dir)); err == nil && len(data) <= maxPfpBytes {
		banner = base64.StdEncoding.EncodeToString(data)
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if a.ws == nil {
		return
	}
	msg, _ := json.Marshal(map[string]interface{}{
		"type": "profile_upload", "profile_updated_at": updatedAt, "bio": bio, "banner_data": banner,
	})
	_ = a.ws.WriteMessage(websocket.TextMessage, msg)
}

// UserProfile is another user's profile card as fetched from the server.
type UserProfile struct {
	Username    string `json:"username"`
	Bio         string `json:"bio"`
	Banner      string `json:"banner"` // data URL, or "" for none
	MemberSince string `json:"member_since"`
}

// FetchUserProfile fetches a user's bio, banner and join date from the
// connected server.
func (a *App) FetchUserProfile(username string) (*UserProfile, error) {
	var raw struct {
		Username    string `json:"username"`
		Bio         string `json:"bio"`
		HasBanner   bool   `json:"has_banner"`
		MemberSince string `json:"member_since"`
	}
	if err := a.doGET("/api/profile/"+url.PathEscape(username), &raw); err != nil {
		return nil, err
	}
	p := &UserProfile{Username: raw.Username, Bio: raw.Bio, MemberSince: raw.MemberSince}
	if raw.HasBanner {
		if data, err := a.fetchAuthed("/api/banner/" + url.PathEscape(username)); err == nil && len(data) > 0 {
			p.Banner = pfpDataURL(data)
		}
	}
	return p, nil
}

// fetchAuthed GETs a server path with the session token and returns the
// raw body (nil, nil on 404).
func (a *App) fetchAuthed(path string) ([]byte, error) {
	a.mu.Lock()
	domain, token := a.domain, a.token
	a.mu.Unlock()
	if domain == "" || token == "" {
		return nil, fmt.Errorf("not connected")
	}
	req, err := http.NewRequest("GET", normaliseHTTP(domain)+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Session-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network error: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// PickBanner opens a file dialog for a profile banner image.
func (a *App) PickBanner() (string, error) {
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title:   "Select profile banner",
		Filters: []runtime.FileFilter{{DisplayName: "Images", Pattern: profileImagePattern}},
	})
}

// PickBackgroundImage opens a file dialog for the app background image
// (Settings → Appearance).
func (a *App) PickBackgroundImage() (string, error) {
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title:   "Select background image",
		Filters: []runtime.FileFilter{{DisplayName: "Images", Pattern: "*.png;*.jpg;*.jpeg;*.webp;*.bmp;*.avif;*.gif"}},
	})
}

// FetchUserPfp fetches another user's cached profile picture from the
// server and returns it as a data URL, or "" if they have none cached —
// nil error either way, since "no pfp" is a normal state, not a failure.
func (a *App) FetchUserPfp(username string) (string, error) {
	a.mu.Lock()
	domain, token := a.domain, a.token
	a.mu.Unlock()
	if domain == "" || token == "" {
		return "", fmt.Errorf("not connected")
	}
	req, err := http.NewRequest("GET", normaliseHTTP(domain)+"/api/pfp/"+url.PathEscape(username), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Session-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("network error: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil // this user simply has no pfp cached — not an error
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	log.Printf("pfp: fetched %d bytes for %s", len(data), username)
	return pfpDataURL(data), nil
}

func (a *App) GetServerInfo(domain string) (*ServerInfo, error) {
	resp, err := http.Get(normaliseHTTP(domain) + "/api/info")
	if err != nil { return nil, fmt.Errorf("cannot reach server: %w", err) }
	defer resp.Body.Close()
	var info ServerInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil { return nil, err }
	return &info, nil
}

// PingServer times a real round trip to the currently connected host, in
// milliseconds. Reuses the existing unauthenticated /api/info endpoint
// rather than needing a dedicated ping route.
func (a *App) PingServer() (int64, error) {
	a.mu.Lock(); domain := a.domain; a.mu.Unlock()
	if domain == "" { return 0, fmt.Errorf("not connected") }
	client := &http.Client{Timeout: 5 * time.Second}
	start := time.Now()
	resp, err := client.Get(normaliseHTTP(domain) + "/api/info")
	if err != nil { return 0, fmt.Errorf("ping failed: %w", err) }
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return time.Since(start).Milliseconds(), nil
}

// requestChallenge asks the host for a nonce to sign, proving control of
// publicKeyHex without ever sending the private key over the wire.
func requestChallenge(base, publicKeyHex string) (string, error) {
	var resp ChallengeResponse
	body := map[string]string{"public_key": publicKeyHex}
	if err := postJSON(base+"/api/challenge", body, &resp); err != nil {
		return "", fmt.Errorf("challenge failed: %w", err)
	}
	return resp.Nonce, nil
}

// Connect joins domain using whichever account is currently unlocked (see
// UnlockAccount). The username comes from the account, not per-server —
// one identity, same name everywhere, same as Discord.
func (a *App) Connect(domain, serverKey string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.account == nil { return fmt.Errorf("no account unlocked") }
	if a.ws != nil { a.ws.Close(); a.ws = nil }

	base := normaliseHTTP(domain)
	pubHex := hex.EncodeToString(a.account.PublicKey)

	// 1. Get a nonce bound to our public key.
	nonce, err := requestChallenge(base, pubHex)
	if err != nil { return err }

	// 2. Sign the *raw* nonce bytes (the server hex-decodes before verifying),
	// not the hex string itself.
	nonceBytes, err := hex.DecodeString(nonce)
	if err != nil { return fmt.Errorf("server sent a malformed nonce: %w", err) }
	signature := ed25519.Sign(a.account.PrivateKey, nonceBytes)

	// 3. Join with the signed nonce.
	joinBody := map[string]interface{}{
		"username":   a.account.Username,
		"public_key": pubHex,
		"nonce":      nonce,
		"signature":  hex.EncodeToString(signature),
	}
	if serverKey != "" { joinBody["server_key"] = serverKey }
	var joinResp JoinResponse
	if err := postJSON(base+"/api/join", joinBody, &joinResp); err != nil { return err }

	a.domain      = domain
	a.token       = joinResp.Token
	a.username    = joinResp.Username
	a.fingerprint = joinResp.Fingerprint

	displayName := domain
	if info, err := a.GetServerInfo(domain); err == nil { displayName = info.Name }
	customName := ""
	for _, s := range a.servers {
		if s.Domain == domain { customName = s.CustomName } // keep a name you set yourself
	}
	a.servers = upsertServer(a.servers, SavedServer{
		Domain: domain, ServerKey: serverKey, DisplayName: displayName, LastUsername: a.username, CustomName: customName,
	})
	saveServers(a.servers)

	wsURL := httpToWS(base) + "/ws?token=" + a.token + "&status=" + url.QueryEscape(a.currentPresence())
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil { return fmt.Errorf("websocket failed: %w", err) }
	a.ws = conn
	go a.wsReader(conn)
	a.sendPfpInfo(a.account.PfpUpdatedAt)
	a.sendProfileInfo(a.account.ProfileUpdatedAt)
	return nil
}

type serverMsg struct {
	Type          string              `json:"type"`
	BoardID       string              `json:"board_id"`
	Data          *ChatMessage        `json:"data"`
	Messages      []ChatMessage       `json:"messages"`
	Online        []string            `json:"online"`
	// Presence on "users" messages: username → "online" | "idle". Its own
	// key — "statuses" is already the voice status list below.
	Presence      map[string]string   `json:"presence"`
	Owner         string              `json:"owner"`  // on "users": the crowned member
	Banned        bool                `json:"banned"` // on "kicked"
	Reason        string              `json:"reason"` // on "kicked"
	Typing        bool                `json:"typing"`
	All           []string            `json:"all"`
	ID            string              `json:"id"`
	Content       string              `json:"content"`
	Channels      map[string][]string `json:"channels"`
	SDP           string              `json:"sdp"`
	Candidate     string              `json:"candidate"`
	SDPMid        string              `json:"sdp_mid"`
	SDPMLineIndex *uint16             `json:"sdp_mline_index"`
	Username      string              `json:"username"`
	Speaking      bool                `json:"speaking"`
	Muted         bool                `json:"muted"`
	Deafened      bool                `json:"deafened"`
	Statuses      []VoiceStatusEntry  `json:"statuses"`
	PfpUpdatedAt  int64               `json:"updated_at"`
	Pinned        bool                `json:"pinned"`
	Emoji         string              `json:"emoji"`
	On            bool                `json:"on"`
	By            string              `json:"by"`
}

// VoiceStatusEntry is one participant's mute/deafen status, as sent in a
// voice_status_snapshot when joining a voice channel already in progress.
type VoiceStatusEntry struct {
	Username string `json:"username"`
	Muted    bool   `json:"muted"`
	Deafened bool   `json:"deafened"`
}

type historyEvent struct {
	BoardID  string        `json:"board_id"`
	Messages []ChatMessage `json:"messages"`
}
type usersEvent  struct { Online []string `json:"online"`; Statuses map[string]string `json:"statuses"`; All []string `json:"all"`; Owner string `json:"owner"` }
type kickedEvent struct { Banned bool `json:"banned"`; Reason string `json:"reason"` }
type typingEvent struct { Username string `json:"username"`; Typing bool `json:"typing"` }
type voiceStateEvent struct { Channels map[string][]string `json:"channels"` }
type editEvent   struct { ID string `json:"id"`; BoardID string `json:"board_id"`; Content string `json:"content"` }
type deleteEvent struct { ID string `json:"id"`; BoardID string `json:"board_id"` }
type pinEvent    struct { ID string `json:"id"`; BoardID string `json:"board_id"`; Pinned bool `json:"pinned"`; By string `json:"by"` }
type reactionEvent struct {
	ID       string `json:"id"`
	BoardID  string `json:"board_id"`
	Emoji    string `json:"emoji"`
	Username string `json:"username"`
	On       bool   `json:"on"`
}
type voiceSpeakingEvent struct {
	BoardID  string `json:"board_id"`
	Username string `json:"username"`
	Speaking bool   `json:"speaking"`
}
type voiceMuteStateEvent struct {
	BoardID  string `json:"board_id"`
	Username string `json:"username"`
	Muted    bool   `json:"muted"`
	Deafened bool   `json:"deafened"`
}
type voiceStatusSnapshotEvent struct {
	BoardID  string             `json:"board_id"`
	Statuses []VoiceStatusEntry `json:"statuses"`
}
type pfpUpdatedEvent struct {
	Username string `json:"username"`
}

func (a *App) wsReader(conn *websocket.Conn) {
	defer func() {
		a.mu.Lock(); if a.ws == conn { a.ws = nil }; a.mu.Unlock()
		runtime.EventsEmit(a.ctx, "ws:disconnected")
	}()
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil { return }
		var msg serverMsg
		if err := json.Unmarshal(raw, &msg); err != nil { continue }
		switch msg.Type {
		case "message":
			if msg.Data != nil { runtime.EventsEmit(a.ctx, "chat:message", *msg.Data) }
		case "history":
			runtime.EventsEmit(a.ctx, "chat:history", historyEvent{BoardID: msg.BoardID, Messages: msg.Messages})
		case "users":
			runtime.EventsEmit(a.ctx, "chat:users", usersEvent{Online: msg.Online, Statuses: msg.Presence, All: msg.All, Owner: msg.Owner})
		case "server_updated":
			runtime.EventsEmit(a.ctx, "server:updated")
		case "kicked":
			runtime.EventsEmit(a.ctx, "server:kicked", kickedEvent{Banned: msg.Banned, Reason: msg.Reason})
		case "typing":
			runtime.EventsEmit(a.ctx, "chat:typing", typingEvent{Username: msg.Username, Typing: msg.Typing})
		case "message_edit":
			runtime.EventsEmit(a.ctx, "chat:edit", editEvent{ID: msg.ID, BoardID: msg.BoardID, Content: msg.Content})
		case "message_delete":
			runtime.EventsEmit(a.ctx, "chat:delete", deleteEvent{ID: msg.ID, BoardID: msg.BoardID})
		case "message_reaction":
			runtime.EventsEmit(a.ctx, "chat:reaction", reactionEvent{ID: msg.ID, BoardID: msg.BoardID, Emoji: msg.Emoji, Username: msg.Username, On: msg.On})
		case "message_pin":
			runtime.EventsEmit(a.ctx, "chat:pin", pinEvent{ID: msg.ID, BoardID: msg.BoardID, Pinned: msg.Pinned, By: msg.By})
		case "rooms_updated":
			runtime.EventsEmit(a.ctx, "rooms:updated")
		case "voice_state":
			runtime.EventsEmit(a.ctx, "voice:state", voiceStateEvent{Channels: msg.Channels})
		case "voice_answer":
			a.handleVoiceAnswer(msg.SDP)
		case "voice_ice":
			a.handleVoiceICE(msg.Candidate, msg.SDPMid, msg.SDPMLineIndex)
		case "voice_renegotiate":
			a.handleVoiceRenegotiate(msg.SDP)
		case "voice_speaking":
			runtime.EventsEmit(a.ctx, "voice:peer_speaking", voiceSpeakingEvent{BoardID: msg.BoardID, Username: msg.Username, Speaking: msg.Speaking})
		case "voice_mute_state":
			runtime.EventsEmit(a.ctx, "voice:peer_mute_state", voiceMuteStateEvent{BoardID: msg.BoardID, Username: msg.Username, Muted: msg.Muted, Deafened: msg.Deafened})
		case "voice_status_snapshot":
			runtime.EventsEmit(a.ctx, "voice:status_snapshot", voiceStatusSnapshotEvent{BoardID: msg.BoardID, Statuses: msg.Statuses})
		case "pfp_request":
			a.handlePfpRequest()
		case "pfp_updated":
			runtime.EventsEmit(a.ctx, "pfp:updated", pfpUpdatedEvent{Username: msg.Username})
		case "profile_request":
			a.handleProfileRequest()
		case "profile_updated":
			runtime.EventsEmit(a.ctx, "profile:updated", pfpUpdatedEvent{Username: msg.Username})
		}
	}
}

func (a *App) GetRooms() ([]Room, error) {
	var rooms []Room
	return rooms, a.doGET("/api/rooms", &rooms)
}

func (a *App) GetBoards(roomID string) ([]Board, error) {
	var boards []Board
	return boards, a.doGET("/api/rooms/"+roomID+"/boards", &boards)
}

// SearchServer searches every board's message content on this host, not
// just one channel — "rooms" render as categories inside one connected
// server, not separate joinable spaces, so search spans all of them by
// default.
func (a *App) SearchServer(query string) ([]SearchResult, error) {
	var results []SearchResult
	path := "/api/search?q=" + url.QueryEscape(query)
	return results, a.doGET(path, &results)
}

// GetMessagesBefore fetches one page of messages older than beforeID, for
// "load older" when scrolling to the top of a board's history. Page size is
// fixed server-side (messages::HISTORY_PAGE) — see the matching constant in
// frontend/index.html.
func (a *App) GetMessagesBefore(boardID, beforeID string) ([]ChatMessage, error) {
	var msgs []ChatMessage
	path := "/api/boards/" + boardID + "/messages?before=" + url.QueryEscape(beforeID)
	return msgs, a.doGET(path, &msgs)
}

// GetMessagesAround fetches a window of messages centered on a specific
// one, for jumping straight to a message (e.g. from a search result)
// without paging backward through history to reach it.
func (a *App) GetMessagesAround(boardID, messageID string) ([]ChatMessage, error) {
	var msgs []ChatMessage
	path := "/api/boards/" + boardID + "/messages/around/" + url.PathEscape(messageID)
	return msgs, a.doGET(path, &msgs)
}

func (a *App) SubscribeBoard(boardID string) error {
	a.writeMu.Lock(); defer a.writeMu.Unlock()
	a.mu.Lock(); conn := a.ws; a.mu.Unlock()
	if conn == nil { return fmt.Errorf("not connected") }
	msg, _ := json.Marshal(map[string]string{"type": "subscribe", "board_id": boardID})
	return conn.WriteMessage(websocket.TextMessage, msg)
}

// JoinVoiceChannel and LeaveVoiceChannel now live in voice.go, alongside
// the actual WebRTC session they establish — this used to be a
// presence-only stub here.

func (a *App) SendMessage(boardID, content string, attachments []Attachment) error {
	a.writeMu.Lock(); defer a.writeMu.Unlock()
	a.mu.Lock(); conn := a.ws; a.mu.Unlock()
	if conn == nil { return fmt.Errorf("not connected") }
	if attachments == nil { attachments = []Attachment{} }
	payload, _ := json.Marshal(map[string]interface{}{
		"type":        "message",
		"board_id":    boardID,
		"content":     content,
		"attachments": attachments,
	})
	return conn.WriteMessage(websocket.TextMessage, payload)
}

func (a *App) DeleteMessage(msgID string) error {
	a.mu.Lock(); domain := a.domain; token := a.token; a.mu.Unlock()
	req, err := http.NewRequest("DELETE", normaliseHTTP(domain)+"/api/messages/"+msgID, nil)
	if err != nil { return err }
	req.Header.Set("X-Session-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil { return fmt.Errorf("network error: %w", err) }
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e map[string]string; json.NewDecoder(resp.Body).Decode(&e)
		if msg, ok := e["error"]; ok { return fmt.Errorf("%s", msg) }
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// GetPinnedMessages returns a board's pinned messages, most recently
// pinned first.
func (a *App) GetPinnedMessages(boardID string) ([]ChatMessage, error) {
	var out []ChatMessage
	err := a.doGET("/api/boards/"+boardID+"/pins", &out)
	return out, err
}

// SetMessagePinned pins (PUT) or unpins (DELETE) a message. The server
// broadcasts the change as message_pin, which is what updates every
// client's view — including this one.
// FetchServerIcon returns a server's icon (shown on the server list) as a
// data URL, or "" if it doesn't have one. Public — it works for any saved
// server, connected or not, so the list can show icons before joining.
func (a *App) FetchServerIcon(domain string) (string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(normaliseHTTP(domain) + "/api/server/icon")
	if err != nil {
		return "", fmt.Errorf("cannot reach server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 3<<20))
	if err != nil || len(data) == 0 {
		return "", err
	}
	return pfpDataURL(data), nil
}

// FetchServerThemeBackground returns the connected server's theme
// background image as a data URL, or "" if it doesn't have one.
func (a *App) FetchServerThemeBackground() (string, error) {
	data, err := a.fetchAuthed("/api/server/theme/background")
	if err != nil || len(data) == 0 {
		return "", err
	}
	return pfpDataURL(data), nil
}

// FetchServerBanner returns the connected server's banner as a data URL,
// or "" if it doesn't have one.
func (a *App) FetchServerBanner() (string, error) {
	data, err := a.fetchAuthed("/api/server/banner")
	if err != nil {
		log.Printf("server banner: fetch failed: %v", err)
		return "", err
	}
	if len(data) == 0 {
		log.Printf("server banner: none on the server (404)")
		return "", nil
	}
	log.Printf("server banner: fetched %d bytes", len(data))
	return pfpDataURL(data), nil
}

// ── Presence ────────────────────────────────────────────────────────────

func (a *App) currentPresence() string {
	if s, ok := a.presence.Load().(string); ok && s != "" {
		return s
	}
	return "online"
}

// SetPresence sets the status others see: "online", "idle" (away) or
// "invisible" (shown as offline). The frontend works out which — your
// manual choice, or automatic away after 10 minutes idle — and calls this
// whenever it changes. It's also what the next (re)connect announces.
func (a *App) SetPresence(status string) error {
	switch status {
	case "online", "idle", "invisible":
	default:
		return fmt.Errorf("unknown status %q", status)
	}
	a.presence.Store(status)
	a.sendWS(map[string]interface{}{"type": "set_status", "status": status})
	return nil
}

// SendTyping tells the server you started (true) or stopped (false)
// typing. The server doesn't pass it on while you're invisible.
func (a *App) SendTyping(typing bool) {
	a.sendWS(map[string]interface{}{"type": "typing", "typing": typing})
}

// sendWS writes one JSON message to the server if connected.
func (a *App) sendWS(v interface{}) {
	msg, err := json.Marshal(v)
	if err != nil {
		return
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if a.ws == nil {
		return
	}
	_ = a.ws.WriteMessage(websocket.TextMessage, msg)
}

// ReactToMessage adds (on=true) or removes (on=false) your emoji reaction.
// The server broadcasts the change as message_reaction, which is what
// updates every client's view — including this one.
func (a *App) ReactToMessage(msgID, emoji string, on bool) error {
	a.mu.Lock(); domain := a.domain; token := a.token; a.mu.Unlock()
	data, _ := json.Marshal(map[string]interface{}{"emoji": emoji, "on": on})
	req, err := http.NewRequest("POST", normaliseHTTP(domain)+"/api/messages/"+msgID+"/reactions", bytes.NewReader(data))
	if err != nil { return err }
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil { return fmt.Errorf("network error: %w", err) }
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e map[string]string; json.NewDecoder(resp.Body).Decode(&e)
		if msg, ok := e["error"]; ok { return fmt.Errorf("%s", msg) }
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func (a *App) SetMessagePinned(msgID string, pinned bool) error {
	a.mu.Lock(); domain := a.domain; token := a.token; a.mu.Unlock()
	method := "PUT"
	if !pinned {
		method = "DELETE"
	}
	req, err := http.NewRequest(method, normaliseHTTP(domain)+"/api/messages/"+msgID+"/pin", nil)
	if err != nil { return err }
	req.Header.Set("X-Session-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil { return fmt.Errorf("network error: %w", err) }
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e map[string]string; json.NewDecoder(resp.Body).Decode(&e)
		if msg, ok := e["error"]; ok { return fmt.Errorf("%s", msg) }
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func (a *App) EditMessage(msgID, content string) error {
	a.mu.Lock(); domain := a.domain; token := a.token; a.mu.Unlock()
	data, _ := json.Marshal(map[string]string{"content": content})
	req, err := http.NewRequest("PATCH", normaliseHTTP(domain)+"/api/messages/"+msgID, bytes.NewReader(data))
	if err != nil { return err }
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil { return fmt.Errorf("network error: %w", err) }
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e map[string]string; json.NewDecoder(resp.Body).Decode(&e)
		if msg, ok := e["error"]; ok { return fmt.Errorf("%s", msg) }
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// DropFiles is called by the frontend when files are dropped via drag & drop.
// Since Wails v2's OnFileDrop requires a newer version, the frontend uses
// the browser drag API which gives us File objects without native paths.
// Instead of paths, the frontend calls this with the filenames and we open
// a file picker pre-filtered — but actually we just expose PickFiles which
// the user can use. For true drag & drop we expose this no-op for forward compat.
func (a *App) DropFiles(paths []string) error {
	return nil
}

// PickFiles opens a native multi-file dialog.
func (a *App) PickFiles() ([]string, error) {
	paths, err := runtime.OpenMultipleFilesDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Attach files",
		Filters: []runtime.FileFilter{
			{DisplayName: "All Files",   Pattern: "*"},
			{DisplayName: "Images",      Pattern: "*.png;*.jpg;*.jpeg;*.gif;*.webp"},
			{DisplayName: "Video",       Pattern: "*.mp4;*.webm;*.mov;*.mkv;*.avi"},
			{DisplayName: "Audio",       Pattern: "*.mp3;*.ogg;*.wav;*.flac;*.m4a"},
			{DisplayName: "Documents",   Pattern: "*.pdf;*.doc;*.docx;*.txt;*.zip"},
		},
	})
	if err != nil { return nil, err }
	return paths, nil
}

// UploadFile streams a file to the server without loading it all into memory.
func (a *App) UploadFile(filePath string) (*UploadResult, error) {
	a.mu.Lock(); domain := a.domain; token := a.token; a.mu.Unlock()
	if domain == "" { return nil, fmt.Errorf("not connected") }

	f, err := os.Open(filePath)
	if err != nil { return nil, fmt.Errorf("could not open file: %w", err) }
	defer f.Close()

	filename := filepath.Base(filePath)
	pr, pw   := io.Pipe()
	mw       := multipart.NewWriter(pw)

	go func() {
		fw, err := mw.CreateFormFile("file", filename)
		if err != nil { pw.CloseWithError(err); return }
		if _, err := io.Copy(fw, f); err != nil { pw.CloseWithError(err); return }
		mw.Close(); pw.Close()
	}()

	req, err := http.NewRequest("POST", normaliseHTTP(domain)+"/api/upload", pr)
	if err != nil { pr.CloseWithError(err); return nil, err }
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Session-Token", token)

	client := &http.Client{Timeout: 10 * 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil { return nil, fmt.Errorf("upload failed: %w", err) }
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var e map[string]string; json.NewDecoder(resp.Body).Decode(&e)
		if msg, ok := e["error"]; ok { return nil, fmt.Errorf("%s", msg) }
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var result UploadResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil { return nil, err }
	return &result, nil
}

// GetFileLink asks the server for a short-lived link to one attachment
// (path is its "/api/files/<name>" URL from the message) and returns it as
// an absolute URL. The server only serves /api/files to a valid, unexpired
// link token — or a session token, which a plain <img>/<video> tag or the
// user's browser can't send — so every inline display and every browser
// download goes through one of these. Links last 5 minutes (server-side
// LINK_TTL), checked when each request starts.
func (a *App) GetFileLink(path string) (string, error) {
	var resp struct {
		URL string `json:"url"`
	}
	if err := a.doGET(path+"/link", &resp); err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return normaliseHTTP(a.domain) + resp.URL, nil
}

// GetFileURL returns the full URL for a server-relative path.
func (a *App) GetFileURL(path string) string {
	a.mu.Lock(); defer a.mu.Unlock()
	return normaliseHTTP(a.domain) + path
}

// FetchLinkPreview fetches OG data for a URL via the server proxy.
func (a *App) FetchLinkPreview(url string) (*LinkPreview, error) {
	a.mu.Lock(); domain := a.domain; token := a.token; a.mu.Unlock()
	if domain == "" { return nil, fmt.Errorf("not connected") }

	req, err := http.NewRequest("GET",
		normaliseHTTP(domain)+"/api/preview?url="+url, nil)
	if err != nil { return nil, err }
	req.Header.Set("X-Session-Token", token)

	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil { return nil, err }
	defer resp.Body.Close()
	if resp.StatusCode >= 400 { return nil, fmt.Errorf("preview unavailable") }

	var p LinkPreview
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil { return nil, err }
	return &p, nil
}

func (a *App) GetUsername() string { return a.username }

// GetFingerprint returns the current session's identity fingerprint
// (first 16 hex chars of the public key), as returned by /api/join.
func (a *App) GetFingerprint() string { return a.fingerprint }

func (a *App) Disconnect() {
	a.mu.Lock(); defer a.mu.Unlock()
	if a.ws != nil { a.ws.Close(); a.ws = nil }
	a.token = ""; a.domain = ""; a.username = ""; a.fingerprint = ""
}

// ResolveServerAddress turns what someone typed into a full server address.
// With http:// or https:// already there it's used as-is (tidied up).
// Without one, https:// is tried first and http:// second, by asking each
// for /api/info; whichever answers is used. So "myserver.com" finds an
// HTTPS server behind a proxy, and "192.168.1.10:7070" still finds a plain
// HTTP one on the LAN.
func (a *App) ResolveServerAddress(input string) (string, error) {
	addr := strings.TrimSpace(input)
	addr = strings.TrimRight(addr, "/")
	if addr == "" {
		return "", fmt.Errorf("enter the server's address")
	}
	lower := strings.ToLower(addr)
	if strings.HasPrefix(lower, "https://") {
		return "https://" + addr[len("https://"):], nil
	}
	if strings.HasPrefix(lower, "http://") {
		return "http://" + addr[len("http://"):], nil
	}
	client := &http.Client{Timeout: 4 * time.Second}
	var lastErr error
	for _, scheme := range []string{"https://", "http://"} {
		resp, err := client.Get(scheme + addr + "/api/info")
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return scheme + addr, nil
		}
		lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return "", fmt.Errorf("couldn't reach a chriscord server at %s over https or http (%v)", addr, lastErr)
}

// UpdateServer edits a saved server in place (same spot in the list):
// its address, join key and your own name for it. oldDomain identifies
// which entry; the new address may differ. An empty custom name goes back
// to showing the server's own name.
func (a *App) UpdateServer(oldDomain, newDomain, serverKey, customName string) ([]SavedServer, error) {
	newDomain = strings.TrimRight(strings.TrimSpace(newDomain), "/")
	if newDomain == "" {
		return a.servers, fmt.Errorf("the address can't be empty")
	}
	idx := -1
	for i, s := range a.servers {
		if s.Domain == oldDomain {
			idx = i
		} else if s.Domain == newDomain {
			return a.servers, fmt.Errorf("%s is already in your server list", newDomain)
		}
	}
	if idx < 0 {
		return a.servers, fmt.Errorf("that server isn't in your list any more")
	}
	s := a.servers[idx]
	s.Domain, s.ServerKey, s.CustomName = newDomain, strings.TrimSpace(serverKey), strings.TrimSpace(customName)
	a.servers[idx] = s
	saveServers(a.servers)
	return a.servers, nil
}

// ReorderServers saves a new order for the server list (from dragging).
// Any saved server missing from domains keeps its place at the end, so a
// stale list from the frontend can never drop one.
func (a *App) ReorderServers(domains []string) []SavedServer {
	byDomain := make(map[string]SavedServer, len(a.servers))
	for _, s := range a.servers {
		byDomain[s.Domain] = s
	}
	out := make([]SavedServer, 0, len(a.servers))
	for _, d := range domains {
		if s, ok := byDomain[d]; ok {
			out = append(out, s)
			delete(byDomain, d)
		}
	}
	for _, s := range a.servers {
		if _, left := byDomain[s.Domain]; left {
			out = append(out, s)
		}
	}
	a.servers = out
	saveServers(a.servers)
	return a.servers
}

func (a *App) RemoveServer(domain string) []SavedServer {
	a.servers = removeServer(a.servers, domain)
	saveServers(a.servers)
	return a.servers
}

package main

// downloads.go — the downloads manager: files are saved by the app itself
// (no browser), with their progress in the downloads button at the top
// right. Each download goes to your downloads folder, or, with "Ask where
// to save each file" on (Settings → System), where you choose.
//
// Encrypted attachments are decrypted straight into the file; server files
// are fetched with a fresh 5-minute link. Each is written to a ".part" file
// next to where it's going and renamed when it's complete, so a cut-off
// download never looks like a finished file. Up to three run at once; the
// rest wait their turn.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	dlParallel   = 3
	dlKeep       = 100 // finished downloads listed (older ones drop off)
	dlEmitEvery  = 200 * time.Millisecond
	dlPartSuffix = ".part"
)

// Download states.
const (
	dlQueued      = "queued"      // waiting for one of the three slots
	dlPreparing   = "preparing"   // getting the link (the server may be zipping)
	dlDownloading = "downloading" //
	dlDone        = "done"
	dlFailed      = "failed"
	dlCancelled   = "cancelled"
)

// DownloadSource is what to download, as the page asks for it.
type DownloadSource struct {
	Kind      string `json:"kind"`                 // "file" or "zip" (all of a message's files)
	URL       string `json:"url,omitempty"`        // the attachment's address ("/api/files/…" or "/e2ee-file/…")
	Name      string `json:"name"`                 // the file name it's saved as
	MessageID string `json:"message_id,omitempty"` // for "zip"
}

// Download is one download, as the downloads list shows it.
type Download struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	State    string `json:"state"`
	Done     int64  `json:"done"`
	Total    int64  `json:"total"` // 0: not known
	Speed    int64  `json:"speed"` // bytes a second, while downloading
	Error    string `json:"error,omitempty"`
	Started  int64  `json:"started"` // unix ms
	Finished int64  `json:"finished,omitempty"`
	Risky    bool   `json:"risky,omitempty"` // a program or script: shown in its folder, never opened
}

type dlItem struct {
	Download
	src      DownloadSource
	server   string // the server it came from (for "where it came from" on Windows)
	cancel   context.CancelFunc
	lastEmit time.Time
	// speed: bytes since the last measurement
	speedAt   time.Time
	speedDone int64
}

type downloadManager struct {
	a      *App
	mu     sync.Mutex
	items  []*dlItem // oldest first
	next   int
	active int
	// paths being written to now, so two downloads of "photo.png" at
	// once don't pick the same name
	claimed map[string]bool
}

func (a *App) downloads() *downloadManager {
	a.dlOnce.Do(func() { a.dlm = &downloadManager{a: a, claimed: map[string]bool{}} })
	return a.dlm
}

// ── Settings (downloads.json in the config folder: this computer's) ──────────

type downloadPrefs struct {
	Ask    bool   `json:"ask"`              // ask where to save each file
	Folder string `json:"folder,omitempty"` // "" = the system's downloads folder
}

var downloadPrefsMu sync.Mutex

func downloadPrefsPath() string { return filepath.Join(chriscordConfigDir(), "downloads.json") }

func loadDownloadPrefs() downloadPrefs {
	downloadPrefsMu.Lock()
	defer downloadPrefsMu.Unlock()
	var p downloadPrefs
	if data, err := os.ReadFile(downloadPrefsPath()); err == nil {
		json.Unmarshal(data, &p)
	}
	return p
}

func updateDownloadPrefs(change func(*downloadPrefs)) error {
	downloadPrefsMu.Lock()
	defer downloadPrefsMu.Unlock()
	var p downloadPrefs
	if data, err := os.ReadFile(downloadPrefsPath()); err == nil {
		json.Unmarshal(data, &p)
	}
	change(&p)
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(downloadPrefsPath(), data, 0644)
}

// DownloadSettings is Settings → System → Downloads.
type DownloadSettings struct {
	Ask       bool   `json:"ask"`
	Folder    string `json:"folder"`     // where downloads go
	IsDefault bool   `json:"is_default"` // the system's downloads folder
}

func downloadFolder(p downloadPrefs) string {
	if p.Folder != "" {
		if st, err := os.Stat(p.Folder); err == nil && st.IsDir() {
			return p.Folder
		}
	}
	return systemDownloadsDir()
}

func (a *App) GetDownloadSettings() DownloadSettings {
	p := loadDownloadPrefs()
	f := downloadFolder(p)
	return DownloadSettings{Ask: p.Ask, Folder: f, IsDefault: p.Folder == "" || f != p.Folder}
}

func (a *App) SetDownloadAsk(ask bool) error {
	return updateDownloadPrefs(func(p *downloadPrefs) { p.Ask = ask })
}

// ChooseDownloadFolder asks for a folder downloads go to.
func (a *App) ChooseDownloadFolder() (DownloadSettings, error) {
	dir, err := runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Where downloads go", DefaultDirectory: downloadFolder(loadDownloadPrefs()), CanCreateDirectories: true,
	})
	if err != nil || dir == "" {
		return a.GetDownloadSettings(), err
	}
	if err := updateDownloadPrefs(func(p *downloadPrefs) { p.Folder = dir }); err != nil {
		return a.GetDownloadSettings(), err
	}
	return a.GetDownloadSettings(), nil
}

// ResetDownloadFolder goes back to the system's downloads folder.
func (a *App) ResetDownloadFolder() (DownloadSettings, error) {
	err := updateDownloadPrefs(func(p *downloadPrefs) { p.Folder = "" })
	return a.GetDownloadSettings(), err
}

// ── Starting downloads ───────────────────────────────────────────────────────

// StartDownload starts downloading src. With "ask where to save" on it
// asks first; cancelling that returns nil (nothing started).
func (a *App) StartDownload(src DownloadSource) (*Download, error) {
	switch src.Kind {
	case "file":
		if !strings.HasPrefix(src.URL, fileLocalPrefix) && !strings.HasPrefix(src.URL, "/api/files/") {
			return nil, errors.New("that isn't a file on this server")
		}
	case "zip":
		if src.MessageID == "" {
			return nil, errors.New("no message to zip")
		}
		if src.Name == "" {
			src.Name = "attachments.zip"
		}
	default:
		return nil, fmt.Errorf("unknown download %q", src.Kind)
	}
	name := safeFileName(src.Name)
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "file"
	}
	src.Name = name

	p := loadDownloadPrefs()
	m := a.downloads()
	var dest string
	if p.Ask {
		ext := filepath.Ext(name)
		filters := []runtime.FileFilter(nil)
		if ext != "" {
			filters = []runtime.FileFilter{{DisplayName: strings.ToUpper(ext[1:]) + " file", Pattern: "*" + ext}, {DisplayName: "All files", Pattern: "*.*"}}
		}
		d, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
			Title: "Save " + name, DefaultFilename: name, DefaultDirectory: downloadFolder(p), Filters: filters,
		})
		if err != nil {
			return nil, err
		}
		if d == "" {
			return nil, nil // cancelled
		}
		dest = d
	}
	a.mu.Lock()
	server := a.domain
	a.mu.Unlock()

	m.mu.Lock()
	if dest == "" {
		dir := downloadFolder(p)
		if err := os.MkdirAll(dir, 0755); err != nil {
			m.mu.Unlock()
			return nil, fmt.Errorf("can't use your downloads folder: %w", err)
		}
		dest = m.freeNameLocked(dir, name)
	}
	m.claimed[dest] = true
	m.next++
	it := &dlItem{Download: Download{ID: fmt.Sprintf("d%d", m.next), Name: filepath.Base(dest), Path: dest,
		State: dlQueued, Started: time.Now().UnixMilli(), Risky: riskyFile(dest)}, src: src, server: server}
	m.items = append(m.items, it)
	m.trimLocked()
	m.pumpLocked()
	d := it.Download
	m.mu.Unlock()
	m.emitList()
	return &d, nil
}

// freeNameLocked: dir/name, or "name (2).ext"… if that's taken (on disk,
// half downloaded, or being downloaded now).
func (m *downloadManager) freeNameLocked(dir, name string) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		n := name
		if i > 1 {
			n = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		p := filepath.Join(dir, n)
		if m.claimed[p] {
			continue
		}
		if _, err := os.Lstat(p); err == nil {
			continue
		}
		if _, err := os.Lstat(p + dlPartSuffix); err == nil {
			continue
		}
		return p
	}
}

// trimLocked drops the oldest finished downloads past dlKeep.
func (m *downloadManager) trimLocked() {
	for len(m.items) > dlKeep {
		i := -1
		for j, it := range m.items {
			if finished(it.State) {
				i = j
				break
			}
		}
		if i < 0 {
			return
		}
		m.items = append(m.items[:i], m.items[i+1:]...)
	}
}

func finished(state string) bool {
	return state == dlDone || state == dlFailed || state == dlCancelled
}

// pumpLocked starts queued downloads while there's a free slot.
func (m *downloadManager) pumpLocked() {
	for _, it := range m.items {
		if m.active >= dlParallel {
			return
		}
		if it.State != dlQueued {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		it.cancel = cancel
		it.State = dlPreparing
		m.active++
		go m.run(ctx, it)
	}
}

func (m *downloadManager) run(ctx context.Context, it *dlItem) {
	err := m.a.fetchDownload(ctx, it, m)
	m.mu.Lock()
	m.active--
	delete(m.claimed, it.Path)
	it.cancel = nil
	it.Speed = 0
	it.Finished = time.Now().UnixMilli()
	switch {
	case err == nil:
		it.State = dlDone
		if it.Total <= 0 {
			it.Total = it.Done
		}
	case ctx.Err() != nil:
		it.State, it.Error = dlCancelled, ""
	default:
		it.State, it.Error = dlFailed, err.Error()
		log.Printf("downloads: %s: %v", it.Name, err)
	}
	m.pumpLocked()
	m.mu.Unlock()
	m.emit(it, true)
}

// progress: n more bytes of it are written.
func (m *downloadManager) progress(it *dlItem, n int64) {
	m.mu.Lock()
	it.Done += n
	now := time.Now()
	if it.speedAt.IsZero() {
		it.speedAt, it.speedDone = now, it.Done
	} else if el := now.Sub(it.speedAt); el >= 500*time.Millisecond {
		inst := int64(float64(it.Done-it.speedDone) / el.Seconds())
		if it.Speed == 0 {
			it.Speed = inst
		} else {
			it.Speed = (it.Speed*2 + inst) / 3 // smoothed
		}
		it.speedAt, it.speedDone = now, it.Done
	}
	due := now.Sub(it.lastEmit) >= dlEmitEvery
	m.mu.Unlock()
	if due {
		m.emit(it, false)
	}
}

func (m *downloadManager) setState(it *dlItem, state string, total int64) {
	m.mu.Lock()
	it.State = state
	if total > 0 {
		it.Total = total
	}
	m.mu.Unlock()
	m.emit(it, true)
}

// emit sends one download's state to the page.
func (m *downloadManager) emit(it *dlItem, force bool) {
	m.mu.Lock()
	if !force && time.Since(it.lastEmit) < dlEmitEvery {
		m.mu.Unlock()
		return
	}
	it.lastEmit = time.Now()
	d := it.Download
	m.mu.Unlock()
	if m.a.ctx != nil {
		runtime.EventsEmit(m.a.ctx, "downloads:item", d)
	}
}

func (m *downloadManager) emitList() {
	if m.a.ctx != nil {
		runtime.EventsEmit(m.a.ctx, "downloads:list", m.list())
	}
}

func (m *downloadManager) list() []Download {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Download, 0, len(m.items))
	for i := len(m.items) - 1; i >= 0; i-- { // newest first
		out = append(out, m.items[i].Download)
	}
	return out
}

// ── Fetching ─────────────────────────────────────────────────────────────────

// countWriter counts what's written through it, and stops a cancelled
// download at its next write.
type countWriter struct {
	ctx context.Context
	w   io.Writer
	m   *downloadManager
	it  *dlItem
}

func (c *countWriter) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.w.Write(p)
	if n > 0 {
		c.m.progress(c.it, int64(n))
	}
	return n, err
}

// fetchDownload downloads it into it.Path (via a .part file).
func (a *App) fetchDownload(ctx context.Context, it *dlItem, m *downloadManager) error {
	src := it.src
	var (
		write func(w io.Writer) error
		total int64
	)
	switch {
	case src.Kind == "file" && strings.HasPrefix(src.URL, fileLocalPrefix):
		e := a.text.fileFor(src.URL)
		if e == nil {
			return errors.New("that file isn't available any more — open its channel and try again")
		}
		total = e.meta.Size
		write = func(w io.Writer) error { return a.decryptToCtx(ctx, w, e) }
	case src.Kind == "zip" && a.messageHasEncryptedFiles(src.MessageID):
		a.text.mu.Lock()
		files := append([]fileMeta(nil), a.text.msgFiles[src.MessageID]...)
		a.text.mu.Unlock()
		var entries []*fileEntry
		for _, f := range files {
			if e := a.text.fileFor(fileLocalPrefix + path.Base(f.URL)); e != nil {
				entries = append(entries, e)
				total += e.meta.Size
			}
		}
		if len(entries) == 0 {
			return errors.New("those files aren't available any more — open the channel and try again")
		}
		write = func(w io.Writer) error { return a.writeE2EEZip(ctx, w, entries) }
	default:
		var link string
		if src.Kind == "zip" {
			z, err := a.GetMessageZipLink(src.MessageID) // the server zips them (can take a moment)
			if err != nil {
				if strings.Contains(err.Error(), "HTTP 404") {
					return errors.New("this server needs an update before it can zip files")
				}
				return err
			}
			link, total = z.URL, z.Size
		} else {
			l, err := a.GetFileLink(src.URL)
			if err != nil {
				return err
			}
			sep := "?"
			if strings.Contains(l, "?") {
				sep = "&"
			}
			link = l + sep + "download=1&name=" + url.QueryEscape(src.Name)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		req, err := http.NewRequestWithContext(ctx, "GET", link, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("network error: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("the server answered HTTP %d", resp.StatusCode)
		}
		if resp.ContentLength > 0 {
			total = resp.ContentLength
		}
		write = func(w io.Writer) error {
			n, err := io.Copy(w, resp.Body)
			if err == nil && resp.ContentLength > 0 && n != resp.ContentLength {
				return fmt.Errorf("cut off after %d of %d bytes", n, resp.ContentLength)
			}
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	part := it.Path + dlPartSuffix
	f, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("can't save there: %w", err)
	}
	m.setState(it, dlDownloading, total)
	werr := write(&countWriter{ctx: ctx, w: f, m: m, it: it})
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil && ctx.Err() != nil {
		werr = ctx.Err()
	}
	if werr != nil {
		os.Remove(part)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return werr
	}
	// The save dialog already asked about replacing a file of that name.
	os.Remove(it.Path)
	if err := os.Rename(part, it.Path); err != nil {
		os.Remove(part)
		return err
	}
	markDownloaded(it.Path, it.server)
	return nil
}

func (a *App) messageHasEncryptedFiles(msgID string) bool {
	if a.text == nil {
		return false
	}
	a.text.mu.Lock()
	defer a.text.mu.Unlock()
	return len(a.text.msgFiles[msgID]) > 0
}

// ── The list's buttons ───────────────────────────────────────────────────────

func (a *App) GetDownloads() []Download { return a.downloads().list() }

func (m *downloadManager) find(id string) *dlItem {
	for _, it := range m.items {
		if it.ID == id {
			return it
		}
	}
	return nil
}

func (a *App) CancelDownload(id string) {
	m := a.downloads()
	m.mu.Lock()
	it := m.find(id)
	if it == nil {
		m.mu.Unlock()
		return
	}
	if it.State == dlQueued {
		it.State = dlCancelled
		it.Finished = time.Now().UnixMilli()
		delete(m.claimed, it.Path)
		m.mu.Unlock()
		m.emit(it, true)
		return
	}
	cancel := it.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// RetryDownload starts a failed or cancelled download again (a fresh link,
// the same file name if it's still free).
func (a *App) RetryDownload(id string) error {
	m := a.downloads()
	m.mu.Lock()
	it := m.find(id)
	if it == nil || !finished(it.State) || it.State == dlDone {
		m.mu.Unlock()
		return nil
	}
	if m.claimed[it.Path] {
		it.Path = m.freeNameLocked(filepath.Dir(it.Path), filepath.Base(it.Path))
		it.Name = filepath.Base(it.Path)
	}
	m.claimed[it.Path] = true
	it.State, it.Error, it.Done, it.Speed, it.Finished = dlQueued, "", 0, 0, 0
	it.speedAt, it.speedDone = time.Time{}, 0
	it.Started = time.Now().UnixMilli()
	m.pumpLocked()
	m.mu.Unlock()
	m.emit(it, true)
	return nil
}

// RemoveDownload takes a download off the list (stopping it if it's
// running). The file stays where it was saved.
func (a *App) RemoveDownload(id string) {
	a.CancelDownload(id)
	m := a.downloads()
	m.mu.Lock()
	for i, it := range m.items {
		if it.ID == id {
			m.items = append(m.items[:i], m.items[i+1:]...)
			break
		}
	}
	m.mu.Unlock()
	m.emitList()
}

// ClearDownloads takes every finished download off the list.
func (a *App) ClearDownloads() {
	m := a.downloads()
	m.mu.Lock()
	kept := m.items[:0]
	for _, it := range m.items {
		if !finished(it.State) {
			kept = append(kept, it)
		}
	}
	m.items = kept
	m.mu.Unlock()
	m.emitList()
}

// doneDownload: the finished download's file, if it's still there.
func (a *App) doneDownload(id string) (*dlItem, error) {
	m := a.downloads()
	m.mu.Lock()
	it := m.find(id)
	var p string
	if it != nil && it.State == dlDone {
		p = it.Path
	}
	m.mu.Unlock()
	if p == "" {
		return nil, errors.New("that download isn't finished")
	}
	if _, err := os.Stat(p); err != nil {
		return nil, errors.New("the file has been moved or deleted")
	}
	return it, nil
}

// OpenDownload opens a finished download with its usual app. Programs and
// scripts aren't opened from here: they're shown in their folder instead.
func (a *App) OpenDownload(id string) error {
	it, err := a.doneDownload(id)
	if err != nil {
		return err
	}
	if it.Risky {
		return showInFolder(it.Path)
	}
	return openWithDefaultApp(it.Path)
}

// ShowDownload shows a finished download in its folder.
func (a *App) ShowDownload(id string) error {
	it, err := a.doneDownload(id)
	if err != nil {
		return err
	}
	return showInFolder(it.Path)
}

// OpenDownloadsFolder opens the folder downloads go to.
func (a *App) OpenDownloadsFolder() error {
	dir := downloadFolder(loadDownloadPrefs())
	os.MkdirAll(dir, 0755)
	return openWithDefaultApp(dir)
}

// riskyFile: a program, script or shortcut — opened by mistake, it runs.
func riskyFile(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".exe", ".msi", ".msix", ".msp", ".bat", ".cmd", ".com", ".scr", ".pif", ".cpl", ".ps1", ".psm1", ".vbs", ".vbe",
		".js", ".jse", ".wsf", ".wsh", ".hta", ".lnk", ".url", ".reg", ".jar", ".dll", ".sys", ".inf", ".application",
		".appx", ".appxbundle", ".gadget", ".msc", ".scf", ".sh", ".command", ".app", ".pkg", ".dmg", ".deb", ".rpm",
		".appimage", ".run", ".iso", ".img", ".vhd", ".vhdx", ".docm", ".xlsm", ".pptm":
		return true
	}
	return false
}

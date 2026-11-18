package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// OpenAttachmentExternally downloads an attachment to a temporary folder
// and opens it with the computer's default app for that kind of file —
// e.g. a video in a format the app can't play inline (MKV with HEVC video
// or AC-3/DTS sound…) opens in VLC or the system player. Everything stays
// on this computer; the server never converts anything.
//
// While it downloads it emits "file:open-progress" {path, done, total}.
//
// The copy is decrypted, so it's kept where only you can read it (your own
// cache folder, not the shared temp folder, and readable only by you) and
// doesn't outlive its use: copies older than openedKeep are swept while the
// app runs, and all of them when it starts and when it closes. (A copy the
// other app still holds open may survive until the next start — Windows
// won't delete a file that's in use.)
func (a *App) OpenAttachmentExternally(path, name string) error {
	link, err := a.GetFileLink(path)
	if err != nil {
		return err
	}
	dir := openedDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	// Just a file name — nothing that could point outside the folder.
	clean := strings.TrimLeft(filepath.Base(strings.ReplaceAll(name, "\\", "/")), ".")
	if clean == "" || clean == "/" {
		clean = "attachment"
	}
	dest := filepath.Join(dir, clean)

	resp, err := http.Get(link) // no timeout: big videos can take a while
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(dest+".part", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	total := resp.ContentLength
	var done int64
	last := time.Time{}
	buf := make([]byte, 256<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				os.Remove(dest + ".part")
				return werr
			}
			done += int64(n)
			if time.Since(last) > 200*time.Millisecond {
				last = time.Now()
				runtime.EventsEmit(a.ctx, "file:open-progress", map[string]interface{}{"path": path, "done": done, "total": total})
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(dest + ".part")
			return fmt.Errorf("download failed: %w", rerr)
		}
	}
	f.Close()
	if err := os.Rename(dest+".part", dest); err != nil {
		return err
	}
	runtime.EventsEmit(a.ctx, "file:open-progress", map[string]interface{}{"path": path, "done": done, "total": done})
	return openWithDefaultApp(dest)
}

const (
	openedKeep  = 3 * time.Hour    // a decrypted copy older than this is deleted while the app runs
	openedSweep = 30 * time.Minute // how often that's checked
)

// openedDir is where decrypted copies for other apps go: inside your own
// cache folder (on Linux not /tmp, which every user shares).
func openedDir() string {
	if base, err := os.UserCacheDir(); err == nil {
		return filepath.Join(base, "chriscord", "open")
	}
	return filepath.Join(chriscordConfigDir(), "open")
}

// sweepOpened deletes decrypted copies older than age (0: all of them).
// Files another app has locked (Windows) are left for next time.
func sweepOpened(age time.Duration) {
	dir := openedDir()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	n := 0
	for _, e := range ents {
		info, err := e.Info()
		if err != nil || (age > 0 && time.Since(info.ModTime()) < age) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if e.IsDir() {
			err = os.RemoveAll(p)
		} else {
			err = os.Remove(p)
		}
		if err == nil {
			n++
		}
	}
	if n > 0 {
		log.Printf("open: deleted %d decrypted cop%s opened in other apps", n, map[bool]string{true: "y", false: "ies"}[n == 1])
	}
}

// startOpenedSweeper clears what earlier runs left — including the old
// shared location, os.TempDir()/chriscord-open — and keeps sweeping.
func startOpenedSweeper() {
	go func() {
		os.RemoveAll(filepath.Join(os.TempDir(), "chriscord-open"))
		sweepOpened(0)
		t := time.NewTicker(openedSweep)
		defer t.Stop()
		for range t.C {
			sweepOpened(openedKeep)
		}
	}()
}

func openWithDefaultApp(file string) error {
	var cmd *exec.Cmd
	switch goruntime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", file)
	case "darwin":
		cmd = exec.Command("open", file)
	default:
		cmd = exec.Command("xdg-open", file)
	}
	cmd.Env = childEnv() // Linux: without Chriscord's own GTK theme (webview_linux.go)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("couldn't open it: %w", err)
	}
	go cmd.Wait() // don't leave a zombie behind
	return nil
}

// ── Opening links, saving files ──────────────────────────────────────────────
//
// Wails' BrowserOpenURL refuses any URL containing characters such as
// ( ) ! ~ * [ ] or a space — which plenty of real links have (Wikipedia
// pages, YouTube timestamps, file names like "Screenshot (3).png"). These
// open links without going through it.

// OpenURL opens an http(s) link in the default browser. The URL is passed
// to the system's opener as a single argument — no shell is involved — so
// unusual characters are harmless.
func (a *App) OpenURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("only web links can be opened")
	}
	return openWithDefaultApp(u.String())
}


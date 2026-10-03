package main

import (
	"fmt"
	"io"
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
func (a *App) OpenAttachmentExternally(path, name string) error {
	link, err := a.GetFileLink(path)
	if err != nil {
		return err
	}
	dir := filepath.Join(os.TempDir(), "chriscord-open")
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
	f, err := os.Create(dest + ".part")
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
// open links and save files without going through it.

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

// SaveAttachment asks where to save an attachment (starting in Downloads,
// named as it was sent) and downloads it there, emitting
// "file:save-progress" {path, done, total}. Returns where it was saved, or
// "" if the dialog was cancelled.
func (a *App) SaveAttachment(path, name string) (string, error) {
	clean := strings.TrimLeft(filepath.Base(strings.ReplaceAll(name, "\\", "/")), ".")
	if clean == "" || clean == "/" {
		clean = "attachment"
	}
	startDir := ""
	if home, err := os.UserHomeDir(); err == nil {
		if st, err := os.Stat(filepath.Join(home, "Downloads")); err == nil && st.IsDir() {
			startDir = filepath.Join(home, "Downloads")
		}
	}
	dest, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title: "Save attachment", DefaultDirectory: startDir, DefaultFilename: clean,
	})
	if err != nil || dest == "" {
		return "", err
	}
	link, err := a.GetFileLink(path)
	if err != nil {
		return "", err
	}
	resp, err := http.Get(link)
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dest + ".part")
	if err != nil {
		return "", err
	}
	total, done := resp.ContentLength, int64(0)
	last := time.Time{}
	buf := make([]byte, 256<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				os.Remove(dest + ".part")
				return "", werr
			}
			done += int64(n)
			if time.Since(last) > 200*time.Millisecond {
				last = time.Now()
				runtime.EventsEmit(a.ctx, "file:save-progress", map[string]interface{}{"path": path, "done": done, "total": total})
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(dest + ".part")
			return "", fmt.Errorf("download failed: %w", rerr)
		}
	}
	f.Close()
	os.Remove(dest) // replacing an existing file was confirmed in the dialog
	if err := os.Rename(dest+".part", dest); err != nil {
		return "", err
	}
	return dest, nil
}

// ShowInFolder opens the file manager with a saved file selected.
func (a *App) ShowInFolder(file string) error {
	var cmd *exec.Cmd
	switch goruntime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", "/select,", file)
	case "darwin":
		cmd = exec.Command("open", "-R", file)
	default:
		cmd = exec.Command("xdg-open", filepath.Dir(file))
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

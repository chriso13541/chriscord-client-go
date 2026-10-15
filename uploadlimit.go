package main

// uploadlimit.go — saying a file's too big before uploading any of it.
//
// The server's per-file limit (set in its admin panel) comes from /api/info.
// What's checked is the size as uploaded — encrypted, so a little larger
// than the file (filecrypt.go) — against it. Servers from before
// max_upload_mb was in /api/info aren't checked here; they still refuse a
// file that's too big part way through, as before.

import (
	"fmt"
	"os"
	"sync"
	"time"
)

var uploadLimitCache struct {
	mu     sync.Mutex
	domain string
	at     time.Time
	limit  int64 // bytes; 0 = no limit
	known  bool
}

// uploadLimit: the connected server's largest file in bytes (0 for none),
// and whether it said.
func (a *App) uploadLimit() (int64, bool) {
	a.mu.Lock()
	domain := a.domain
	a.mu.Unlock()
	if domain == "" {
		return 0, false
	}
	c := &uploadLimitCache
	c.mu.Lock()
	if c.domain == domain && time.Since(c.at) < time.Minute {
		limit, known := c.limit, c.known
		c.mu.Unlock()
		return limit, known
	}
	c.mu.Unlock()
	info, err := a.GetServerInfo(domain)
	if err != nil {
		return 0, false
	}
	var limit int64
	known := info.MaxUploadMB != nil
	if known {
		limit = int64(*info.MaxUploadMB) * 1024 * 1024
	}
	c.mu.Lock()
	c.domain, c.at, c.limit, c.known = domain, time.Now(), limit, known
	c.mu.Unlock()
	return limit, known
}

func fmtFileSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
}

// checkUploadSize: an error saying so if a file of size bytes is more
// than the server takes.
func (a *App) checkUploadSize(name string, size int64) error {
	limit, known := a.uploadLimit()
	if !known || limit == 0 || fileCipherSize(size) <= limit {
		return nil
	}
	return fmt.Errorf("%s is %s — this server takes files up to %s", name, fmtFileSize(size), fmtFileSize(limit))
}

// CheckUploadFiles checks files before they're uploaded: for each path, ""
// if it's fine, or why it can't be sent.
func (a *App) CheckUploadFiles(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			continue // the upload itself will say what's wrong
		}
		if err := a.checkUploadSize(st.Name(), st.Size()); err != nil {
			out[i] = err.Error()
		}
	}
	return out
}

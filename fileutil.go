package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// writeFileAtomic replaces path with data all at once: written to a
// temporary file next to it, flushed to disk, then renamed over it. A
// crash or a full disk part-way leaves the old file as it was, never half
// of the new one.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if err := f.Chmod(perm); err != nil && runtime.GOOS != "windows" {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	if runtime.GOOS != "windows" { // the rename itself, on disk
		if d, err := os.Open(dir); err == nil {
			d.Sync()
			d.Close()
		}
	}
	return nil
}

// quarantine moves a file that can't be read aside (path.corrupt-<time>),
// so it isn't overwritten and can be looked at; returns where it went.
func quarantine(path string) string {
	to := fmt.Sprintf("%s.corrupt-%s", path, time.Now().Format("20060102-150405"))
	if err := os.Rename(path, to); err != nil {
		return ""
	}
	return to
}

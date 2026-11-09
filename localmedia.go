package main

// Videos and audio in chat, on Linux.
//
// The web view there (WebKitGTK) plays <video>/<audio> through GStreamer,
// and that has two problems:
//
//  1. GStreamer can't open the app's own wails:// addresses, so an
//     encrypted attachment (served decrypted by the app, filecrypt.go)
//     wouldn't load at all. So on Linux media elements get it from a small
//     server on this computer instead: http://127.0.0.1:<port>/<token>/…,
//     on a random port, answering only requests that carry a random token
//     made at start (and only to the 127.0.0.1 address itself, so a web
//     page can't reach it through a made-up domain name). Nothing is
//     opened to the network.
//
//  2. What it can decode depends on the distro's GStreamer plugins: Fedora
//     can't play H.264 or HEVC out of the box, and MKV with unusual codecs
//     fails everywhere. When a video won't play, the page asks for a
//     playable copy (PlayableMedia): the bundled FFmpeg converts it once to
//     WebM — VP8 video and Opus sound, which every distro's GStreamer
//     plays — kept in the cache folder for next time (up to a few GB, the
//     oldest removed first).
//
// On Windows and macOS none of this is used: their web views play the
// app's own addresses and the usual formats (GetMediaLink is GetFileLink).

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const playableCacheMax = 4 << 30 // bytes of converted copies kept at most

var localMedia struct {
	once  sync.Once
	base  string // http://127.0.0.1:<port>/<token>
	host  string // 127.0.0.1:<port>
	token string
	err   error

	mu   sync.Mutex
	busy map[string]chan struct{} // conversions under way, by file name
}

// mediaServer starts the local server (once) and returns its base address.
func (a *App) mediaServer() (string, error) {
	localMedia.once.Do(func() {
		var tok [16]byte
		if _, err := rand.Read(tok[:]); err != nil {
			localMedia.err = err
			return
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			localMedia.err = err
			return
		}
		localMedia.token = hex.EncodeToString(tok[:])
		localMedia.host = ln.Addr().String()
		localMedia.base = "http://" + localMedia.host + "/" + localMedia.token
		localMedia.busy = map[string]chan struct{}{}
		srv := &http.Server{Handler: http.HandlerFunc(a.serveLocalMedia), ReadHeaderTimeout: 10 * time.Second}
		go srv.Serve(ln)
		log.Printf("media: serving chat media to the web view on %s", localMedia.host)
	})
	return localMedia.base, localMedia.err
}

func (a *App) serveLocalMedia(w http.ResponseWriter, r *http.Request) {
	if r.Host != localMedia.host || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		http.Error(w, "no", http.StatusForbidden)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/"+localMedia.token)
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasPrefix(rest, fileLocalPrefix): // an encrypted attachment, decrypted
		r2 := r.Clone(r.Context())
		r2.URL.Path = rest
		a.serveE2EEFile(w, r2)
	case strings.HasPrefix(rest, "/playable/"): // a converted copy
		name := strings.TrimPrefix(rest, "/playable/")
		if !playableName(name) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "video/webm")
		http.ServeFile(w, r, filepath.Join(playableDir(), name))
	default:
		http.NotFound(w, r)
	}
}

// GetMediaLink is GetFileLink for <video>/<audio> (see the top of this
// file): on Linux, an encrypted attachment comes from the local server.
func (a *App) GetMediaLink(path string) (string, error) {
	if goruntime.GOOS != "linux" || !strings.HasPrefix(path, fileLocalPrefix) {
		return a.GetFileLink(path)
	}
	base, err := a.mediaServer()
	if err != nil {
		return "", err
	}
	return base + path, nil
}

func playableDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "chriscord", "playable")
}

func playableName(name string) bool {
	base, ok := strings.CutSuffix(name, ".webm")
	if !ok || len(base) != 32 {
		return false
	}
	_, err := hex.DecodeString(base)
	return err == nil
}

// PlayableMedia makes (or finds) a copy of an attachment that this
// computer can play — WebM with VP8 and Opus — and returns its address.
// kind is "video" or "audio". Linux only. Can take a while for a long
// video: the page shows "Converting…" meanwhile.
func (a *App) PlayableMedia(path, kind string) (string, error) {
	if goruntime.GOOS != "linux" {
		return "", errors.New("not needed here")
	}
	base, err := a.mediaServer()
	if err != nil {
		return "", err
	}
	if findFFmpeg() == "" {
		return "", errors.New("FFmpeg isn't available")
	}
	enc, _ := ffmpegCmd("-hide_banner", "-encoders").Output()
	if !strings.Contains(string(enc), " libvpx ") || !strings.Contains(string(enc), " opus ") {
		return "", errors.New("this FFmpeg can't convert videos for playing — rebuild it (ffmpeg/build-ffmpeg.sh linux)")
	}
	sum := sha256.Sum256([]byte(path))
	name := hex.EncodeToString(sum[:16]) + ".webm"
	url := base + "/playable/" + name
	dir := playableDir()
	out := filepath.Join(dir, name)

	// One conversion per file at a time; others asking wait for it.
	for {
		if fi, err := os.Stat(out); err == nil && fi.Size() > 0 {
			now := time.Now()
			os.Chtimes(out, now, now) // recently used: kept longest
			return url, nil
		}
		localMedia.mu.Lock()
		wait, running := localMedia.busy[name]
		if !running {
			wait = make(chan struct{})
			localMedia.busy[name] = wait
		}
		localMedia.mu.Unlock()
		if running {
			<-wait
			continue
		}
		err := a.convertPlayable(path, kind, dir, out)
		localMedia.mu.Lock()
		delete(localMedia.busy, name)
		close(wait)
		localMedia.mu.Unlock()
		if err != nil {
			return "", err
		}
		trimPlayableCache(dir)
		return url, nil
	}
}

func (a *App) convertPlayable(path, kind, dir, out string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	// The original, decrypted, to a temporary file (this FFmpeg has no
	// network support, and seeking in the input needs a file).
	src, err := os.CreateTemp(dir, "src-*")
	if err != nil {
		return err
	}
	defer os.Remove(src.Name())
	if strings.HasPrefix(path, fileLocalPrefix) {
		e := a.text.fileFor(path)
		if e == nil {
			src.Close()
			return errors.New("that file isn't known here")
		}
		err = a.decryptTo(src, e)
	} else {
		var link string
		if link, err = a.GetFileLink(path); err == nil {
			var resp *http.Response
			if resp, err = http.Get(link); err == nil {
				if resp.StatusCode >= 400 {
					err = fmt.Errorf("HTTP %d", resp.StatusCode)
				} else {
					_, err = io.Copy(src, resp.Body)
				}
				resp.Body.Close()
			}
		}
	}
	if cerr := src.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("couldn't get the file: %w", err)
	}

	tmp := out + ".part"
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y", "-i", src.Name()}
	if kind == "video" {
		threads := strconv.Itoa(max(2, min(8, goruntime.NumCPU())))
		args = append(args, "-map", "0:v:0", "-map", "0:a:0?",
			// At most 1280 wide (as shown in the app), fast: about real
			// time or quicker on most computers.
			"-vf", "scale=w='min(1280,iw)':h=-2", "-pix_fmt", "yuv420p",
			"-c:v", "libvpx", "-deadline", "realtime", "-cpu-used", "8", "-threads", threads,
			"-b:v", "3M", "-crf", "10", "-qmin", "4", "-qmax", "50", "-auto-alt-ref", "0")
	} else {
		args = append(args, "-map", "0:a:0", "-vn")
	}
	args = append(args, "-c:a", "opus", "-strict", "-2", "-b:a", "128k", "-ar", "48000", "-ac", "2",
		"-f", "webm", tmp)
	t0 := time.Now()
	cmd := ffmpegCmd(args...)
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, n: 4096}
	if err := cmd.Run(); err != nil {
		os.Remove(tmp)
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		log.Printf("media: converting %s failed: %s", path, msg)
		return fmt.Errorf("couldn't convert it: %s", lastLine([]byte(msg)))
	}
	if err := os.Rename(tmp, out); err != nil {
		return err
	}
	log.Printf("media: converted %s for playing in %s", path, time.Since(t0).Round(100*time.Millisecond))
	return nil
}

// trimPlayableCache removes the least recently used copies past the limit.
func trimPlayableCache(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type f struct {
		path string
		size int64
		t    time.Time
	}
	var files []f
	var total int64
	for _, e := range ents {
		if !playableName(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, f{filepath.Join(dir, e.Name()), fi.Size(), fi.ModTime()})
		total += fi.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].t.Before(files[j].t) })
	for _, x := range files {
		if total <= playableCacheMax {
			break
		}
		if os.Remove(x.path) == nil {
			total -= x.size
		}
	}
}

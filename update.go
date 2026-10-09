package main

// The "update ready" notice in the title bar (the green download arrow next
// to minimise, like Discord's).
//
// Chriscord is installed and kept up to date by the Chriscord launcher
// (chriscord-dist/updater), which lays the install out as:
//
//	%LOCALAPPDATA%\Programs\Chriscord\
//	    Chriscord.exe      the launcher / updater
//	    state.json         which build it installed (channel, sha256, …)
//	    app\chriscord.exe  this program
//
// The launcher already updates everything each time it starts, so all the
// running app has to do is notice when a newer build has come out since it
// was started, and offer to restart through the launcher to pick it up.
//
// It's kept as cheap as possible:
//   - It only runs when started from an install like the one above. A dev
//     build, or a Linux/macOS build, never checks anything.
//   - It's a single goroutine asleep on a timer between checks; no CPU in
//     between. The first check is a minute after start (the launcher has
//     only just checked), then once an hour.
//   - Each check is one small GET of the release manifest (well under 1 KB),
//     sent with the ETag of the last one, so an unchanged manifest comes
//     back as an empty "304 Not Modified".
//   - Once an update is found, it stops checking: there's nothing more to
//     tell you until you restart.
//   - If the launcher installs a newer build on disk while this copy is
//     running (you clicked the shortcut again), the notice shows straight
//     away, with no network request at all.
//
// The manifest isn't signature-checked here: the worst a forged one could do
// is show the arrow. The launcher checks the signature before it installs
// anything, exactly as it does on every start.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	updateManifestBase = "https://chriscord.app/dl" // same as the launcher's
	updateFirstCheck   = 1 * time.Minute
	updateCheckEvery   = 1 * time.Hour
	launcherExeName    = "Chriscord.exe"
	launcherStateName  = "state.json"
)

// launcherState is the part of the launcher's state.json this needs.
type launcherState struct {
	Channel  string `json:"channel"`
	Released string `json:"released"`
	Version  string `json:"version"`
	Commit   string `json:"commit"`
	SHA256   string `json:"sha256"`
}

// UpdateStatus is what the frontend gets (UpdateStatus() and the
// "update:available" event).
type UpdateStatus struct {
	Available bool   `json:"available"`
	Version   string `json:"version"`
}

type updateWatcher struct {
	mu      sync.Mutex
	dir     string        // the install folder; "" when not installed by the launcher
	running launcherState // the build this process was started as
	status  UpdateStatus
	kick    chan struct{} // "look at the disk now": another launch just happened
	client  *http.Client
}

// findInstall reports the launcher's install folder if this exe is the one
// the launcher manages (…\app\chriscord.exe next to …\Chriscord.exe and
// …\state.json), and what build that is.
func findInstall() (dir string, st launcherState, ok bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", st, false
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	appDir := filepath.Dir(exe)
	if !strings.EqualFold(filepath.Base(appDir), "app") {
		return "", st, false
	}
	dir = filepath.Dir(appDir)
	if _, err := os.Stat(filepath.Join(dir, launcherExeName)); err != nil {
		return "", st, false
	}
	st, err = readLauncherState(dir)
	if err != nil || st.SHA256 == "" || st.Channel == "" {
		return "", st, false
	}
	return dir, st, true
}

func readLauncherState(dir string) (launcherState, error) {
	var st launcherState
	b, err := os.ReadFile(filepath.Join(dir, launcherStateName))
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(b, &st)
	return st, err
}

// startUpdateWatcher is called from startup. The launcher writes state.json
// before it starts the app, so what's there now is the build that's running.
func (a *App) startUpdateWatcher() {
	u := &a.updates
	u.kick = make(chan struct{}, 1)
	dir, st, ok := findInstall()
	if !ok {
		return // not installed by the launcher: nothing to check against
	}
	u.mu.Lock()
	u.dir, u.running = dir, st
	u.client = &http.Client{Timeout: 15 * time.Second}
	u.mu.Unlock()
	go a.watchForUpdates()
}

func (a *App) watchForUpdates() {
	u := &a.updates
	timer := time.NewTimer(updateFirstCheck)
	defer timer.Stop()
	etag := ""
	for {
		fromNetwork := false
		select {
		case <-timer.C:
			fromNetwork = true
		case <-u.kick:
		}
		found, version := u.newerOnDisk()
		if !found && fromNetwork {
			found, version, _ = u.newerOnServer(&etag) // offline etc.: just try again next time
		}
		if found {
			a.announceUpdate(version)
			return
		}
		if fromNetwork {
			timer.Reset(updateCheckEvery)
		}
	}
}

// newerOnDisk: has the launcher installed a different build since this one
// started? Then a restart is all it takes.
func (u *updateWatcher) newerOnDisk() (bool, string) {
	st, err := readLauncherState(u.dir)
	if err != nil || st.SHA256 == "" {
		return false, ""
	}
	if strings.EqualFold(st.SHA256, u.running.SHA256) {
		return false, ""
	}
	return true, st.Version
}

// announceUpdate records that an update is waiting and tells the frontend
// (the title bar arrow and Settings > About).
func (a *App) announceUpdate(version string) {
	u := &a.updates
	u.mu.Lock()
	u.status = UpdateStatus{Available: true, Version: version}
	s := u.status
	u.mu.Unlock()
	runtime.EventsEmit(a.ctx, "update:available", s)
}

// newerOnServer: is there a newer build on this channel than the running
// one? err is for a check that couldn't be done (offline, server error);
// an unchanged manifest (304) is simply "no".
func (u *updateWatcher) newerOnServer(etag *string) (bool, string, error) {
	base := updateManifestBase
	if v := os.Getenv("CHRISCORD_UPDATE_URL"); v != "" { // same override as the launcher
		base = strings.TrimRight(v, "/")
	}
	url := fmt.Sprintf("%s/client/%s/latest.json", base, u.running.Channel)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return false, "", err
	}
	req.Header.Set("User-Agent", "Chriscord/"+u.running.Version)
	if *etag != "" {
		req.Header.Set("If-None-Match", *etag)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return false, "", fmt.Errorf("couldn't reach chriscord.app")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == http.StatusNotModified {
			return false, "", nil // same manifest as last time
		}
		return false, "", fmt.Errorf("chriscord.app answered %s", resp.Status)
	}
	var m struct {
		Channel  string `json:"channel"`
		Version  string `json:"version"`
		Released string `json:"released"`
		Builds   map[string]struct {
			SHA256 string `json:"sha256"`
		} `json:"builds"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&m); err != nil {
		return false, "", fmt.Errorf("the release info from chriscord.app couldn't be read")
	}
	*etag = resp.Header.Get("ETag")

	b, ok := m.Builds[goruntime.GOOS+"-"+goruntime.GOARCH]
	switch {
	case !ok || b.SHA256 == "":
		return false, "", nil // nothing published for this platform
	case m.Channel != "" && m.Channel != u.running.Channel:
		return false, "", nil
	case strings.EqualFold(b.SHA256, u.running.SHA256):
		return false, "", nil // that's us
	case u.running.Released != "" && m.Released <= u.running.Released:
		return false, "", nil // not newer (the launcher would refuse it anyway)
	}
	return true, m.Version, nil
}

// onSecondLaunch is called when Chriscord was started again while running.
// That launch went through the launcher, which may just have installed a
// newer build, so look at the disk now instead of waiting for the next
// check.
func (u *updateWatcher) onSecondLaunch() {
	if u.kick == nil {
		return
	}
	select {
	case u.kick <- struct{}{}:
	default:
	}
}

// ── Exposed to frontend ───────────────────────────────────────────────────────

// UpdateStatus says whether an update is waiting (for the title bar arrow,
// in case the "update:available" event fired before the page was ready).
func (a *App) UpdateStatus() UpdateStatus {
	a.updates.mu.Lock()
	defer a.updates.mu.Unlock()
	return a.updates.status
}

// AboutInfo is what Settings > About shows.
type AboutInfo struct {
	// Installed: started from an install made by the Chriscord launcher,
	// so it can update itself. False for a dev build or a copied exe.
	Installed bool         `json:"installed"`
	Version   string       `json:"version"`  // e.g. 2026.10.08-302bf3a, as on chriscord.app
	Commit    string       `json:"commit"`   // the full git commit it was built from
	Channel   string       `json:"channel"`  // stable or testing
	Released  string       `json:"released"` // RFC 3339
	Update    UpdateStatus `json:"update"`
}

// GetAboutInfo describes the running build.
func (a *App) GetAboutInfo() AboutInfo {
	u := &a.updates
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.dir == "" {
		return AboutInfo{}
	}
	r := u.running
	return AboutInfo{Installed: true, Version: r.Version, Commit: r.Commit, Channel: r.Channel, Released: r.Released, Update: u.status}
}

// CheckForUpdates is the "Check for updates" button: checks right now
// (one small request, same as the hourly check) and says whether a newer
// build is out. If there is one, the title bar arrow appears too.
func (a *App) CheckForUpdates() (UpdateStatus, error) {
	u := &a.updates
	u.mu.Lock()
	dir, status := u.dir, u.status
	u.mu.Unlock()
	if dir == "" {
		return UpdateStatus{}, fmt.Errorf("this copy of Chriscord wasn't installed with the Chriscord installer, so it can't update itself")
	}
	if status.Available {
		return status, nil
	}
	found, version := u.newerOnDisk()
	if !found {
		noETag := "" // always the full manifest: the answer has to be fresh
		var err error
		if found, version, err = u.newerOnServer(&noETag); err != nil {
			return UpdateStatus{}, err
		}
	}
	if found {
		a.announceUpdate(version)
		return UpdateStatus{Available: true, Version: version}, nil
	}
	return UpdateStatus{}, nil
}

// RestartToUpdate closes Chriscord and starts the launcher, which installs
// the update and opens Chriscord again. --wait-pid tells the launcher to
// let this process finish closing before it starts the new one; otherwise
// the new one could find this one still running and just hand over to it.
func (a *App) RestartToUpdate() error {
	a.updates.mu.Lock()
	dir := a.updates.dir
	a.updates.mu.Unlock()
	if dir == "" {
		return fmt.Errorf("this copy of Chriscord wasn't installed by the Chriscord launcher")
	}
	cmd := exec.Command(filepath.Join(dir, launcherExeName), "--wait-pid", strconv.Itoa(os.Getpid()))
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("couldn't start the updater: %w", err)
	}
	cmd.Process.Release()
	runtime.Quit(a.ctx) // leaves any call and closes cleanly (see shutdown)
	return nil
}

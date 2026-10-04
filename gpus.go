package main

import (
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// listGPUs names the graphics cards in this computer (for Settings, so it
// can say "Found NVIDIA GeForce RTX 3080 Ti, but…" rather than just "no
// graphics card"). Asked once; nil if the system won't say.
var (
	gpuListOnce sync.Once
	gpuList     []string
)

func listGPUs() []string {
	gpuListOnce.Do(func() { gpuList = readGPUs() })
	return gpuList
}

func readGPUs() []string {
	var names []string
	switch goruntime.GOOS {
	case "windows":
		out := runQuiet("powershell", "-NoProfile", "-NonInteractive", "-Command",
			"Get-CimInstance Win32_VideoController | ForEach-Object { $_.Name }")
		names = splitLines(out)
	case "linux":
		// NVIDIA's driver names its cards here…
		files, _ := filepath.Glob("/proc/driver/nvidia/gpus/*/information")
		for _, f := range files {
			data, _ := os.ReadFile(f)
			for _, l := range strings.Split(string(data), "\n") {
				if k, v, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "Model" {
					names = append(names, "NVIDIA "+strings.TrimSpace(v))
				}
			}
		}
		// …and lspci knows everyone's.
		if len(names) == 0 {
			for _, l := range splitLines(runQuiet("lspci")) {
				if strings.Contains(l, "VGA compatible controller") || strings.Contains(l, "3D controller") || strings.Contains(l, "Display controller") {
					if _, desc, ok := strings.Cut(l, ": "); ok {
						names = append(names, desc)
					}
				}
			}
		}
	case "darwin":
		for _, l := range splitLines(runQuiet("system_profiler", "SPDisplaysDataType")) {
			if k, v, ok := strings.Cut(strings.TrimSpace(l), ":"); ok && k == "Chipset Model" {
				names = append(names, strings.TrimSpace(v))
			}
		}
	}
	// Skip remote-desktop / virtual adapters, and repeats.
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		low := strings.ToLower(n)
		if n == "" || seen[n] || strings.Contains(low, "remote display") || strings.Contains(low, "basic display") || strings.Contains(low, "virtual") {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	// NVIDIA first: it's the one the encoder check cares about most.
	sort.SliceStable(out, func(i, j int) bool {
		return strings.Contains(strings.ToUpper(out[i]), "NVIDIA") && !strings.Contains(strings.ToUpper(out[j]), "NVIDIA")
	})
	return out
}

// runQuiet runs a command without a console window, giving up after 8 s.
func runQuiet(name string, args ...string) string {
	cmd := exec.Command(name, args...)
	hideWindow(cmd)
	done := make(chan []byte, 1)
	go func() { out, _ := cmd.Output(); done <- out }()
	select {
	case out := <-done:
		return string(out)
	case <-time.After(8 * time.Second):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		return ""
	}
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r", ""), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

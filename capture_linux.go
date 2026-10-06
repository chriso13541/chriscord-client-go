//go:build linux

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Native capture on Linux: cameras through V4L2.

func cameraInputFormat() string { return "v4l2" }

func captureProcAttr(cmd *exec.Cmd) {}

// listCameraDevices: the /dev/video* nodes that capture video, named as the
// driver names them (cameras often have a second node for metadata,
// which is skipped).
func listCameraDevices() ([]CameraDevice, error) {
	nodes, _ := filepath.Glob("/sys/class/video4linux/video*")
	sort.Strings(nodes)
	var cams []CameraDevice
	for _, n := range nodes {
		if idx, _ := os.ReadFile(filepath.Join(n, "index")); strings.TrimSpace(string(idx)) != "" && strings.TrimSpace(string(idx)) != "0" {
			continue // a camera's extra (metadata) node
		}
		name, _ := os.ReadFile(filepath.Join(n, "name"))
		dev := "/dev/" + filepath.Base(n)
		label := strings.TrimSpace(string(name))
		if label == "" {
			label = dev
		}
		cams = append(cams, CameraDevice{ID: dev, Name: label})
	}
	return cams, nil
}

var reV4L2Fmt = regexp.MustCompile(`(Raw|Compressed)\s*:\s*(\S+)\s*:[^:]*:\s*([\dx ]+)`)

// listCameraModes: V4L2 lists sizes per format, not frame rates, so 30 is
// assumed (what nearly every webcam does at its listed sizes).
func listCameraModes(id string) ([]CameraMode, error) {
	out, _ := ffmpegCmd("-hide_banner", "-f", "v4l2", "-list_formats", "all", "-i", id).CombinedOutput()
	var all []CameraMode
	for _, m := range reV4L2Fmt.FindAllStringSubmatch(string(out), -1) {
		for _, sz := range strings.Fields(m[3]) {
			wh := strings.SplitN(sz, "x", 2)
			if len(wh) != 2 {
				continue
			}
			w, _ := strconv.Atoi(wh[0])
			h, _ := strconv.Atoi(wh[1])
			if w > 0 && h > 0 {
				all = append(all, CameraMode{W: w, H: h, FPS: 30, Format: m[2]})
			}
		}
	}
	if len(all) == 0 {
		return nil, errors.New("the camera didn't report any modes")
	}
	// Prefer MJPEG at larger sizes: raw formats usually can't do 30 fps there.
	for i := range all {
		if all[i].Format != "mjpeg" && all[i].W*all[i].H >= 1280*720 {
			all[i].FPS = 10
		}
	}
	return bestModes(all), nil
}

func cameraInputArgs(o CameraStart) []string {
	args := []string{"-f", "v4l2", "-video_size", strconv.Itoa(o.W) + "x" + strconv.Itoa(o.H), "-framerate", strconv.Itoa(o.ModeFPS)}
	if o.Format != "" {
		args = append(args, "-input_format", o.Format)
	}
	return append(args, "-i", o.ID)
}

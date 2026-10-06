//go:build windows

package main

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// Native capture on Windows: cameras through DirectShow.

func cameraInputFormat() string { return "dshow" }

// Capture runs at normal priority (it's live), just without a console.
func captureProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

// listCameraDevices asks DirectShow (through FFmpeg) for its cameras. Each
// is opened by its "alternative name" (a stable device path) when it has
// one, so two identical webcams can be told apart.
func listCameraDevices() ([]CameraDevice, error) {
	out, _ := ffmpegCmd("-hide_banner", "-list_devices", "true", "-f", "dshow", "-i", "dummy").CombinedOutput()
	var cams []CameraDevice
	for _, line := range strings.Split(string(out), "\n") {
		if m := reDevDshow.FindStringSubmatch(line); m != nil {
			cams = append(cams, CameraDevice{ID: m[1], Name: m[1]})
			continue
		}
		if m := reAltDshow.FindStringSubmatch(line); m != nil && len(cams) > 0 && cams[len(cams)-1].ID == cams[len(cams)-1].Name {
			cams[len(cams)-1].ID = m[1]
		}
	}
	if cams == nil && !strings.Contains(string(out), "dshow") {
		return nil, errors.New("FFmpeg couldn't list cameras")
	}
	return cams, nil
}

// listCameraModes asks the driver what the camera produces natively.
func listCameraModes(id string) ([]CameraMode, error) {
	out, _ := ffmpegCmd("-hide_banner", "-list_options", "true", "-f", "dshow", "-i", "video="+id).CombinedOutput()
	var all []CameraMode
	for _, m := range reModeDshow.FindAllStringSubmatch(string(out), -1) {
		w, _ := strconv.Atoi(m[6])
		h, _ := strconv.Atoi(m[7])
		fps, _ := strconv.ParseFloat(m[8], 64)
		if w <= 0 || h <= 0 || fps < 1 {
			continue
		}
		all = append(all, CameraMode{W: w, H: h, FPS: int(fps + .5), Format: m[2]})
	}
	if len(all) == 0 {
		if strings.Contains(strings.ToLower(string(out)), "could not find") {
			return nil, errors.New("that camera isn't connected any more")
		}
		return nil, errors.New("the camera didn't report any modes")
	}
	return bestModes(all), nil
}

func cameraInputArgs(o CameraStart) []string {
	// A small capture buffer: enough to ride out a hiccup (about 8 frames of
	// uncompressed 720p), without holding 64 MB in memory for nothing.
	args := []string{"-f", "dshow", "-rtbufsize", "16M",
		"-video_size", strconv.Itoa(o.W) + "x" + strconv.Itoa(o.H), "-framerate", strconv.Itoa(o.ModeFPS)}
	if o.Format == "mjpeg" || o.Format == "h264" {
		args = append(args, "-vcodec", o.Format)
	} else if o.Format != "" {
		args = append(args, "-pixel_format", o.Format)
	}
	return append(args, "-i", "video="+o.ID)
}

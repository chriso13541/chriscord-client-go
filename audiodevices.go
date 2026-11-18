package main

// The microphones and speakers listed in Settings > Voice, on Linux.
//
// The page can't list them there: WebKitGTK's device list (through
// GStreamer) comes back empty or nameless. And the names it would give
// wouldn't help anyway — calls open sound through PortAudio, whose Linux
// devices are ALSA's: raw sound cards (which the desktop's sound server
// usually has open already) plus the sound server's own "pipewire" or
// "pulse" device, which plays to whatever the desktop's default is.
//
// So here the list comes from the sound server itself (PipeWire's pw-dump,
// or pactl for PulseAudio — also PipeWire's pulse server), and choosing
// one opens the sound server's device aimed at it: PIPEWIRE_NODE for
// PipeWire's ALSA device, PULSE_SINK / PULSE_SOURCE for PulseAudio's, set
// just while the stream opens (openAudioStream). Without a sound server
// the list is PortAudio's own devices.
//
// The page passes the choice back as "sys:<node name>" (a sound server
// device) or "pa:<PortAudio name>"; "" is the desktop's default. Anything
// else is an older saved name, matched as before (pickDevice).

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gordonklaus/portaudio"
)

type AudioDevice struct {
	ID      string `json:"id"`    // what to pass back ("sys:…" / "pa:…")
	Label   string `json:"label"` // shown in the list
	Default bool   `json:"default"`
}

type AudioDeviceList struct {
	Supported bool          `json:"supported"` // false: the page lists them itself (not Linux)
	Server    string        `json:"server"`    // "PipeWire", "PulseAudio" or "" (ALSA only)
	Inputs    []AudioDevice `json:"inputs"`
	Outputs   []AudioDevice `json:"outputs"`
}

// ListAudioDevices: for Settings > Voice (Linux; elsewhere Supported is
// false and the page lists them itself).
func (a *App) ListAudioDevices() AudioDeviceList {
	if runtime.GOOS != "linux" {
		return AudioDeviceList{}
	}
	out := AudioDeviceList{Supported: true}
	if soundServerDevice(true) != nil || soundServerDevice(false) != nil {
		if in, outs, ok := pipewireDevices(); ok {
			out.Server, out.Inputs, out.Outputs = "PipeWire", in, outs
			return out
		}
		if in, outs, ok := pulseDevices(); ok {
			out.Server, out.Inputs, out.Outputs = "PulseAudio", in, outs
			return out
		}
	}
	// No sound server (or no way to ask it): PortAudio's own devices.
	devices, _ := portaudio.Devices()
	defIn, _ := portaudio.DefaultInputDevice()
	defOut, _ := portaudio.DefaultOutputDevice()
	for _, d := range devices {
		if d.MaxInputChannels > 0 {
			out.Inputs = append(out.Inputs, AudioDevice{ID: "pa:" + d.Name, Label: d.Name, Default: d == defIn})
		}
		if d.MaxOutputChannels > 0 {
			out.Outputs = append(out.Outputs, AudioDevice{ID: "pa:" + d.Name, Label: d.Name, Default: d == defOut})
		}
	}
	return out
}

func runAudioTool(d time.Duration, name string, args ...string) ([]byte, error) {
	if _, err := exec.LookPath(name); err != nil {
		return nil, err
	}
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	t := time.AfterFunc(d, func() { cmd.Process.Kill() })
	err := cmd.Wait()
	t.Stop()
	return buf.Bytes(), err
}

// pipewireDevices: PipeWire's sinks and sources, from pw-dump.
func pipewireDevices() (inputs, outputs []AudioDevice, ok bool) {
	b, err := runAudioTool(3*time.Second, "pw-dump", "--no-colors")
	if err != nil {
		return nil, nil, false
	}
	var objs []struct {
		Type string `json:"type"`
		Info struct {
			Props    map[string]any `json:"props"`
			Metadata []struct {
				Key   string `json:"key"`
				Value any    `json:"value"`
			} `json:"metadata"`
		} `json:"info"`
		Metadata []struct {
			Key   string `json:"key"`
			Value any    `json:"value"`
		} `json:"metadata"`
		Props map[string]any `json:"props"`
	}
	if json.Unmarshal(b, &objs) != nil {
		return nil, nil, false
	}
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	defSink, defSource := "", ""
	for _, o := range objs {
		if o.Type != "PipeWire:Interface:Metadata" || str(o.Props, "metadata.name") != "default" {
			continue
		}
		for _, m := range o.Metadata {
			v, _ := m.Value.(map[string]any)
			switch m.Key {
			case "default.audio.sink":
				defSink = str(v, "name")
			case "default.audio.source":
				defSource = str(v, "name")
			}
		}
	}
	for _, o := range objs {
		if o.Type != "PipeWire:Interface:Node" {
			continue
		}
		p := o.Info.Props
		name := str(p, "node.name")
		if name == "" {
			continue
		}
		label := str(p, "node.description")
		if label == "" {
			label = str(p, "node.nick")
		}
		if label == "" {
			label = name
		}
		switch str(p, "media.class") {
		case "Audio/Sink", "Audio/Duplex":
			outputs = append(outputs, AudioDevice{ID: "sys:" + name, Label: label, Default: name == defSink})
		case "Audio/Source", "Audio/Source/Virtual":
			inputs = append(inputs, AudioDevice{ID: "sys:" + name, Label: label, Default: name == defSource})
		}
	}
	sortDevices(inputs)
	sortDevices(outputs)
	return inputs, outputs, true
}

// pulseDevices: PulseAudio's sinks and sources (not the monitors of
// sinks), from pactl.
func pulseDevices() (inputs, outputs []AudioDevice, ok bool) {
	info, err := runAudioTool(3*time.Second, "pactl", "info")
	if err != nil {
		return nil, nil, false
	}
	var defSink, defSource string
	for _, l := range strings.Split(string(info), "\n") {
		if v, ok := strings.CutPrefix(l, "Default Sink: "); ok {
			defSink = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(l, "Default Source: "); ok {
			defSource = strings.TrimSpace(v)
		}
	}
	list := func(kind, def string) []AudioDevice {
		b, err := runAudioTool(3*time.Second, "pactl", "list", kind)
		if err != nil {
			return nil
		}
		var out []AudioDevice
		var name string
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			l := strings.TrimSpace(sc.Text())
			if v, ok := strings.CutPrefix(l, "Name: "); ok {
				name = v
			} else if v, ok := strings.CutPrefix(l, "Description: "); ok && name != "" {
				if kind == "sources" && strings.HasSuffix(name, ".monitor") {
					name = ""
					continue
				}
				out = append(out, AudioDevice{ID: "sys:" + name, Label: v, Default: name == def})
				name = ""
			}
		}
		sortDevices(out)
		return out
	}
	return list("sources", defSource), list("sinks", defSink), true
}

func sortDevices(d []AudioDevice) {
	sort.SliceStable(d, func(i, j int) bool { return strings.ToLower(d[i].Label) < strings.ToLower(d[j].Label) })
}

// ── Opening one ─────────────────────────────────────────────────────────

var audioOpenMu sync.Mutex // the target variables are the whole process's

// openAudioStream is portaudio.OpenStream for the device chosen as name
// (see the top of this file): a "sys:" choice sets the sound server's
// target variables just while the stream opens.
func openAudioStream(name string, input bool, p portaudio.StreamParameters, args ...interface{}) (*portaudio.Stream, error) {
	node, ok := strings.CutPrefix(name, "sys:")
	if !ok || runtime.GOOS != "linux" {
		return portaudio.OpenStream(p, args...)
	}
	audioOpenMu.Lock()
	defer audioOpenMu.Unlock()
	pulse := "PULSE_SINK"
	if input {
		pulse = "PULSE_SOURCE"
	}
	vars := []string{"PIPEWIRE_NODE", pulse}
	old := map[string]*string{}
	for _, k := range vars {
		if v, set := os.LookupEnv(k); set {
			old[k] = &v
		} else {
			old[k] = nil
		}
		os.Setenv(k, node)
	}
	defer func() {
		for _, k := range vars {
			if v := old[k]; v != nil {
				os.Setenv(k, *v)
			} else {
				os.Unsetenv(k)
			}
		}
	}()
	return portaudio.OpenStream(p, args...)
}

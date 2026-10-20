//go:build !windows

package main

// usage_other.go — Settings → About's memory/CPU/GPU readout is Windows
// only for now (usage_windows.go); elsewhere the page doesn't show it.

const usageSupported = false

// UsagePart is one kind of Chriscord's processes.
type UsagePart struct {
	Name      string `json:"name"`
	Processes int    `json:"processes"`
	Memory    uint64 `json:"memory"`
}

// Usage is what Chriscord is using right now.
type Usage struct {
	Supported bool        `json:"supported"`
	Memory    uint64      `json:"memory"`
	CPU       *float64    `json:"cpu"`
	GPU       *float64    `json:"gpu"`
	Parts     []UsagePart `json:"parts"`
	Processes int         `json:"processes"`
}

// GetUsage: not measured on this platform.
func (a *App) GetUsage() Usage { return Usage{} }

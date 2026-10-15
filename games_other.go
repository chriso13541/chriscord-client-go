//go:build !windows

package main

import (
	"path/filepath"
	"strings"
)

// Working out what game you're playing is Windows-only for now.

const gameDetectionSupported = false

func detectGame(string) string { return "" }

func openPrograms() []RunningProgram { return nil }

func programName(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func programIcon(string) string { return "" }

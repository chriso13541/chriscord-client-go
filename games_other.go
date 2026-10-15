//go:build !windows

package main

import (
	"path/filepath"
	"strings"
)

// Working out what game you're playing is Windows-only for now.

const gameDetectionSupported = false

func detectGame(string) (string, uint32) { return "", 0 }

func startForegroundWatch(func()) bool { return false }
func processAlive(uint32) bool         { return false }
func watchGameExit(uint32, func())     {}

func openPrograms() []RunningProgram { return nil }

func programName(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func programIcon(string) string { return "" }

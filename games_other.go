//go:build !windows

package main

// Working out what game you're playing is Windows-only for now.

const gameDetectionSupported = false

func detectGame(string) string { return "" }

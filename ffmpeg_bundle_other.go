//go:build !windows && !linux

package main

import "embed"

// macOS: nothing bundled — an ffmpeg on the PATH (e.g. Homebrew's) is used.
var bundledFFmpeg embed.FS

const bundledFFmpegDir, bundledFFmpegFile, ffmpegExeName = "", "", "ffmpeg"

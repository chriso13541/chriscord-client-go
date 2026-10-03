//go:build windows

package main

import "embed"

// The FFmpeg built by ffmpeg/build-ffmpeg.sh windows, if it was there at
// build time (otherwise only the README is embedded).
//
//go:embed ffmpeg/windows
var bundledFFmpeg embed.FS

const bundledFFmpegDir, bundledFFmpegFile, ffmpegExeName = "ffmpeg/windows", "ffmpeg.exe.gz", "ffmpeg.exe"

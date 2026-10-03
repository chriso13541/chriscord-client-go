//go:build linux

package main

import "embed"

// The FFmpeg built by ffmpeg/build-ffmpeg.sh linux, if it was there at
// build time (otherwise only the README is embedded).
//
//go:embed ffmpeg/linux
var bundledFFmpeg embed.FS

const bundledFFmpegDir, bundledFFmpegFile, ffmpegExeName = "ffmpeg/linux", "ffmpeg.gz", "ffmpeg"

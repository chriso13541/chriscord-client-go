//go:build linux

package main

import "embed"

// The FFmpeg built by ffmpeg/build-ffmpeg.sh linux, if it was there at
// build time (otherwise only the README is embedded).
//
//go:embed ffmpeg/linux
var bundledFFmpeg embed.FS

const bundledFFmpegDir, bundledFFmpegFile, ffmpegExeName = "ffmpeg/linux", "ffmpeg.gz", "ffmpeg"

// ffmpegPreferSystem: on Linux the distro's FFmpeg comes first when it's
// installed (sudo apt install ffmpeg): it's kept up to date, has every
// format and hardware encoder, and nothing has to be unpacked. The bundled
// one is for computers without it.
const ffmpegPreferSystem = true

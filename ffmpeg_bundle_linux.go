//go:build linux

package main

import "embed"

// The FFmpeg built by ffmpeg/build-ffmpeg.sh linux, if it was there at
// build time (otherwise only the README is embedded).
//
//go:embed ffmpeg/linux
var bundledFFmpeg embed.FS

const bundledFFmpegDir, bundledFFmpegFile, ffmpegExeName = "ffmpeg/linux", "ffmpeg.gz", "ffmpeg"

// ffmpegPreferSystem: the bundled FFmpeg comes first on Linux too, as on
// Windows: the same build everywhere, with everything sharing needs —
// distros' differ (Fedora's ffmpeg-free has no x264, Debian's may be too
// old to share single windows). The distro's is only used when the app was
// built without one (or it can't be unpacked).
const ffmpegPreferSystem = false

#!/bin/sh
# Builds the Linux FFmpeg (ffmpeg/linux/ffmpeg.gz) inside an Ubuntu 22.04
# container, so it runs on any distro from about 2022 on.
#
#   sh ffmpeg/build-ffmpeg-linux-portable.sh
#
# Why a container: a Linux program only runs where the system's C library
# (glibc) is at least as new as the one it was built against. Built straight
# on Fedora 43 or Ubuntu 25.10 it would refuse to start on a friend's older
# Debian or Mint ("GLIBC_2.41 not found"). Built on 22.04 (glibc 2.35) it
# runs on Ubuntu 22.04+, Debian 12+, Fedora 36+, Mint 21+, Arch, etc.
#
# Needs podman (Fedora has it) or docker. Everything else is installed
# inside the container; your system is left alone. The build folder is
# ffmpeg/.build-linux-portable (delete it after changing versions in
# build-ffmpeg.sh). Takes ~5–15 minutes the first time.
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(dirname "$HERE")
if command -v podman >/dev/null 2>&1; then RUN=podman
elif command -v docker >/dev/null 2>&1; then RUN=docker
else echo "needs podman or docker (Fedora: sudo dnf install podman; Ubuntu: sudo apt install podman)" >&2; exit 1
fi

# :Z relabels the folder for SELinux (Fedora); harmless elsewhere.
$RUN run --rm -v "$REPO:/src:Z" -w /src docker.io/library/ubuntu:22.04 sh -euc '
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq build-essential nasm pkg-config curl ca-certificates \
    libxcb1-dev libxcb-shm0-dev libxcb-xfixes0-dev libxcb-shape0-dev >/dev/null
  WORK=/src/ffmpeg/.build-linux-portable sh ffmpeg/build-ffmpeg.sh linux
  chown -R '"$(id -u):$(id -g)"' ffmpeg/linux ffmpeg/.build-linux-portable
'

echo
echo "Built: ffmpeg/linux/ffmpeg.gz ($(du -h "$HERE/linux/ffmpeg.gz" | cut -f1))"
echo "Commit ffmpeg/linux/ (ffmpeg.gz, FFMPEG-LICENSE.txt, FFMPEG-SOURCE.txt), then build the app as usual."

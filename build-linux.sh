#!/bin/sh
# Builds chriscord for Linux (x86-64): build/bin/chriscord.
#
# Run it from the repo folder:
#   sh build-linux.sh
#
# Needs (build machine only):
#   Debian/Ubuntu:  sudo apt install build-essential pkg-config libgtk-3-dev \
#                     libwebkit2gtk-4.1-dev portaudio19-dev libopus-dev libpipewire-0.3-dev \
#                     nasm curl libxcb1-dev libxcb-shm0-dev libxcb-xfixes0-dev libxcb-shape0-dev
#                   (older releases: libwebkit2gtk-4.0-dev instead of 4.1)
#   Arch:           sudo pacman -S --needed base-devel gtk3 webkit2gtk-4.1 portaudio opus pipewire nasm curl libxcb
#   Fedora:         sudo dnf install gcc gcc-c++ make pkgconf gtk3-devel webkit2gtk4.1-devel portaudio-devel opus-devel \
#                     pipewire-devel nasm curl libxcb-devel
#   (nasm, curl and the xcb headers are for building the bundled FFmpeg.)
#   plus Go and the wails CLI (go install github.com/wailsapp/wails/v2/cmd/wails@latest).
#
# The people you send it to need the same libraries' runtime parts — GTK 3,
# WebKitGTK, PortAudio and Opus — which nearly every desktop already has
# (the package names above without -dev / -devel). PipeWire's headers are
# only for building: the library itself is opened when sharing on Wayland,
# so the app still runs where it isn't installed. FFmpeg is built in
# (ffmpeg/build-ffmpeg-linux-portable.sh makes ffmpeg/linux/ffmpeg.gz first).
#
# On their computer, optionally:
#   xdg-desktop-portal(-gnome/-kde…)  to share on Wayland (desktops include it)
#   xrandr                     on X11, to share one monitor at a time (else the whole X screen)
#   parec or pw-record         to share the computer's sound with a screen
#                              (pulseaudio-utils / pipewire — usually there already)
#   a desktop keyring          (gnome-keyring, KWallet…) for "Keep me signed in";
#                              without one it still works, with the key in a private file
#
# -tags nolibopusfile: chriscord only encodes and decodes Opus packets, it
# never opens .opus files, so it needs neither opusfile nor libogg.
# WebKitGTK 4.1 (libsoup 3) is what current distros ship; 4.0 is used only
# when 4.1 isn't installed.
set -eu

for tool in gcc pkg-config go wails; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing: $tool (see the top of this file)" >&2; exit 1; }
done

TAGS=nolibopusfile
if pkg-config --exists webkit2gtk-4.1; then
  TAGS="$TAGS,webkit2_41"
  echo "== WebKitGTK 4.1"
elif pkg-config --exists webkit2gtk-4.0; then
  echo "== WebKitGTK 4.0"
else
  echo "missing library: webkit2gtk-4.1 (or 4.0) — install the packages at the top of this file" >&2; exit 1
fi
for pc in gtk+-3.0 portaudio-2.0 opus libpipewire-0.3; do
  pkg-config --exists "$pc" || { echo "missing library: $pc — install the packages at the top of this file" >&2; exit 1; }
done

# The bundled FFmpeg: built here, with this machine's tools, when there
# isn't one yet or the one there is older than what the app needs (no raw
# video reader — Wayland sharing — or JPEG writer — showing video). REBUILD_FFMPEG=1 builds it again anyway.
# Fine for testing; for builds you hand out, ffmpeg/build-ffmpeg-linux-portable.sh
# makes one that also runs on older distros.
ffmpeg_ok() {
  [ -f ffmpeg/linux/ffmpeg.gz ] || return 1
  t=$(mktemp)
  gzip -dc ffmpeg/linux/ffmpeg.gz > "$t" && chmod +x "$t"
  ok=1
  "$t" -hide_banner -demuxers 2>/dev/null | grep -q ' rawvideo ' || ok=0
  "$t" -hide_banner -encoders 2>/dev/null | grep -q ' libx264 ' || ok=0
  "$t" -hide_banner -encoders 2>/dev/null | grep -q ' mjpeg ' || ok=0   # showing video (videodec.go)
  "$t" -hide_banner -encoders 2>/dev/null | grep -q ' libvpx ' || ok=0  # playing chat videos (localmedia.go)
  rm -f "$t"
  [ "$ok" = 1 ]
}
if [ "${REBUILD_FFMPEG:-0}" = 1 ] || ! ffmpeg_ok; then
  echo "== FFmpeg: building it (a few minutes the first time; later runs reuse ffmpeg/.build-linux)"
  for tool in nasm curl make; do
    command -v "$tool" >/dev/null 2>&1 || { echo "missing: $tool — needed to build FFmpeg (Ubuntu: sudo apt install nasm curl; Fedora: sudo dnf install nasm curl)" >&2; exit 1; }
  done
  for pc in xcb xcb-shm xcb-xfixes xcb-shape; do
    pkg-config --exists "$pc" || { echo "missing library: $pc — Ubuntu: sudo apt install libxcb1-dev libxcb-shm0-dev libxcb-xfixes0-dev libxcb-shape0-dev; Fedora: sudo dnf install libxcb-devel" >&2; exit 1; }
  done
  sh ffmpeg/build-ffmpeg.sh linux
fi
echo "== FFmpeg: built in ($(du -h ffmpeg/linux/ffmpeg.gz | cut -f1))"

wails build -platform linux/amd64 -tags "$TAGS" "$@"

BIN=build/bin/chriscord
echo
echo "== libraries $BIN needs:"
ldd "$BIN" | grep -E 'gtk|webkit|portaudio|opus|not found' | sed 's/^/   /'
if ldd "$BIN" | grep -q 'not found'; then
  echo "!! Some aren't installed here." >&2
fi
echo
echo "Done: $BIN ($(du -h "$BIN" | cut -f1))."
echo "It adds itself to the app menu (and its icon to the dock) when it starts: desktop_linux.go."

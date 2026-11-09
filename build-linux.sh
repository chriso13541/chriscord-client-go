#!/bin/sh
# Builds chriscord for Linux (x86-64): build/bin/chriscord.
#
# Run it from the repo folder:
#   sh build-linux.sh
#
# Needs (build machine only):
#   Debian/Ubuntu:  sudo apt install build-essential pkg-config libgtk-3-dev \
#                     libwebkit2gtk-4.1-dev portaudio19-dev libopus-dev libpipewire-0.3-dev
#                   (older releases: libwebkit2gtk-4.0-dev instead of 4.1)
#   Arch:           sudo pacman -S --needed base-devel gtk3 webkit2gtk-4.1 portaudio opus pipewire
#   Fedora:         sudo dnf install gcc pkgconf gtk3-devel webkit2gtk4.1-devel portaudio-devel opus-devel pipewire-devel
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

if [ -f ffmpeg/linux/ffmpeg.gz ]; then
  echo "== FFmpeg: built in ($(du -h ffmpeg/linux/ffmpeg.gz | cut -f1))"
else
  echo "== FFmpeg: not built in — the app will use ffmpeg from the PATH (sh ffmpeg/build-ffmpeg-linux-portable.sh to build it in)"
fi

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

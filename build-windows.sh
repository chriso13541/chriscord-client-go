#!/bin/sh
# Builds chriscord.exe for Windows as ONE self-contained file: PortAudio,
# Opus and the MinGW runtime (libgcc, winpthread) are linked into the exe
# statically, so whoever runs it needs nothing installed — no MSYS2, no DLLs
# next to it. (FFmpeg is already embedded; see ffmpeg/build-ffmpeg.sh.)
#
# Run it in the "MSYS2 MINGW64" shell, from the repo folder:
#   sh build-windows.sh
#
# Needs (build machine only — not the people you send it to):
#   pacman -S --needed mingw-w64-x86_64-gcc mingw-w64-x86_64-pkgconf \
#                      mingw-w64-x86_64-portaudio mingw-w64-x86_64-opus
#   plus Go and the wails CLI on the PATH.
#
# How it works:
#   * The Go bindings ask pkg-config how to link PortAudio and Opus. Normally
#     that names the DLLs; with --static it also lists everything those
#     libraries need from Windows (winmm, ole32, setupapi…). Go can't be told
#     to add --static, so a tiny wrapper that does is put first in the PATH
#     and handed to Go as PKG_CONFIG.
#   * -extldflags=-static makes the linker take the .a (static) versions of
#     every library instead of the .dll.a ones.
#   * -tags nolibopusfile leaves out the Opus *file* reader (opusfile, and the
#     libogg it needs): chriscord only encodes and decodes Opus packets, it
#     never opens .opus files, so it needs neither.
#
# At the end it lists the DLLs the exe still loads: they should all be
# Windows' own (in C:\Windows). Anything from mingw64 means something wasn't
# linked statically.
set -eu

case "${MSYSTEM:-}" in
  MINGW64) ;;
  *) echo "Run this in the MSYS2 MINGW64 shell (MSYSTEM is '${MSYSTEM:-unset}')." >&2; exit 1 ;;
esac
for tool in gcc pkg-config go wails cygpath; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing: $tool (see the top of this file)" >&2; exit 1; }
done
for pc in portaudio-2.0 opus; do
  pkg-config --exists "$pc" || { echo "missing library: $pc — pacman -S the packages at the top of this file" >&2; exit 1; }
done

# The pkg-config wrapper: a .bat, because Go (a Windows program) starts it
# directly, and Windows can't run a shell script.
WRAP_DIR=$(mktemp -d)
trap 'rm -rf "$WRAP_DIR"' EXIT
REAL_PKGCONFIG=$(cygpath -w "$(command -v pkg-config)")
printf '@"%s" --static %%*\r\n' "$REAL_PKGCONFIG" > "$WRAP_DIR/pkg-config-static.bat"
export PATH="$WRAP_DIR:$PATH"
export PKG_CONFIG=pkg-config-static
export CGO_ENABLED=1

echo "== linking against (static):"
echo "   portaudio: $(pkg-config --static --libs portaudio-2.0)"
echo "   opus:      $(pkg-config --static --libs opus)"

wails build -clean -platform windows/amd64 -tags nolibopusfile -ldflags "-extldflags=-static" "$@"

EXE=build/bin/chriscord.exe
echo
echo "== DLLs $EXE still loads (should all be in /c/Windows):"
ldd "$EXE" | sed 's/^/   /'
if ldd "$EXE" | grep -qi '/mingw64/'; then
  echo
  echo "!! Some come from mingw64 — those would be missing on other computers." >&2
  exit 1
fi
echo
echo "Done: $EXE ($(du -h "$EXE" | cut -f1)) — send just this file."

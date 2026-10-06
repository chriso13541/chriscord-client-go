#!/bin/sh
# Builds the small FFmpeg that ships inside chriscord, for converting
# videos so they play in the app (see ../transcode.go).
#
#   ffmpeg/build-ffmpeg.sh windows    # → ffmpeg/windows/ffmpeg.exe.gz   (run on Linux / WSL)
#   ffmpeg/build-ffmpeg.sh linux      # → ffmpeg/linux/ffmpeg.gz
#
# Then build the app as usual (wails build); the matching file is embedded.
# Without one, the app falls back to an ffmpeg on the PATH, and without
# that, videos are just sent as they are.
#
# It's FFmpeg with only what chriscord needs: the common video/audio
# decoders (H.264, HEVC, VP8/9, MPEG-2/4, VC-1; AAC, AC-3, E-AC-3, DTS,
# TrueHD, FLAC, Opus, Vorbis, MP3…), the MKV/MP4/AVI/TS/etc. readers, MP4
# and WebM writers, and H.264 encoders: x264 (software) plus NVIDIA NVENC
# and, on Windows, Media Foundation (Intel/AMD/NVIDIA hardware). Plus
# graphics-card DECODING, so a whole movie can be converted without the
# CPU doing the decode: NVIDIA NVDEC everywhere, and on Windows D3D11VA
# (any Intel/AMD/NVIDIA card) — H.264, HEVC, VP9, AV1, MPEG-2 and VC-1.
# AV1 files can only be decoded on a graphics card that supports AV1
# (RTX 30-series and newer, RX 6000+, Intel Arc/11th gen+). About
# 12–17 MB instead of ~165 MB for a full build.
#
# It also captures, for chriscord's native webcam and screen sharing
# (../capture.go): webcams through DirectShow (Windows) / V4L2 (Linux), and
# screens through Windows Graphics Capture (gfxcapture: a monitor or a
# single window, kept on the graphics card start to finish — scaled and
# converted there with scale_d3d11 and fed straight to NVENC / Media
# Foundation) with Desktop Duplication (ddagrab) as a fallback, or X11
# (xcbgrab) on Linux. Output is raw H.264 on a pipe.
#
# Needs: a C and C++ compiler, make, nasm, pkg-config, curl.
#   Windows build from WSL/Linux (cross-compiles):
#     sudo apt install build-essential nasm pkg-config curl mingw-w64
#   Windows build from MSYS2 (in the "MSYS2 UCRT64" shell) — use this if the
#   WSL build reports gfxcapture as missing (WSL's mingw-w64 may be too old
#   for the Windows Graphics Capture headers; MSYS2's is always current):
#     pacman -S --needed make curl mingw-w64-ucrt-x86_64-toolchain mingw-w64-ucrt-x86_64-nasm mingw-w64-ucrt-x86_64-pkgconf mingw-w64-ucrt-x86_64-cmake
#   (cmake builds Intel's libvpl here, statically, for Intel Quick Sync —
#   h264_qsv. MSYS2's own libvpl package is a DLL only, which a static
#   ffmpeg.exe can't use. Without cmake, Quick Sync is simply left out.)
#   Linux build:
#     sudo apt install build-essential nasm pkg-config curl libxcb1-dev libxcb-shm0-dev libxcb-xfixes0-dev libxcb-shape0-dev
# From WSL, building inside /mnt/c is slow; point WORK at the Linux side:
#   WORK=~/ffmpeg-build-windows sh ffmpeg/build-ffmpeg.sh windows
# After changing any version below, delete the WORK folder first: sources
# and installed headers are reused if they're already there.
#
# Licence: with x264 the result is GPL-licensed FFmpeg. Shipping it next to
# chriscord is fine (it's a separate program chriscord runs), as long as its
# licence and source go with it — this script records both in
# ffmpeg/<os>/FFMPEG-LICENSE.txt and FFMPEG-SOURCE.txt.
set -eu

TARGET=${1:-}
case "$TARGET" in windows|linux) ;; *) echo "usage: $0 windows|linux" >&2; exit 2 ;; esac

FFMPEG_VER=n9.0.2        # FFmpeg release tag
X264_REF=stable          # x264 branch
VPL_REF=v2.17.0          # Intel libvpl (Quick Sync) tag, Windows only
NVHDR_REF=n12.1.14.0     # nv-codec-headers (NVENC) tag. This sets the OLDEST NVIDIA driver
                         # NVENC will work with: 12.1 needs 531.61+ (Windows) / 530.41+ (Linux);
                         # 13.0 would need 570+. FFmpeg 8 still accepts 12.1.

HERE=$(cd "$(dirname "$0")" && pwd)
WORK=${WORK:-$HERE/.build-$TARGET}
PREFIX=$WORK/prefix
OUT=$HERE/$TARGET
JOBS=$(nproc 2>/dev/null || echo 4)
mkdir -p "$WORK" "$PREFIX" "$OUT"
cd "$WORK"

get() { # url dir
  [ -d "$2" ] || { mkdir -p "$2"; curl -fsSL "$1" | tar xz --strip-components=1 -C "$2"; }
}
get "https://codeload.github.com/FFmpeg/FFmpeg/tar.gz/refs/tags/$FFMPEG_VER" ffmpeg-src
get "https://codeload.github.com/mirror/x264/tar.gz/refs/heads/$X264_REF" x264-src
get "https://codeload.github.com/FFmpeg/nv-codec-headers/tar.gz/refs/tags/$NVHDR_REF" nvhdr-src

case "$(uname -s)" in MINGW*|MSYS*|UCRT*|CLANG*) NATIVE_WIN=1 ;; *) NATIVE_WIN=0 ;; esac
if [ "$TARGET" = windows ] && [ "$NATIVE_WIN" = 1 ]; then
  # MSYS2: building on Windows for Windows — no cross-compiling.
  CROSS=""
  X264_HOST=""
  FF_TARGET=""
elif [ "$TARGET" = windows ]; then
  CROSS=x86_64-w64-mingw32-
  X264_HOST="--host=x86_64-w64-mingw32 --cross-prefix=$CROSS"
  FF_TARGET="--arch=x86_64 --target-os=mingw32 --cross-prefix=$CROSS"
fi
if [ "$TARGET" = windows ]; then
  HW_ENC="h264_nvenc,h264_mf"
  EXE=ffmpeg.exe
  EXTRA_LIBS="-static -static-libgcc -static-libstdc++"
  # Capture: webcams (DirectShow), screens/windows (Windows Graphics
  # Capture; Desktop Duplication for border-free screens), GPU
  # scaling/conversion. Border-free windows on Windows 10 are copied by
  # chriscord itself (screen_gdi_windows.c) and handed over as raw frames:
  # the rawvideo reader.
  CAPTURE="--enable-indev=dshow --enable-demuxer=rawvideo --enable-filter=gfxcapture,ddagrab,scale_d3d11,hwupload,hwmap"
  # Intel Quick Sync (always the Intel chip, unlike Media Foundation):
  # libvpl, built below as a static library, when cmake is available.
  command -v cmake >/dev/null 2>&1 && WANT_VPL=1 || WANT_VPL=0
else
  X264_HOST=""
  FF_TARGET=""
  HW_ENC="h264_nvenc"
  EXE=ffmpeg
  EXTRA_LIBS=""
  # Capture: webcams (V4L2), X11 screens (also XWayland windows).
  CAPTURE="--enable-indev=v4l2,xcbgrab --enable-libxcb --enable-libxcb-shm --enable-libxcb-xfixes --enable-libxcb-shape --enable-filter=hwupload"
fi

echo "== nv-codec-headers"
make -C nvhdr-src PREFIX="$PREFIX" install >/dev/null

echo "== x264"
(cd x264-src && ./configure --prefix="$PREFIX" $X264_HOST --enable-static --enable-pic --disable-cli --disable-opencl >/dev/null && make -j"$JOBS" >/dev/null && make install >/dev/null)

if [ "${WANT_VPL:-0}" = 1 ]; then
  echo "== libvpl (Intel Quick Sync)"
  get "https://codeload.github.com/intel/libvpl/tar.gz/refs/tags/$VPL_REF" libvpl-src
  # libvpl's "#if _MSC_VER < 1400" (meant for ancient Visual Studio) is also
  # true for GCC, where _MSC_VER isn't defined, and its wcscpy_s macro then
  # breaks current MinGW headers. MinGW has the real wcscpy_s: skip it.
  sed -i 's/^#if _MSC_VER < 1400$/#if defined(_MSC_VER) \&\& _MSC_VER < 1400/' libvpl-src/libvpl/src/windows/mfx_dispatcher_defs.h
  if [ -n "${CROSS:-}" ]; then VPL_CROSS="-DCMAKE_SYSTEM_NAME=Windows -DCMAKE_C_COMPILER=${CROSS}gcc -DCMAKE_CXX_COMPILER=${CROSS}g++ -DCMAKE_RC_COMPILER=${CROSS}windres"; else VPL_CROSS=""; fi
  if cmake -S libvpl-src -B libvpl-build -G "Unix Makefiles" $VPL_CROSS \
       -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX="$PREFIX" -DCMAKE_INSTALL_LIBDIR=lib \
       -DBUILD_SHARED_LIBS=OFF -DBUILD_TESTS=OFF -DBUILD_EXAMPLES=OFF -DINSTALL_EXAMPLES=OFF \
       -DBUILD_EXPERIMENTAL=OFF >"$WORK/libvpl.log" 2>&1 \
     && cmake --build libvpl-build -j"$JOBS" >>"$WORK/libvpl.log" 2>&1 \
     && cmake --install libvpl-build >>"$WORK/libvpl.log" 2>&1; then
    HW_ENC="$HW_ENC,h264_qsv"
    # libvpl's dispatcher is C++: link the C++ runtime (statically, see EXTRA_LIBS).
    CAPTURE="$CAPTURE --enable-libvpl --extra-libs=-lstdc++"
  else
    echo "  libvpl didn't build (see $WORK/libvpl.log) — leaving Intel Quick Sync out"
  fi
fi

echo "== ffmpeg"
cd ffmpeg-src
if [ "$TARGET" = windows ]; then
  # chriscord's additions to gfxcapture (see patches/gfxcapture_borderless.h):
  # really hiding the yellow border on Windows 11, and following whether the
  # mouse pointer is showing. Each step is skipped if already done.
  cp "$HERE/patches/gfxcapture_borderless.h" libavfilter/
  F=libavfilter/vsrc_gfxcapture_winrt.cpp
  grep -q '"gfxcapture_borderless.h"' "$F" || \
    sed -i 's|^#include <cinttypes>$|#include "gfxcapture_borderless.h"\n&|' "$F"
  grep -q 'cc_request_borderless(avctx' "$F" || \
    sed -i 's|^\( *\)if (SUCCEEDED(wgctx->capture_session.As(&session3))) {$|\1if (!cctx->display_border)\n\1    cc_request_borderless(avctx, ctx);\n&|' "$F"
  grep -q 'cc_follow_cursor(avctx' "$F" || \
    sed -i 's|^\( *\)CHECK_HR_RET(wgctx->frame_pool->TryGetNextFrame(&capture_frame));$|\1if (cctx->capture_cursor)\n\1    cc_follow_cursor(avctx, wgctx->capture_session);\n&|' "$F"
  for want in '"gfxcapture_borderless.h"' 'cc_request_borderless(avctx' 'cc_follow_cursor(avctx'; do
    grep -q "$want" "$F" || { echo "couldn't patch $F ($want) — FFmpeg changed?" >&2; exit 1; }
  done
fi
PKG_CONFIG_PATH="$PREFIX/lib/pkgconfig" ./configure $FF_TARGET \
  --prefix="$PREFIX" --pkg-config=pkg-config --pkg-config-flags=--static \
  --extra-cflags="-I$PREFIX/include" --extra-ldflags="-L$PREFIX/lib $EXTRA_LIBS" \
  --enable-gpl --enable-libx264 --enable-ffnvcodec --enable-nvenc \
  --enable-cuda --enable-nvdec --enable-cuvid \
  --enable-hwaccel=h264_nvdec,hevc_nvdec,vp9_nvdec,av1_nvdec,mpeg2_nvdec,vc1_nvdec \
  --disable-everything --disable-autodetect --disable-doc --disable-debug --disable-network \
  --disable-ffplay --disable-ffprobe --enable-small \
  $CAPTURE \
  $( [ "$TARGET" = windows ] && echo --enable-mediafoundation --enable-d3d11va \
       --enable-hwaccel=h264_d3d11va,h264_d3d11va2,hevc_d3d11va,hevc_d3d11va2,vp9_d3d11va,vp9_d3d11va2,av1_d3d11va,av1_d3d11va2,mpeg2_d3d11va,mpeg2_d3d11va2,vc1_d3d11va,vc1_d3d11va2 ) \
  --enable-protocol=file,pipe \
  --enable-demuxer=matroska,mov,avi,mpegts,mpegps,flv,asf,ogg,wav,mp3,aac,ac3,eac3,dts,m4v,h264,hevc,ivf,obu \
  --enable-muxer=mp4,webm,null,h264,flv \
  --enable-decoder=wrapped_avframe,rawvideo,h264,hevc,av1,mpeg1video,mpeg2video,mpeg4,msmpeg4v3,vc1,wmv3,vp8,vp9,mjpeg,theora,aac,aac_latm,ac3,eac3,dca,truehd,mlp,mp1,mp2,mp3,flac,alac,opus,vorbis,wmav2,pcm_s16le,pcm_s24le,pcm_s32le,pcm_f32le,pcm_s16be \
  --enable-encoder=libx264,$HW_ENC,aac \
  --enable-parser=h264,hevc,mpegvideo,mpeg4video,vc1,vp8,vp9,av1,aac,ac3,mpegaudio,dca,flac,opus,vorbis,mlp \
  --enable-bsf=h264_mp4toannexb,hevc_mp4toannexb,aac_adtstoasc,vp9_superframe,extract_extradata \
  --enable-indev=lavfi --enable-filter=scale,format,aformat,aresample,null,anull,color,anullsrc,setsar,fps,hwdownload \
  --enable-swscale --enable-swresample >"$WORK/configure.log" 2>&1 || {
    echo "FFmpeg's configure failed. Last lines of $WORK/configure.log"
    echo "(full details: $WORK/ffmpeg-src/ffbuild/config.log):"; tail -15 "$WORK/configure.log"; exit 1; }
make -j"$JOBS" >/dev/null
# What chriscord's capture will be able to use.
have() { grep -q "^$1=yes" ffbuild/config.mak && echo yes || echo NO; }
echo
echo "== capture support in this build"
if [ "$TARGET" = windows ]; then
  echo "  webcams (dshow):                         $(have CONFIG_DSHOW_INDEV)"
  echo "  screens/windows on the GPU (gfxcapture): $(have CONFIG_GFXCAPTURE_FILTER)"
  echo "  screens without a border (ddagrab):      $(have CONFIG_DDAGRAB_FILTER)"
  echo "  windows without a border, Win10 (rawvideo): $(have CONFIG_RAWVIDEO_DEMUXER)"
  echo "  GPU scaling (scale_d3d11):               $(have CONFIG_SCALE_D3D11_FILTER)"
  echo "  NVIDIA / Media Foundation encoders:      $(have CONFIG_H264_NVENC_ENCODER) / $(have CONFIG_H264_MF_ENCODER)"
  echo "  Intel Quick Sync (h264_qsv):             $(have CONFIG_H264_QSV_ENCODER)"
  if ! grep -q "^CONFIG_GFXCAPTURE_FILTER=yes" ffbuild/config.mak; then
    echo
    echo "  NOTE: gfxcapture is missing — this compiler's Windows headers are too old"
    echo "  for Windows Graphics Capture. Screen sharing will fall back to whole-monitor"
    echo "  ddagrab. For window capture, build under MSYS2 instead (see the top of this file)."
  fi
else
  echo "  webcams (v4l2):          $(have CONFIG_V4L2_INDEV)"
  echo "  X11 screens (xcbgrab):   $(have CONFIG_XCBGRAB_INDEV)"
  echo "  NVIDIA encoder:          $(have CONFIG_H264_NVENC_ENCODER)"
fi
cd ..

${CROSS:-}strip -o "$WORK/$EXE" "ffmpeg-src/$EXE"
gzip -9 -c "$WORK/$EXE" > "$OUT/$EXE.gz"
cp ffmpeg-src/COPYING.GPLv3 "$OUT/FFMPEG-LICENSE.txt" 2>/dev/null || cp ffmpeg-src/COPYING.GPLv2 "$OUT/FFMPEG-LICENSE.txt"
cat > "$OUT/FFMPEG-SOURCE.txt" <<SRC
This FFmpeg ($EXE, gzipped) was built by ffmpeg/build-ffmpeg.sh from:
  FFmpeg           $FFMPEG_VER   https://github.com/FFmpeg/FFmpeg/tree/$FFMPEG_VER
  x264             $X264_REF     https://code.videolan.org/videolan/x264 (mirror: https://github.com/mirror/x264)
  nv-codec-headers $NVHDR_REF    https://github.com/FFmpeg/nv-codec-headers/tree/$NVHDR_REF
It is licensed under the GNU GPL (see FFMPEG-LICENSE.txt). The exact build
recipe is build-ffmpeg.sh in this directory.
SRC
echo
echo "Built $OUT/$EXE.gz ($(du -h "$OUT/$EXE.gz" | cut -f1) compressed, $(du -h "$WORK/$EXE" | cut -f1) unpacked)"

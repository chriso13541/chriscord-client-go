package main

// Pictures handed to the page (profile pictures, banners, server icons,
// theme backgrounds) are made no bigger than they're ever shown. A
// profile picture uploaded straight from a phone or camera can be a
// 4000×4000 PNG of several MB: as a data URL that's ~9 MB of text pushed
// through the window's main thread, and every avatar on screen decodes it
// into a 64 MB bitmap — enough, with a few people like that, to freeze the
// window or run a small machine (a VM) out of memory. Shrunk once here (and
// kept — in memory, and in the cache folder so the next start doesn't
// decode it again), it's a few tens of KB.
//
// Animated pictures (GIF, WebP, animated PNG) and anything Go can't read
// are passed on as they are, as are pictures already small enough.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"sync"

	_ "image/gif"
)

const (
	pfpMaxDim        = 512      // avatars, server icons (shown at most ~128 px, sharp on HiDPI)
	bannerMaxDim     = 1600     // profile and server banners
	backgroundMaxDim = 2560     // theme backgrounds (the whole window)
	pickedMaxDim     = 2048     // a picture just picked, for cropping
	shrinkMinBytes   = 16 << 10 // smaller: too small to be worth looking at (the size, not the bytes, is what costs)
)

var shrinkCache = struct {
	sync.Mutex
	m map[[32]byte][]byte
}{m: map[[32]byte][]byte{}}

// sizedDataURL: data as a data: URL, shrunk to fit maxDim×maxDim first if
// it's a big still PNG or JPEG.
func sizedDataURL(data []byte, maxDim int) string {
	return pfpDataURL(shrinkImage(data, maxDim))
}

func shrinkImage(data []byte, maxDim int) []byte {
	if len(data) < shrinkMinBytes {
		return data
	}
	mime := pfpMime(data)
	if mime != "image/png" && mime != "image/jpeg" {
		return data
	}
	if mime == "image/png" && bytes.Contains(data[:min(len(data), 4096)], []byte("acTL")) {
		return data // animated PNG
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (cfg.Width <= maxDim && cfg.Height <= maxDim) {
		return data
	}
	key := sha256.Sum256(append(data[:0:0], data...))
	key[0] ^= byte(maxDim) // a different size, a different entry
	key[1] ^= byte(maxDim >> 8)
	shrinkCache.Lock()
	if out, ok := shrinkCache.m[key]; ok {
		shrinkCache.Unlock()
		return out
	}
	shrinkCache.Unlock()
	diskPath := shrinkDiskPath(key)
	if out, err := os.ReadFile(diskPath); err == nil && len(out) > 0 {
		shrinkRemember(key, out)
		return out
	}

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w >= h {
		w, h = maxDim, max(1, h*maxDim/w)
	} else {
		w, h = max(1, w*maxDim/h), maxDim
	}
	rgba := image.NewRGBA(b)
	draw.Draw(rgba, b, src, b.Min, draw.Src)
	dst := shrinkRGBA(rgba, w, h)

	var buf bytes.Buffer
	if mime == "image/jpeg" || opaque(dst) {
		err = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 88})
	} else {
		err = (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&buf, dst)
	}
	if err != nil || buf.Len() >= len(data) {
		return data
	}
	out := buf.Bytes()
	shrinkRemember(key, out)
	if os.MkdirAll(filepath.Dir(diskPath), 0700) == nil {
		writeFileAtomic(diskPath, out, 0600)
	}
	return out
}

func shrinkRemember(key [32]byte, out []byte) {
	shrinkCache.Lock()
	if len(shrinkCache.m) > 256 {
		shrinkCache.m = map[[32]byte][]byte{}
	}
	shrinkCache.m[key] = out
	shrinkCache.Unlock()
}

// shrinkDiskPath: where a shrunk copy is kept between starts (the cache
// folder: safe to delete). Named by the original's hash and size, so a
// changed picture gets a new file.
func shrinkDiskPath(key [32]byte) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "chriscord", "pictures", hex.EncodeToString(key[:16]))
}

// shrinkRGBA: src (premultiplied RGBA) at w×h, each pixel the average of
// the block it covers.
func shrinkRGBA(src *image.RGBA, w, h int) *image.RGBA {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		y0, y1 := y*sh/h, (y+1)*sh/h
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < w; x++ {
			x0, x1 := x*sw/w, (x+1)*sw/w
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, a, n uint32
			for yy := y0; yy < y1; yy++ {
				row := src.Pix[(yy)*src.Stride:]
				for xx := x0; xx < x1; xx++ {
					p := row[xx*4:]
					r += uint32(p[0])
					g += uint32(p[1])
					bl += uint32(p[2])
					a += uint32(p[3])
					n++
				}
			}
			o := dst.PixOffset(x, y)
			dst.Pix[o], dst.Pix[o+1], dst.Pix[o+2], dst.Pix[o+3] = uint8(r/n), uint8(g/n), uint8(bl/n), uint8(a/n)
		}
	}
	return dst
}

func opaque(img *image.RGBA) bool {
	for i := 3; i < len(img.Pix); i += 4 {
		if img.Pix[i] != 255 {
			return false
		}
	}
	return true
}

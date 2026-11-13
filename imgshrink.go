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
// Animated GIFs are shrunk too (shrinkGIF), to half that size at most: a
// big animated avatar — several MB, hundreds of frames — is redrawn every
// frame in every place it's shown (the member list, each message), and on
// a slow computer that alone can make a whole server crawl. Animated WebP
// and PNG and anything Go can't read are passed on as they are, as are
// pictures already small enough.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"sync"
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
	if mime == "image/gif" {
		maxDim = min(maxDim, max(256, maxDim/2)) // animated: costs per frame
	} else if mime != "image/png" && mime != "image/jpeg" {
		return data
	}
	if mime == "image/png" && bytes.Contains(data[:min(len(data), 4096)], []byte("acTL")) {
		return data // animated PNG
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (cfg.Width <= maxDim && cfg.Height <= maxDim) || cfg.Width*cfg.Height > 4096*4096 {
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

	var out []byte
	if mime == "image/gif" {
		out = shrinkGIF(data, maxDim)
	} else {
		out = shrinkStill(data, mime, maxDim)
	}
	if out == nil || len(out) >= len(data) {
		return data
	}
	shrinkRemember(key, out)
	if os.MkdirAll(filepath.Dir(diskPath), 0700) == nil {
		writeFileAtomic(diskPath, out, 0600)
	}
	return out
}

func fitIn(w, h, maxDim int) (int, int) {
	if w >= h {
		return maxDim, max(1, h*maxDim/w)
	}
	return max(1, w*maxDim/h), maxDim
}

// shrinkStill: a still PNG or JPEG at maxDim at most (nil: couldn't).
func shrinkStill(data []byte, mime string, maxDim int) []byte {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	b := src.Bounds()
	w, h := fitIn(b.Dx(), b.Dy(), maxDim)
	rgba := image.NewRGBA(b)
	draw.Draw(rgba, b, src, b.Min, draw.Src)
	dst := shrinkRGBA(rgba, w, h)

	var buf bytes.Buffer
	if mime == "image/jpeg" || opaque(dst) {
		err = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 88})
	} else {
		err = (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&buf, dst)
	}
	if err != nil {
		return nil
	}
	return buf.Bytes()
}

// shrinkGIF: a GIF (animated or not) at maxDim at most (nil: couldn't).
// Each frame is put together as it would be shown (GIFs draw each frame
// over the last), shrunk, and stored whole. Frames faster than 25 a
// second are merged: no one sees them on an avatar, and each costs.
func shrinkGIF(data []byte, maxDim int) []byte {
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil || len(g.Image) == 0 {
		return nil
	}
	W, H := g.Config.Width, g.Config.Height
	if W <= 0 || H <= 0 {
		b := g.Image[0].Bounds()
		W, H = b.Max.X, b.Max.Y
	}
	w, h := fitIn(W, H, maxDim)
	canvas := image.NewRGBA(image.Rect(0, 0, W, H))
	var saved *image.RGBA
	out := &gif.GIF{LoopCount: g.LoopCount, Config: image.Config{Width: w, Height: h}}
	wait := 0 // hundredths of a second not yet given to an output frame
	for i, fr := range g.Image {
		disposal := byte(0)
		if i < len(g.Disposal) {
			disposal = g.Disposal[i]
		}
		delay := 10 // browsers show a frame of 0 or 1 for a tenth of a second
		if i < len(g.Delay) && g.Delay[i] >= 2 {
			delay = g.Delay[i]
		}
		if disposal == gif.DisposalPrevious {
			saved = image.NewRGBA(canvas.Rect)
			copy(saved.Pix, canvas.Pix)
		}
		fb := fr.Bounds().Intersect(canvas.Rect)
		draw.Draw(canvas, fb, fr, fb.Min, draw.Over)

		wait += delay
		if wait >= 4 || i == len(g.Image)-1 {
			out.Image = append(out.Image, toPaletted(shrinkRGBA(canvas, w, h), fr.Palette))
			out.Delay = append(out.Delay, wait)
			out.Disposal = append(out.Disposal, gif.DisposalBackground) // whole frames: no leftovers
			wait = 0
		}

		switch disposal {
		case gif.DisposalBackground:
			draw.Draw(canvas, fb, image.Transparent, image.Point{}, draw.Src)
		case gif.DisposalPrevious:
			if saved != nil {
				copy(canvas.Pix, saved.Pix)
			}
		}
	}
	var buf bytes.Buffer
	if gif.EncodeAll(&buf, out) != nil {
		return nil
	}
	return buf.Bytes()
}

// toPaletted: img (premultiplied) in the colours of pal, plus see-through
// where img mostly is.
func toPaletted(img *image.RGBA, pal color.Palette) *image.Paletted {
	p := make(color.Palette, 0, 256)
	trans := -1
	for _, c := range pal {
		if _, _, _, a := c.RGBA(); a == 0 && trans < 0 {
			trans = len(p)
		}
		p = append(p, c)
	}
	if len(p) == 0 {
		p = append(p, color.Black, color.White)
	}
	if trans < 0 && len(p) < 256 {
		trans = len(p)
		p = append(p, color.RGBA{})
	}
	type rgb struct{ r, g, b int32 }
	cols := make([]rgb, len(p))
	for i, c := range p {
		n := color.NRGBAModel.Convert(c).(color.NRGBA)
		cols[i] = rgb{int32(n.R), int32(n.G), int32(n.B)}
	}
	var memo [1 << 15]int16 // nearest colour, by 5-bit r, g, b
	for i := range memo {
		memo[i] = -1
	}
	dst := image.NewPaletted(img.Rect, p)
	for i, j := 0, 0; i < len(img.Pix); i, j = i+4, j+1 {
		a := int32(img.Pix[i+3])
		if a < 128 && trans >= 0 {
			dst.Pix[j] = uint8(trans)
			continue
		}
		if a == 0 {
			a = 1
		}
		r := min(255, int32(img.Pix[i])*255/a)
		g := min(255, int32(img.Pix[i+1])*255/a)
		b := min(255, int32(img.Pix[i+2])*255/a)
		k := r>>3<<10 | g>>3<<5 | b>>3
		if memo[k] < 0 {
			best, bestD := 0, int32(1<<30)
			for n, c := range cols {
				if n == trans {
					continue
				}
				dr, dg, db := c.r-r, c.g-g, c.b-b
				if d := 3*dr*dr + 4*dg*dg + 2*db*db; d < bestD {
					best, bestD = n, d
				}
			}
			memo[k] = int16(best)
		}
		dst.Pix[j] = uint8(memo[k])
	}
	return dst
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

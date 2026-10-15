//go:build windows

package main

// A program's icon, as a PNG data URL — for the games list in Settings.
// The first icon in the .exe, drawn into a 32×32 bitmap with its alpha.

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

var (
	pDrawIconEx        = user32.NewProc("DrawIconEx")
	pDestroyIcon       = user32.NewProc("DestroyIcon")
	pCreateDIBSection  = gdi32.NewProc("CreateDIBSection")
	iconCache          sync.Map // lowercase path → data URL ("" for none)
	iconExtractIconExW = syscall.NewLazyDLL("shell32.dll").NewProc("ExtractIconExW")
)

type iconBitmapInfo struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

func programIcon(path string) string {
	key := strings.ToLower(path)
	if v, ok := iconCache.Load(key); ok {
		return v.(string)
	}
	url := extractIcon(path)
	iconCache.Store(key, url)
	return url
}

func extractIcon(path string) string {
	const size = 32
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return ""
	}
	var large uintptr
	if n, _, _ := iconExtractIconExW.Call(uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(&large)), 0, 1); n == 0 || large == 0 {
		return ""
	}
	defer pDestroyIcon.Call(large)

	dc, _, _ := pCreateCompatibleDC.Call(0)
	if dc == 0 {
		return ""
	}
	defer pDeleteDC.Call(dc)
	bi := iconBitmapInfo{Width: size, Height: -size, Planes: 1, BitCount: 32} // top-down
	bi.Size = uint32(unsafe.Sizeof(bi))
	var bits uintptr
	bmp, _, _ := pCreateDIBSection.Call(dc, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if bmp == 0 || bits == 0 {
		return ""
	}
	defer pDeleteObject.Call(bmp)
	old, _, _ := pSelectObject.Call(dc, bmp)
	const diNormal = 3
	r, _, _ := pDrawIconEx.Call(dc, 0, 0, large, size, size, 0, 0, diNormal)
	pSelectObject.Call(dc, old)
	if r == 0 {
		return ""
	}
	px := unsafe.Slice((*byte)(unsafe.Pointer(bits)), size*size*4) // BGRA, premultiplied
	anyAlpha := false
	for i := 3; i < len(px); i += 4 {
		if px[i] != 0 {
			anyAlpha = true
			break
		}
	}
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			i := (y*size + x) * 4
			b, g, rr, a := px[i], px[i+1], px[i+2], px[i+3]
			if !anyAlpha { // an old icon with no alpha: opaque
				a = 255
			} else if a > 0 && a < 255 { // un-premultiply
				rr = byte(int(rr) * 255 / int(a))
				g = byte(int(g) * 255 / int(a))
				b = byte(int(b) * 255 / int(a))
			}
			img.SetNRGBA(x, y, color.NRGBA{R: rr, G: g, B: b, A: a})
		}
	}
	var buf bytes.Buffer
	if png.Encode(&buf, img) != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

//go:build linux

package main

import (
	"log"
	"os"
)

// The window's web view on Linux is WebKitGTK. Its newer DMA-BUF renderer
// (WebKitGTK 2.42 and later) is known to lose the page on many graphics
// drivers — older Intel chips and NVIDIA especially: everything stops
// drawing and the window turns plain grey. Turned off here, before the web
// view starts, unless it's already been set either way.
//
// If it still happens, starting Chriscord with
// WEBKIT_DISABLE_COMPOSITING_MODE=1 (drawing without the graphics card)
// is the next thing to try.
func init() {
	if _, set := os.LookupEnv("WEBKIT_DISABLE_DMABUF_RENDERER"); !set {
		os.Setenv("WEBKIT_DISABLE_DMABUF_RENDERER", "1")
	}
	if os.Getenv("WEBKIT_DISABLE_COMPOSITING_MODE") != "" {
		log.Printf("webview: WEBKIT_DISABLE_COMPOSITING_MODE is set — drawing without the graphics card")
	}
}

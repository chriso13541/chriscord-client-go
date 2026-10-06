// Sharing one window on Windows 10 without the yellow border.
//
// Windows Graphics Capture (FFmpeg's gfxcapture) always outlines a captured
// window in yellow on Windows 10 — only Windows 11 lets a program turn that
// off. So on Windows 10 the window is copied the way Chrome's (and so
// Discord's) window sharing does it: PrintWindow with PW_RENDERFULLCONTENT,
// which asks the desktop compositor for the window's finished picture —
// including GPU-drawn content (browsers, Electron apps, video players) that
// a plain BitBlt of the window (FFmpeg's gdigrab) shows as black. It works
// while the window is covered by others, too.
//
// Each frame: the window is copied whole, the mouse pointer drawn on if
// it's over the window, the invisible resize borders cut off, and the rest
// fitted (keeping its shape, black bars if needed) into a fixed-size BGRA
// picture — so FFmpeg gets the same size of frame even if the window is
// resized mid-share.
//
// Everything here must run on one OS thread (screen_gdi_windows.go locks it).

#define WIN32_LEAN_AND_MEAN
#ifndef _WIN32_WINNT
#define _WIN32_WINNT 0x0A00
#endif
#include <windows.h>
#include <stdlib.h>
#include <string.h>

#include "screen_gdi_windows.h"

#ifndef PW_RENDERFULLCONTENT
#define PW_RENDERFULLCONTENT 0x00000002
#endif
#define CC_DWMWA_EXTENDED_FRAME_BOUNDS 9

typedef HRESULT (WINAPI *cc_dwm_get_attr_fn)(HWND, DWORD, PVOID, DWORD);

struct gdicap {
    HWND hwnd;
    int ow, oh;          // output size
    HDC screen;          // the screen's DC, for compatible bitmaps
    HDC full;            // the whole window, as PrintWindow draws it
    HBITMAP full_bmp, full_old;
    int fw, fh;
    HDC out;             // the output picture (a top-down 32-bit DIB)
    HBITMAP out_bmp, out_old;
    void *bits;
    HMODULE dwm;
    cc_dwm_get_attr_fn get_attr;
    HCURSOR cur;         // last pointer seen, and its hotspot
    int cur_hx, cur_hy;
};

gdicap *gdicap_open(unsigned long long hwnd, int width, int height) {
    if (width <= 0 || height <= 0) return NULL;
    gdicap *c = (gdicap *)calloc(1, sizeof(*c));
    if (!c) return NULL;
    c->hwnd = (HWND)(ULONG_PTR)hwnd;
    c->ow = width;
    c->oh = height;
    c->screen = GetDC(NULL);
    if (!c->screen) goto fail;
    c->full = CreateCompatibleDC(c->screen);
    c->out = CreateCompatibleDC(c->screen);
    if (!c->full || !c->out) goto fail;

    BITMAPINFO bi;
    memset(&bi, 0, sizeof(bi));
    bi.bmiHeader.biSize = sizeof(bi.bmiHeader);
    bi.bmiHeader.biWidth = width;
    bi.bmiHeader.biHeight = -height; // top-down, like FFmpeg's raw video
    bi.bmiHeader.biPlanes = 1;
    bi.bmiHeader.biBitCount = 32;
    bi.bmiHeader.biCompression = BI_RGB;
    c->out_bmp = CreateDIBSection(c->screen, &bi, DIB_RGB_COLORS, &c->bits, NULL, 0);
    if (!c->out_bmp || !c->bits) goto fail;
    c->out_old = (HBITMAP)SelectObject(c->out, c->out_bmp);
    PatBlt(c->out, 0, 0, width, height, BLACKNESS);

    c->dwm = LoadLibraryW(L"dwmapi.dll");
    if (c->dwm) c->get_attr = (cc_dwm_get_attr_fn)(void *)GetProcAddress(c->dwm, "DwmGetWindowAttribute");
    return c;
fail:
    gdicap_close(c);
    return NULL;
}

void *gdicap_bits(gdicap *c) { return c->bits; }

static void cc_free_full(gdicap *c) {
    if (c->full_bmp) {
        SelectObject(c->full, c->full_old);
        DeleteObject(c->full_bmp);
        c->full_bmp = NULL;
    }
    c->fw = c->fh = 0;
}

void gdicap_close(gdicap *c) {
    if (!c) return;
    cc_free_full(c);
    if (c->out_bmp) {
        SelectObject(c->out, c->out_old);
        DeleteObject(c->out_bmp);
    }
    if (c->full) DeleteDC(c->full);
    if (c->out) DeleteDC(c->out);
    if (c->screen) ReleaseDC(NULL, c->screen);
    if (c->dwm) FreeLibrary(c->dwm);
    free(c);
}

// The mouse pointer, drawn onto the window's picture — only while it's
// showing (a video player hiding it hides it here too) and over this
// window rather than one in front of it.
static void cc_draw_cursor(gdicap *c, const RECT *wr) {
    CURSORINFO ci;
    memset(&ci, 0, sizeof(ci));
    ci.cbSize = sizeof(ci);
    if (!GetCursorInfo(&ci) || !(ci.flags & CURSOR_SHOWING) || !ci.hCursor) return;
    HWND under = WindowFromPoint(ci.ptScreenPos);
    if (!under || GetAncestor(under, GA_ROOT) != c->hwnd) return;
    if (ci.hCursor != c->cur) {
        ICONINFO ii;
        c->cur = ci.hCursor;
        c->cur_hx = c->cur_hy = 0;
        if (GetIconInfo(ci.hCursor, &ii)) {
            c->cur_hx = (int)ii.xHotspot;
            c->cur_hy = (int)ii.yHotspot;
            if (ii.hbmMask) DeleteObject(ii.hbmMask);
            if (ii.hbmColor) DeleteObject(ii.hbmColor);
        }
    }
    DrawIconEx(c->full, ci.ptScreenPos.x - wr->left - c->cur_hx, ci.ptScreenPos.y - wr->top - c->cur_hy,
               ci.hCursor, 0, 0, 0, NULL, DI_NORMAL);
}

int gdicap_frame(gdicap *c, int draw_cursor) {
    if (!IsWindow(c->hwnd)) return GDICAP_GONE;
    if (IsIconic(c->hwnd)) return GDICAP_HIDDEN;
    RECT wr;
    if (!GetWindowRect(c->hwnd, &wr)) return GDICAP_GONE;
    int ww = wr.right - wr.left, wh = wr.bottom - wr.top;
    if (ww <= 0 || wh <= 0) return GDICAP_HIDDEN;

    // The part of the window that's actually seen: without the invisible
    // resize borders (and, maximised, without what hangs off the screen).
    RECT vis = wr, fb;
    if (c->get_attr && c->get_attr(c->hwnd, CC_DWMWA_EXTENDED_FRAME_BOUNDS, &fb, sizeof(fb)) == S_OK &&
        fb.right > fb.left && fb.bottom > fb.top)
        vis = fb;
    if (IsZoomed(c->hwnd)) {
        MONITORINFO mi;
        memset(&mi, 0, sizeof(mi));
        mi.cbSize = sizeof(mi);
        RECT clip;
        if (GetMonitorInfoW(MonitorFromWindow(c->hwnd, MONITOR_DEFAULTTONEAREST), &mi) &&
            IntersectRect(&clip, &vis, &mi.rcWork))
            vis = clip;
    }
    int cx = vis.left - wr.left, cy = vis.top - wr.top;
    int cw = vis.right - vis.left, ch = vis.bottom - vis.top;
    if (cx < 0) { cw += cx; cx = 0; }
    if (cy < 0) { ch += cy; cy = 0; }
    if (cx + cw > ww) cw = ww - cx;
    if (cy + ch > wh) ch = wh - cy;
    if (cw <= 0 || ch <= 0) return GDICAP_HIDDEN;

    if (ww != c->fw || wh != c->fh) {
        cc_free_full(c);
        c->full_bmp = CreateCompatibleBitmap(c->screen, ww, wh);
        if (!c->full_bmp) return GDICAP_FAIL;
        c->full_old = (HBITMAP)SelectObject(c->full, c->full_bmp);
        c->fw = ww;
        c->fh = wh;
    }
    if (!PrintWindow(c->hwnd, c->full, PW_RENDERFULLCONTENT)) return GDICAP_FAIL;
    if (draw_cursor) cc_draw_cursor(c, &wr);

    // Fit it into the output, keeping its shape.
    int dw, dh;
    if ((long long)cw * c->oh > (long long)ch * c->ow) {
        dw = c->ow;
        dh = (int)((long long)ch * c->ow / cw);
    } else {
        dh = c->oh;
        dw = (int)((long long)cw * c->oh / ch);
    }
    if (dw < 1) dw = 1;
    if (dh < 1) dh = 1;
    int dx = (c->ow - dw) / 2, dy = (c->oh - dh) / 2;
    if (dw != c->ow || dh != c->oh) PatBlt(c->out, 0, 0, c->ow, c->oh, BLACKNESS);
    BOOL ok;
    if (dw == cw && dh == ch) {
        ok = BitBlt(c->out, dx, dy, dw, dh, c->full, cx, cy, SRCCOPY);
    } else {
        SetStretchBltMode(c->out, HALFTONE); // smooth scaling, readable text
        SetBrushOrgEx(c->out, 0, 0, NULL);
        ok = StretchBlt(c->out, dx, dy, dw, dh, c->full, cx, cy, cw, ch, SRCCOPY);
    }
    GdiFlush(); // the DIB's pixels are read straight after this
    return ok ? GDICAP_OK : GDICAP_FAIL;
}

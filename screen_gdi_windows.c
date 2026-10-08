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
// Video players: when the window has a video area of its own (VLC draws its
// video into a child window), only that area is sent — not the player's
// menus, toolbar and seek bar — and it follows the area as the window is
// resized or goes full screen. With no video showing (an audio file, the
// playlist), the whole window is sent.
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
    HWND video;          // the player's video area, if it has one (see video_child)
    int video_check;     // frames until it's looked for again
    int screen_mode;     // copying a part of the desktop, not a window
    RECT src;            // that part, in desktop coordinates
};

// Window classes of video players' video areas. VLC's video output lives
// in "VLC video main", with the picture in its "VLC video output" child.
static const wchar_t *cc_video_classes[] = { L"VLC video main", L"VLC video output" };

typedef struct { HWND best; LONG area; } cc_video_search;

static BOOL CALLBACK cc_find_video(HWND h, LPARAM lp) {
    cc_video_search *vs = (cc_video_search *)lp;
    wchar_t cls[64];
    if (!IsWindowVisible(h) || !GetClassNameW(h, cls, 64)) return TRUE;
    for (size_t i = 0; i < sizeof(cc_video_classes) / sizeof(cc_video_classes[0]); i++) {
        if (wcscmp(cls, cc_video_classes[i]) != 0) continue;
        RECT r;
        if (GetWindowRect(h, &r)) {
            LONG area = (r.right - r.left) * (r.bottom - r.top);
            if (area > vs->area) { vs->area = area; vs->best = h; } // the largest: the whole picture
        }
    }
    return TRUE;
}

// video_child: the visible video area inside top, or NULL. Tiny ones (a
// player with nothing playing still has one, a few pixels big) don't count.
static HWND video_child(HWND top) {
    cc_video_search vs = { NULL, 0 };
    EnumChildWindows(top, cc_find_video, (LPARAM)&vs);
    return vs.area >= 160 * 90 ? vs.best : NULL;
}

int gdicap_video_size(unsigned long long hwnd, int *w, int *h) {
    HWND v = video_child((HWND)(ULONG_PTR)hwnd);
    RECT r;
    if (!v || !GetWindowRect(v, &r)) return 0;
    *w = r.right - r.left;
    *h = r.bottom - r.top;
    return 1;
}

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

// A whole monitor, copied with BitBlt from the screen's DC — what the
// share picker's thumbnails do. It's slower than Desktop Duplication and
// Windows Graphics Capture, but needs nothing from the graphics card, so
// it works where they can't: virtual machines (Hyper-V, VirtualBox,
// VMware with the basic display driver), Remote Desktop sessions.
gdicap *gdicap_open_screen(int x, int y, int src_w, int src_h, int width, int height) {
    if (src_w <= 0 || src_h <= 0) return NULL;
    gdicap *c = gdicap_open(0, width, height);
    if (!c) return NULL;
    c->screen_mode = 1;
    c->src.left = x;
    c->src.top = y;
    c->src.right = x + src_w;
    c->src.bottom = y + src_h;
    return c;
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

static int cc_screen_frame(gdicap *c, int draw_cursor) {
    int sw = c->src.right - c->src.left, sh = c->src.bottom - c->src.top;
    BOOL ok;
    // No CAPTUREBLT: with it, Windows hides the mouse pointer for every
    // copy (to include layered windows), so at 30 copies a second it
    // flickers on the sharer's own screen. Since Windows 8 the desktop is
    // always composited, and a plain copy already includes layered windows
    // (menus, tooltips) — Chrome's GDI capturer leaves it off for the same
    // reason. The pointer is drawn onto the copy below instead.
    if (sw == c->ow && sh == c->oh) {
        ok = BitBlt(c->out, 0, 0, c->ow, c->oh, c->screen, c->src.left, c->src.top, SRCCOPY);
    } else {
        SetStretchBltMode(c->out, HALFTONE);
        SetBrushOrgEx(c->out, 0, 0, NULL);
        ok = StretchBlt(c->out, 0, 0, c->ow, c->oh, c->screen, c->src.left, c->src.top, sw, sh,
                        SRCCOPY);
    }
    if (!ok) {
        GdiFlush();
        return GDICAP_FAIL; // e.g. the secure desktop (a UAC prompt) is up
    }
    if (draw_cursor) {
        // GDI copies leave the pointer out: draw it on, scaled into place.
        CURSORINFO ci;
        memset(&ci, 0, sizeof(ci));
        ci.cbSize = sizeof(ci);
        if (GetCursorInfo(&ci) && (ci.flags & CURSOR_SHOWING) && ci.hCursor &&
            PtInRect(&c->src, ci.ptScreenPos)) {
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
            int px = (int)((long long)(ci.ptScreenPos.x - c->src.left) * c->ow / sw);
            int py = (int)((long long)(ci.ptScreenPos.y - c->src.top) * c->oh / sh);
            DrawIconEx(c->out, px - c->cur_hx, py - c->cur_hy, ci.hCursor, 0, 0, 0, NULL, DI_NORMAL);
        }
    }
    GdiFlush(); // the DIB's pixels are read straight after this
    return GDICAP_OK;
}

int gdicap_frame(gdicap *c, int draw_cursor) {
    if (c->screen_mode) return cc_screen_frame(c, draw_cursor);
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
    // Just the video, when the player is showing one (looked for again
    // every 15 frames, and whenever the one found goes away).
    if (--c->video_check <= 0 || (c->video && !IsWindow(c->video))) {
        c->video = video_child(c->hwnd);
        c->video_check = 15;
    }
    if (c->video && IsWindowVisible(c->video)) {
        RECT vr, both;
        if (GetWindowRect(c->video, &vr) && IntersectRect(&both, &vr, &vis) &&
            (both.right - both.left) * (both.bottom - both.top) >= 160 * 90)
            vis = both;
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

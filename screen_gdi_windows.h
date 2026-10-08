#ifndef CC_SCREEN_GDI_WINDOWS_H
#define CC_SCREEN_GDI_WINDOWS_H

/* Border-free single-window capture for Windows 10 (screen_gdi_windows.c). */

typedef struct gdicap gdicap;

enum {
    GDICAP_OK = 0,
    GDICAP_GONE = 1,   /* the window has been closed */
    GDICAP_HIDDEN = 2, /* minimised (or zero-sized): last picture kept */
    GDICAP_FAIL = 3    /* this copy didn't work */
};

gdicap *gdicap_open(unsigned long long hwnd, int width, int height);
int gdicap_frame(gdicap *c, int draw_cursor);
void *gdicap_bits(gdicap *c);
void gdicap_close(gdicap *c);

#endif

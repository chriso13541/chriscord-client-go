// Reading a screen or window from PipeWire — what the desktop's
// screen-sharing portal hands out on Wayland (pwcapture_linux.c).
#ifndef CHRISCORD_PWCAPTURE_H
#define CHRISCORD_PWCAPTURE_H

#include <stdint.h>

typedef struct pwcap pwcap;

// pwcap_frame's answers.
#define PWCAP_FRESH 1   // a new picture is in dst
#define PWCAP_SAME 0    // nothing new since the last call (dst untouched)
#define PWCAP_NONE -2   // no picture has come yet
#define PWCAP_ENDED -1  // the stream has stopped (sharing ended from the desktop)

// pwcap_load opens libpipewire (once). 0 = ok; otherwise err says why.
int pwcap_load(char *err, int errlen);

// pwcap_open reads node from the PipeWire connection fd (which it takes
// over), asking for about fps pictures a second. NULL on failure (err).
pwcap *pwcap_open(int fd, uint32_t node, int fps, char *err, int errlen);

// pwcap_frame scales the latest picture into dst (w x h, BGRA, tightly
// packed), keeping its shape (black bars if it doesn't fit exactly).
int pwcap_frame(pwcap *c, uint8_t *dst, int w, int h);

// pwcap_size: the size of the pictures arriving (0x0 before the first).
void pwcap_size(pwcap *c, int *w, int *h);

void pwcap_close(pwcap *c);

#endif

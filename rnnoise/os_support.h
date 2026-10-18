/* os_support.h — added by chriscord, not part of the RNNoise release.

   RNNoise's vec_neon.h (used on ARM: Apple Silicon Macs, ARM Linux)
   includes Opus's os_support.h, which the RNNoise 0.2 release doesn't ship.
   It only needs OPUS_CLEAR from it, defined here the way Opus does. */
#ifndef OS_SUPPORT_H
#define OS_SUPPORT_H

#include <string.h>
#include "common.h"

#ifndef OPUS_CLEAR
#define OPUS_CLEAR(dst, n) (memset((dst), 0, (n)*sizeof(*(dst))))
#endif
#ifndef OPUS_COPY
#define OPUS_COPY(dst, src, n) (memcpy((dst), (src), (n)*sizeof(*(dst)) + 0*((dst)-(src)) ))
#endif

#endif

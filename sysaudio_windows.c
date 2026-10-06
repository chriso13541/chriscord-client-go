// System audio capture for screen sharing on Windows: WASAPI process
// loopback through miniaudio. "Exclude" mode records everything playing
// on the computer except this app (so a call's voices aren't sent back to
// the call); "include" mode records only one program (the window being
// shared) and anything it started.

#define MA_ENABLE_ONLY_SPECIFIC_BACKENDS
#define MA_ENABLE_WASAPI
#define MA_NO_DECODING
#define MA_NO_ENCODING
#define MA_NO_GENERATION
#define MA_NO_RESOURCE_MANAGER
#define MA_NO_NODE_GRAPH
#define MA_NO_ENGINE
#define MINIAUDIO_IMPLEMENTATION
#include "miniaudio.h"
#include "sysaudio_windows.h"

#include <stdio.h>
#include <string.h>

static ma_device sa_device;
static ma_pcm_rb sa_rb;
static int sa_running = 0;

static void sa_data(ma_device* dev, void* out, const void* in, ma_uint32 frames) {
    (void)dev; (void)out;
    while (frames > 0 && in != NULL) {
        ma_uint32 n = frames;
        void* dst;
        if (ma_pcm_rb_acquire_write(&sa_rb, &n, &dst) != MA_SUCCESS || n == 0) {
            return; /* full: the reader has fallen behind, drop the rest */
        }
        memcpy(dst, in, (size_t)n * 2 * sizeof(float));
        ma_pcm_rb_commit_write(&sa_rb, n);
        in = (const float*)in + (size_t)n * 2;
        frames -= n;
    }
}

int sa_start(unsigned int pid, int exclude, char* err, int errlen) {
    ma_device_config cfg;
    ma_result r;
    if (sa_running) sa_stop();
    r = ma_pcm_rb_init(ma_format_f32, 2, 48000, NULL, NULL, &sa_rb); /* 1 s */
    if (r != MA_SUCCESS) {
        snprintf(err, errlen, "couldn't make the audio buffer (%s)", ma_result_description(r));
        return -1;
    }
    cfg = ma_device_config_init(ma_device_type_loopback);
    cfg.capture.format = ma_format_f32;
    cfg.capture.channels = 2;
    cfg.sampleRate = 48000;
    cfg.dataCallback = sa_data;
    cfg.wasapi.loopbackProcessID = pid;
    cfg.wasapi.loopbackProcessExclude = exclude ? MA_TRUE : MA_FALSE;
    r = ma_device_init(NULL, &cfg, &sa_device);
    if (r != MA_SUCCESS) {
        ma_pcm_rb_uninit(&sa_rb);
        snprintf(err, errlen, "%s", ma_result_description(r));
        return -1;
    }
    r = ma_device_start(&sa_device);
    if (r != MA_SUCCESS) {
        ma_device_uninit(&sa_device);
        ma_pcm_rb_uninit(&sa_rb);
        snprintf(err, errlen, "%s", ma_result_description(r));
        return -1;
    }
    sa_running = 1;
    return 0;
}

int sa_read(float* out, int frames) {
    int total = 0;
    if (!sa_running) return 0;
    while (total < frames) {
        ma_uint32 n = (ma_uint32)(frames - total);
        void* src;
        if (ma_pcm_rb_acquire_read(&sa_rb, &n, &src) != MA_SUCCESS || n == 0) break;
        memcpy(out + (size_t)total * 2, src, (size_t)n * 2 * sizeof(float));
        ma_pcm_rb_commit_read(&sa_rb, n);
        total += (int)n;
    }
    return total;
}

int sa_available(void) {
    if (!sa_running) return 0;
    return (int)ma_pcm_rb_available_read(&sa_rb);
}

void sa_stop(void) {
    if (!sa_running) return;
    sa_running = 0;
    ma_device_uninit(&sa_device);
    ma_pcm_rb_uninit(&sa_rb);
}

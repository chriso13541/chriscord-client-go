// Reading a screen or window from PipeWire (see pwcapture_linux.go).
//
// libpipewire is opened with dlopen, not linked: the app still starts on a
// system without PipeWire (an older X11 desktop), where Wayland sharing is
// never needed anyway. Only its headers are needed to build
// (libpipewire-0.3-dev / pipewire-devel); the SPA helpers used here are
// all inline in those headers.
//
// The stream is asked for plain memory buffers in a 32-bit RGB format —
// no DMA-BUF, which would need the graphics card to read back — and the
// newest picture is copied out as it arrives (on PipeWire's own thread).
// pwcap_frame then scales that copy to the size being sent, when the
// sending side asks for a frame.

#include "pwcapture_linux.h"

#include <dlfcn.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include <pipewire/pipewire.h>
#include <spa/param/video/format-utils.h>
#include <spa/param/props.h>
#include <spa/buffer/meta.h>
#include <spa/debug/types.h>

// ── libpipewire, opened at run time ─────────────────────────────────────

#define PWFN(name) __typeof__(name) *name;
static struct {
	PWFN(pw_init)
	PWFN(pw_thread_loop_new)
	PWFN(pw_thread_loop_get_loop)
	PWFN(pw_thread_loop_start)
	PWFN(pw_thread_loop_stop)
	PWFN(pw_thread_loop_lock)
	PWFN(pw_thread_loop_unlock)
	PWFN(pw_thread_loop_destroy)
	PWFN(pw_context_new)
	PWFN(pw_context_destroy)
	PWFN(pw_context_connect_fd)
	PWFN(pw_core_disconnect)
	PWFN(pw_properties_new)
	PWFN(pw_stream_new)
	PWFN(pw_stream_add_listener)
	PWFN(pw_stream_connect)
	PWFN(pw_stream_disconnect)
	PWFN(pw_stream_dequeue_buffer)
	PWFN(pw_stream_queue_buffer)
	PWFN(pw_stream_update_params)
	PWFN(pw_stream_destroy)
} pw;
#undef PWFN

static pthread_mutex_t load_mu = PTHREAD_MUTEX_INITIALIZER;
static int loaded; // 1 ok, -1 failed
static char load_err[256];

int pwcap_load(char *err, int errlen) {
	pthread_mutex_lock(&load_mu);
	if (!loaded) {
		void *lib = dlopen("libpipewire-0.3.so.0", RTLD_NOW | RTLD_LOCAL);
		if (!lib) {
			snprintf(load_err, sizeof load_err, "PipeWire isn't installed (%s)", dlerror());
			loaded = -1;
		} else {
			loaded = 1;
#define LOAD(name)                                                                      \
	if (loaded == 1 && !(pw.name = (__typeof__(pw.name))dlsym(lib, #name))) {         \
		snprintf(load_err, sizeof load_err, "this PipeWire has no %s", #name);       \
		loaded = -1;                                                                    \
	}
			LOAD(pw_init)
			LOAD(pw_thread_loop_new)
			LOAD(pw_thread_loop_get_loop)
			LOAD(pw_thread_loop_start)
			LOAD(pw_thread_loop_stop)
			LOAD(pw_thread_loop_lock)
			LOAD(pw_thread_loop_unlock)
			LOAD(pw_thread_loop_destroy)
			LOAD(pw_context_new)
			LOAD(pw_context_destroy)
			LOAD(pw_context_connect_fd)
			LOAD(pw_core_disconnect)
			LOAD(pw_properties_new)
			LOAD(pw_stream_new)
			LOAD(pw_stream_add_listener)
			LOAD(pw_stream_connect)
			LOAD(pw_stream_disconnect)
			LOAD(pw_stream_dequeue_buffer)
			LOAD(pw_stream_queue_buffer)
			LOAD(pw_stream_update_params)
			LOAD(pw_stream_destroy)
#undef LOAD
			if (loaded == 1)
				pw.pw_init(NULL, NULL);
		}
	}
	int ok = loaded == 1;
	if (!ok && err && errlen > 0)
		snprintf(err, errlen, "%s", load_err);
	pthread_mutex_unlock(&load_mu);
	return ok ? 0 : -1;
}

// ── One stream ──────────────────────────────────────────────────────────

struct pwcap {
	struct pw_thread_loop *loop;
	struct pw_context *ctx;
	struct pw_core *core;
	struct pw_stream *stream;
	struct spa_hook listener;
	struct spa_video_info_raw fmt;
	int have_fmt;

	pthread_mutex_t mu; // guards everything below
	uint8_t *frame;     // the newest picture, tightly packed, 4 bytes a pixel
	size_t cap;
	int fw, fh;
	int rgb;   // the picture is R,G,B,x rather than B,G,R,x
	int fresh; // not taken by pwcap_frame yet
	int got;   // at least one picture has come
	int ended;
};

static void on_state_changed(void *data, enum pw_stream_state old, enum pw_stream_state state, const char *error) {
	struct pwcap *c = data;
	(void)old;
	if (state == PW_STREAM_STATE_ERROR || state == PW_STREAM_STATE_UNCONNECTED) {
		pthread_mutex_lock(&c->mu);
		c->ended = 1;
		pthread_mutex_unlock(&c->mu);
		if (error)
			fprintf(stderr, "pwcapture: stream ended: %s\n", error);
	}
}

static void on_param_changed(void *data, uint32_t id, const struct spa_pod *param) {
	struct pwcap *c = data;
	if (id != SPA_PARAM_Format || param == NULL)
		return;
	uint32_t mtype, msub;
	if (spa_format_parse(param, &mtype, &msub) < 0 || mtype != SPA_MEDIA_TYPE_video || msub != SPA_MEDIA_SUBTYPE_raw)
		return;
	struct spa_video_info_raw info;
	memset(&info, 0, sizeof info);
	if (spa_format_video_raw_parse(param, &info) < 0)
		return;
	c->fmt = info;
	c->have_fmt = 1;

	// Plain memory, and the header (to skip damaged pictures) and crop
	// (a window inside a bigger buffer) notes.
	uint8_t buf[1024];
	struct spa_pod_builder b = SPA_POD_BUILDER_INIT(buf, sizeof buf);
	const struct spa_pod *params[3];
	params[0] = spa_pod_builder_add_object(&b, SPA_TYPE_OBJECT_ParamBuffers, SPA_PARAM_Buffers,
		SPA_PARAM_BUFFERS_buffers, SPA_POD_CHOICE_RANGE_Int(4, 2, 8),
		SPA_PARAM_BUFFERS_dataType, SPA_POD_CHOICE_FLAGS_Int((1 << SPA_DATA_MemPtr) | (1 << SPA_DATA_MemFd)));
	params[1] = spa_pod_builder_add_object(&b, SPA_TYPE_OBJECT_ParamMeta, SPA_PARAM_Meta,
		SPA_PARAM_META_type, SPA_POD_Id(SPA_META_Header),
		SPA_PARAM_META_size, SPA_POD_Int(sizeof(struct spa_meta_header)));
	params[2] = spa_pod_builder_add_object(&b, SPA_TYPE_OBJECT_ParamMeta, SPA_PARAM_Meta,
		SPA_PARAM_META_type, SPA_POD_Id(SPA_META_VideoCrop),
		SPA_PARAM_META_size, SPA_POD_Int(sizeof(struct spa_meta_region)));
	pw.pw_stream_update_params(c->stream, params, 3);
}

static void on_process(void *data) {
	struct pwcap *c = data;
	struct pw_buffer *b = NULL, *next;
	// Only the newest picture matters: give back any older ones.
	while ((next = pw.pw_stream_dequeue_buffer(c->stream)) != NULL) {
		if (b)
			pw.pw_stream_queue_buffer(c->stream, b);
		b = next;
	}
	if (!b)
		return;
	struct spa_buffer *buf = b->buffer;
	struct spa_data *d = &buf->datas[0];
	struct spa_meta_header *hdr = spa_buffer_find_meta_data(buf, SPA_META_Header, sizeof(*hdr));
	if (!c->have_fmt || buf->n_datas < 1 || d->data == NULL || d->chunk == NULL || d->chunk->size == 0 ||
	    (hdr && (hdr->flags & SPA_META_HEADER_FLAG_CORRUPTED)))
		goto done;

	int bw = (int)c->fmt.size.width, bh = (int)c->fmt.size.height;
	int stride = d->chunk->stride > 0 ? d->chunk->stride : bw * 4;
	int x = 0, y = 0, w = bw, h = bh;
	struct spa_meta_region *crop = spa_buffer_find_meta_data(buf, SPA_META_VideoCrop, sizeof(*crop));
	if (crop && spa_meta_region_is_valid(crop)) {
		x = crop->region.position.x;
		y = crop->region.position.y;
		w = (int)crop->region.size.width;
		h = (int)crop->region.size.height;
		if (x < 0 || y < 0 || x + w > bw || y + h > bh) {
			x = y = 0;
			w = bw;
			h = bh;
		}
	}
	if (w <= 0 || h <= 0)
		goto done;
	const uint8_t *src = (const uint8_t *)d->data + (d->chunk->offset % (d->maxsize ? d->maxsize : 1));
	if ((size_t)stride * (size_t)(y + h) > d->maxsize)
		goto done; // shorter than its size says: skip it

	int rgb = c->fmt.format == SPA_VIDEO_FORMAT_RGBx || c->fmt.format == SPA_VIDEO_FORMAT_RGBA;
	size_t need = (size_t)w * h * 4;
	pthread_mutex_lock(&c->mu);
	if (c->cap < need) {
		uint8_t *nf = realloc(c->frame, need);
		if (!nf) {
			pthread_mutex_unlock(&c->mu);
			goto done;
		}
		c->frame = nf;
		c->cap = need;
	}
	for (int r = 0; r < h; r++)
		memcpy(c->frame + (size_t)r * w * 4, src + (size_t)(y + r) * stride + (size_t)x * 4, (size_t)w * 4);
	c->fw = w;
	c->fh = h;
	c->rgb = rgb;
	c->fresh = 1;
	c->got = 1;
	pthread_mutex_unlock(&c->mu);
done:
	pw.pw_stream_queue_buffer(c->stream, b);
}

static const struct pw_stream_events stream_events = {
	PW_VERSION_STREAM_EVENTS,
	.state_changed = on_state_changed,
	.param_changed = on_param_changed,
	.process = on_process,
};

pwcap *pwcap_open(int fd, uint32_t node, int fps, char *err, int errlen) {
	if (pwcap_load(err, errlen) != 0) {
		close(fd);
		return NULL;
	}
	if (fps <= 0)
		fps = 30;
	struct pwcap *c = calloc(1, sizeof *c);
	if (!c) {
		close(fd);
		snprintf(err, errlen, "out of memory");
		return NULL;
	}
	pthread_mutex_init(&c->mu, NULL);
	c->loop = pw.pw_thread_loop_new("chriscord-screen", NULL);
	if (!c->loop) {
		close(fd);
		snprintf(err, errlen, "couldn't start PipeWire's thread");
		free(c);
		return NULL;
	}
	c->ctx = pw.pw_context_new(pw.pw_thread_loop_get_loop(c->loop), NULL, 0);
	if (!c->ctx || pw.pw_thread_loop_start(c->loop) < 0) {
		close(fd);
		snprintf(err, errlen, "couldn't set up PipeWire");
		pwcap_close(c);
		return NULL;
	}
	pw.pw_thread_loop_lock(c->loop);
	c->core = pw.pw_context_connect_fd(c->ctx, fd, NULL, 0); // takes the fd
	if (!c->core) {
		pw.pw_thread_loop_unlock(c->loop);
		snprintf(err, errlen, "couldn't connect to PipeWire");
		pwcap_close(c);
		return NULL;
	}
	c->stream = pw.pw_stream_new(c->core, "Chriscord screen share",
		pw.pw_properties_new(PW_KEY_MEDIA_TYPE, "Video", PW_KEY_MEDIA_CATEGORY, "Capture", PW_KEY_MEDIA_ROLE, "Screen", NULL));
	if (!c->stream) {
		pw.pw_thread_loop_unlock(c->loop);
		snprintf(err, errlen, "couldn't make a PipeWire stream");
		pwcap_close(c);
		return NULL;
	}
	pw.pw_stream_add_listener(c->stream, &c->listener, &stream_events, c);

	uint8_t buf[1024];
	struct spa_pod_builder b = SPA_POD_BUILDER_INIT(buf, sizeof buf);
	const struct spa_pod *params[1];
	params[0] = spa_pod_builder_add_object(&b, SPA_TYPE_OBJECT_Format, SPA_PARAM_EnumFormat,
		SPA_FORMAT_mediaType, SPA_POD_Id(SPA_MEDIA_TYPE_video),
		SPA_FORMAT_mediaSubtype, SPA_POD_Id(SPA_MEDIA_SUBTYPE_raw),
		SPA_FORMAT_VIDEO_format, SPA_POD_CHOICE_ENUM_Id(5, SPA_VIDEO_FORMAT_BGRx, SPA_VIDEO_FORMAT_BGRx,
			SPA_VIDEO_FORMAT_BGRA, SPA_VIDEO_FORMAT_RGBx, SPA_VIDEO_FORMAT_RGBA),
		SPA_FORMAT_VIDEO_size, SPA_POD_CHOICE_RANGE_Rectangle(
			&SPA_RECTANGLE(1920, 1080), &SPA_RECTANGLE(1, 1), &SPA_RECTANGLE(16384, 16384)),
		SPA_FORMAT_VIDEO_framerate, SPA_POD_CHOICE_RANGE_Fraction(
			&SPA_FRACTION((uint32_t)fps, 1), &SPA_FRACTION(0, 1), &SPA_FRACTION(360, 1)));
	int res = pw.pw_stream_connect(c->stream, PW_DIRECTION_INPUT, node,
		PW_STREAM_FLAG_AUTOCONNECT | PW_STREAM_FLAG_MAP_BUFFERS, params, 1);
	pw.pw_thread_loop_unlock(c->loop);
	if (res < 0) {
		snprintf(err, errlen, "couldn't open the screen's PipeWire stream (%s)", strerror(-res));
		pwcap_close(c);
		return NULL;
	}
	return c;
}

void pwcap_size(pwcap *c, int *w, int *h) {
	pthread_mutex_lock(&c->mu);
	*w = c->got ? c->fw : 0;
	*h = c->got ? c->fh : 0;
	pthread_mutex_unlock(&c->mu);
}

int pwcap_frame(pwcap *c, uint8_t *dst, int w, int h) {
	pthread_mutex_lock(&c->mu);
	if (c->ended) {
		pthread_mutex_unlock(&c->mu);
		return PWCAP_ENDED;
	}
	if (!c->got) {
		pthread_mutex_unlock(&c->mu);
		return PWCAP_NONE;
	}
	if (!c->fresh) {
		pthread_mutex_unlock(&c->mu);
		return PWCAP_SAME;
	}
	c->fresh = 0;
	int sw = c->fw, sh = c->fh, rgb = c->rgb;
	const uint8_t *src = c->frame;
	int ri = rgb ? 0 : 2, bi = rgb ? 2 : 0; // where red and blue are in the source

	// Fit the picture in w x h keeping its shape; the rest is black.
	int ow = w, oh = (int)((long long)sh * w / sw);
	if (oh > h) {
		oh = h;
		ow = (int)((long long)sw * h / sh);
	}
	if (ow < 1)
		ow = 1;
	if (oh < 1)
		oh = 1;
	int ox = (w - ow) / 2, oy = (h - oh) / 2;
	if (ow != w || oh != h) {
		for (size_t i = 0; i < (size_t)w * h; i++) {
			dst[i * 4 + 0] = dst[i * 4 + 1] = dst[i * 4 + 2] = 0;
			dst[i * 4 + 3] = 255;
		}
	}

	if (ow == sw && oh == sh && !rgb) {
		for (int y = 0; y < oh; y++) {
			uint8_t *o = dst + ((size_t)(oy + y) * w + ox) * 4;
			memcpy(o, src + (size_t)y * sw * 4, (size_t)sw * 4);
			for (int x = 0; x < ow; x++)
				o[x * 4 + 3] = 255;
		}
		pthread_mutex_unlock(&c->mu);
		return PWCAP_FRESH;
	}

	// Smaller (the usual case): each output pixel averages the block of
	// source pixels it covers, which keeps text readable. Bigger: the
	// nearest source pixel.
	for (int y = 0; y < oh; y++) {
		int y0 = (int)((long long)y * sh / oh), y1 = (int)((long long)(y + 1) * sh / oh);
		if (y1 <= y0)
			y1 = y0 + 1;
		if (y1 > sh)
			y1 = sh;
		uint8_t *o = dst + ((size_t)(oy + y) * w + ox) * 4;
		for (int x = 0; x < ow; x++) {
			int x0 = (int)((long long)x * sw / ow), x1 = (int)((long long)(x + 1) * sw / ow);
			if (x1 <= x0)
				x1 = x0 + 1;
			if (x1 > sw)
				x1 = sw;
			unsigned r = 0, g = 0, bl = 0, n = 0;
			for (int yy = y0; yy < y1; yy++) {
				const uint8_t *p = src + ((size_t)yy * sw + x0) * 4;
				for (int xx = x0; xx < x1; xx++, p += 4) {
					r += p[ri];
					g += p[1];
					bl += p[bi];
					n++;
				}
			}
			o[x * 4 + 0] = (uint8_t)(bl / n);
			o[x * 4 + 1] = (uint8_t)(g / n);
			o[x * 4 + 2] = (uint8_t)(r / n);
			o[x * 4 + 3] = 255;
		}
	}
	pthread_mutex_unlock(&c->mu);
	return PWCAP_FRESH;
}

void pwcap_close(pwcap *c) {
	if (!c)
		return;
	if (c->loop) {
		pw.pw_thread_loop_lock(c->loop);
		if (c->stream) {
			pw.pw_stream_disconnect(c->stream);
			pw.pw_stream_destroy(c->stream);
			c->stream = NULL;
		}
		if (c->core) {
			pw.pw_core_disconnect(c->core);
			c->core = NULL;
		}
		pw.pw_thread_loop_unlock(c->loop);
		pw.pw_thread_loop_stop(c->loop);
	}
	if (c->ctx)
		pw.pw_context_destroy(c->ctx);
	if (c->loop)
		pw.pw_thread_loop_destroy(c->loop);
	pthread_mutex_destroy(&c->mu);
	free(c->frame);
	free(c);
}

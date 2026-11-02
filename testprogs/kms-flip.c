// SPDX-License-Identifier: GPL-2.0
/*
 * Page-flip GPU-rendered frames on the first connected connector of a DRM
 * card, in its preferred mode, and report the rate of the flips. Each frame
 * is a scanout buffer of a GBM surface that GLES clears to a new color, and
 * the program waits for each flip's event before it renders the next frame.
 * It also reports how long rendering and flipping took on average, and how
 * many refresh periods lay between the flips by the events' timestamps.
 *
 * usage: kms-flip <card> <frames>
 */
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <stdio.h>
#include <stdlib.h>
#include <time.h>
#include <unistd.h>

#include <EGL/egl.h>
#include <EGL/eglext.h>
#include <GLES2/gl2.h>
#include <drm_fourcc.h>
#include <gbm.h>
#include <xf86drm.h>
#include <xf86drmMode.h>

static const char *prog = "kms-flip";

static void die(const char *msg)
{
	fprintf(stderr, "%s: %s\n", prog, msg);
	exit(1);
}

static double now(void)
{
	struct timespec ts;

	clock_gettime(CLOCK_MONOTONIC, &ts);
	return ts.tv_sec + ts.tv_nsec / 1e9;
}

/* A flip that is pending, and the timestamp of its event. */
struct flip {
	int pending;
	double time;
};

static void flip_done(int fd, unsigned int seq, unsigned int sec,
		      unsigned int usec, void *data)
{
	struct flip *f = data;

	(void)fd;
	(void)seq;
	f->pending = 0;
	f->time = sec + usec / 1e6;
}

/* Return the framebuffer of a GBM buffer, which the buffer keeps. */
static uint32_t bo_fb(int fd, struct gbm_bo *bo)
{
	uint32_t fb = (uintptr_t)gbm_bo_get_user_data(bo);
	uint32_t handles[4] = { gbm_bo_get_handle(bo).u32 };
	uint32_t pitches[4] = { gbm_bo_get_stride(bo) };
	uint32_t offsets[4] = { 0 };

	if (fb)
		return fb;
	if (drmModeAddFB2(fd, gbm_bo_get_width(bo), gbm_bo_get_height(bo),
			  DRM_FORMAT_XRGB8888, handles, pitches, offsets, &fb,
			  0))
		die("drmModeAddFB2 failed");
	gbm_bo_set_user_data(bo, (void *)(uintptr_t)fb, NULL);
	return fb;
}

int main(int argc, char **argv)
{
	PFNEGLGETPLATFORMDISPLAYEXTPROC get_display;
	drmEventContext ev = {
		.version = 2,
		.page_flip_handler = flip_done,
	};
	EGLint attribs[] = {
		EGL_SURFACE_TYPE, EGL_WINDOW_BIT,
		EGL_RENDERABLE_TYPE, EGL_OPENGL_ES2_BIT,
		EGL_RED_SIZE, 8, EGL_GREEN_SIZE, 8, EGL_BLUE_SIZE, 8,
		EGL_NONE,
	};
	EGLint ctx_attribs[] = { EGL_CONTEXT_CLIENT_VERSION, 2, EGL_NONE };
	drmModeConnector *conn = NULL;
	drmModeModeInfo *mode = NULL;
	struct gbm_bo *bo, *prev = NULL;
	struct gbm_surface *surface;
	struct gbm_device *gbm;
	drmModeEncoder *enc;
	drmModeRes *res;
	EGLConfig configs[64], config = NULL;
	EGLDisplay dpy;
	EGLContext ctx;
	EGLSurface surf;
	EGLint n, id;
	uint32_t crtc;
	int fd, frames, i, j;
	double start, t, frame, render = 0, flip = 0, last = 0, period;
	unsigned int periods[4] = { 0 };
	struct flip f;

	setvbuf(stdout, NULL, _IOLBF, 0);
	if (argc != 3 || (frames = atoi(argv[2])) < 2) {
		fprintf(stderr, "usage: %s <card> <frames>\n", prog);
		return 2;
	}
	fd = open(argv[1], O_RDWR | O_CLOEXEC);
	if (fd < 0) {
		perror(argv[1]);
		return 1;
	}
	if (drmSetMaster(fd))
		die("drmSetMaster failed");

	res = drmModeGetResources(fd);
	if (!res)
		die("drmModeGetResources failed");
	for (i = 0; i < res->count_connectors && !conn; i++) {
		conn = drmModeGetConnector(fd, res->connectors[i]);
		if (conn && (conn->connection != DRM_MODE_CONNECTED ||
			     !conn->count_modes)) {
			drmModeFreeConnector(conn);
			conn = NULL;
		}
	}
	if (!conn)
		die("no connected connector");
	mode = &conn->modes[0];
	for (i = 0; i < conn->count_modes; i++) {
		if (conn->modes[i].type & DRM_MODE_TYPE_PREFERRED)
			mode = &conn->modes[i];
	}

	/* The encoder's CRTC, or the first CRTC that an encoder can drive. */
	crtc = 0;
	for (i = 0; i < conn->count_encoders && !crtc; i++) {
		enc = drmModeGetEncoder(fd, conn->encoders[i]);
		if (!enc)
			continue;
		if (enc->crtc_id)
			crtc = enc->crtc_id;
		for (j = 0; j < res->count_crtcs && !crtc; j++) {
			if (enc->possible_crtcs & (1u << j))
				crtc = res->crtcs[j];
		}
		drmModeFreeEncoder(enc);
	}
	if (!crtc)
		die("no CRTC for the connector");
	printf("%s: connector %u, %ux%u@%u\n", prog, conn->connector_id,
	       mode->hdisplay, mode->vdisplay, mode->vrefresh);

	gbm = gbm_create_device(fd);
	if (!gbm)
		die("gbm_create_device failed");
	surface = gbm_surface_create(gbm, mode->hdisplay, mode->vdisplay,
				     GBM_FORMAT_XRGB8888,
				     GBM_BO_USE_SCANOUT | GBM_BO_USE_RENDERING);
	if (!surface)
		die("gbm_surface_create failed");

	get_display = (PFNEGLGETPLATFORMDISPLAYEXTPROC)
		eglGetProcAddress("eglGetPlatformDisplayEXT");
	dpy = get_display(EGL_PLATFORM_GBM_KHR, gbm, NULL);
	if (dpy == EGL_NO_DISPLAY || !eglInitialize(dpy, NULL, NULL))
		die("eglInitialize failed");
	eglBindAPI(EGL_OPENGL_ES_API);
	if (!eglChooseConfig(dpy, attribs, configs, 64, &n))
		die("eglChooseConfig failed");
	for (i = 0; i < n && !config; i++) {
		if (eglGetConfigAttrib(dpy, configs[i], EGL_NATIVE_VISUAL_ID,
				       &id) && id == GBM_FORMAT_XRGB8888)
			config = configs[i];
	}
	if (!config)
		die("no EGL config for XRGB8888");
	ctx = eglCreateContext(dpy, config, EGL_NO_CONTEXT, ctx_attribs);
	surf = eglCreateWindowSurface(dpy, config,
				      (EGLNativeWindowType)surface, NULL);
	if (ctx == EGL_NO_CONTEXT || surf == EGL_NO_SURFACE ||
	    !eglMakeCurrent(dpy, surf, surf, ctx))
		die("cannot make an EGL context current");
	printf("%s: GL renderer %s\n", prog, glGetString(GL_RENDERER));

	start = 0;
	period = 1.0 / mode->vrefresh;
	for (i = 0; i < frames; i++) {
		frame = now();
		glClearColor((i % 60) / 60.0f, 0.2f, 1.0f - (i % 60) / 60.0f,
			     1.0f);
		glClear(GL_COLOR_BUFFER_BIT);
		if (!eglSwapBuffers(dpy, surf))
			die("eglSwapBuffers failed");
		bo = gbm_surface_lock_front_buffer(surface);
		if (!bo)
			die("gbm_surface_lock_front_buffer failed");

		if (!i) {
			if (drmModeSetCrtc(fd, crtc, bo_fb(fd, bo), 0, 0,
					   &conn->connector_id, 1, mode))
				die("drmModeSetCrtc failed");
			start = now();
		} else {
			t = now();
			render += t - frame;
			f.pending = 1;
			if (drmModePageFlip(fd, crtc, bo_fb(fd, bo),
					    DRM_MODE_PAGE_FLIP_EVENT, &f))
				die("drmModePageFlip failed");
			while (f.pending) {
				struct pollfd p = { .fd = fd, .events = POLLIN };

				if (poll(&p, 1, 1000) <= 0)
					die("no flip event within 1 s");
				drmHandleEvent(fd, &ev);
			}
			flip += now() - t;
			/* The first interval follows the modeset, not a flip. */
			if (last) {
				j = (int)((f.time - last) / period + 0.5);
				periods[j < 1 ? 0 : j > 3 ? 3 : j]++;
			}
			last = f.time;
		}
		if (prev)
			gbm_surface_release_buffer(surface, prev);
		prev = bo;
	}
	t = now() - start;
	printf("%s: %d flips in %.3f s, %.1f Hz\n", prog, frames - 1, t,
	       (frames - 1) / t);
	printf("%s: render %.2f ms, flip %.2f ms on average\n", prog,
	       render * 1e3 / (frames - 1), flip * 1e3 / (frames - 1));
	printf("%s: flips after 0, 1, 2 and 3 or more periods: %u %u %u %u\n",
	       prog, periods[0], periods[1], periods[2], periods[3]);
	return 0;
}

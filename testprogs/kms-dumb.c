// SPDX-License-Identifier: GPL-2.0
/*
 * Show a dumb buffer on the first connected connector of a DRM card, in its
 * preferred mode, and redraw part of it for each frame. A white bar moves
 * across a gradient, and each frame marks the bar's old and new columns
 * dirty, which a virtio-gpu card transfers to its device. A 64x64 dumb
 * cursor, a white square with a black border, moves with the bar. The frames
 * come at the mode's refresh rate.
 *
 * usage: kms-dumb <card> <frames>
 */
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <time.h>
#include <unistd.h>

#include <drm_fourcc.h>
#include <xf86drm.h>
#include <xf86drmMode.h>

#define BAR 64
#define CURSOR 64

static const char *prog = "kms-dumb";

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

/* A mapped dumb buffer of 32-bit pixels. */
struct dumb {
	uint32_t handle, pitch;
	uint64_t size;
	uint32_t *pixels;
};

static void dumb_create(int fd, struct dumb *b, uint32_t w, uint32_t h)
{
	uint64_t offset;

	if (drmModeCreateDumbBuffer(fd, w, h, 32, 0, &b->handle, &b->pitch,
				    &b->size))
		die("drmModeCreateDumbBuffer failed");
	if (drmModeMapDumbBuffer(fd, b->handle, &offset))
		die("drmModeMapDumbBuffer failed");
	b->pixels = mmap(NULL, b->size, PROT_READ | PROT_WRITE, MAP_SHARED,
			 fd, offset);
	if (b->pixels == MAP_FAILED)
		die("cannot map the dumb buffer");
}

/* Draw the gradient in columns x to x + w, or the bar. */
static void draw(struct dumb *b, uint32_t h, uint32_t x, uint32_t w, int bar)
{
	uint32_t i, j, *row;

	for (j = 0; j < h; j++) {
		row = b->pixels + j * (b->pitch / 4);
		for (i = x; i < x + w; i++)
			row[i] = bar ? 0xffffff : ((i & 0xff) << 16) |
						  ((j & 0xff) << 8) | 0x80;
	}
}

int main(int argc, char **argv)
{
	drmModeConnector *conn = NULL;
	drmModeModeInfo *mode;
	drmModeEncoder *enc;
	drmModeRes *res;
	struct dumb fb, cursor;
	uint32_t crtc = 0, fb_id, handles[4] = { 0 }, pitches[4] = { 0 };
	uint32_t offsets[4] = { 0 }, w, h, x, last = 0;
	struct timespec next;
	double start, dirty = 0, t;
	int fd, frames, i, j;
	long period;

	setvbuf(stdout, NULL, _IOLBF, 0);
	if (argc != 3 || (frames = atoi(argv[2])) < 1) {
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
	w = mode->hdisplay;
	h = mode->vdisplay;
	if (w <= BAR || h <= CURSOR || !mode->vrefresh)
		die("the mode is too small");
	printf("%s: connector %u, %ux%u@%u\n", prog, conn->connector_id, w, h,
	       mode->vrefresh);

	dumb_create(fd, &fb, w, h);
	draw(&fb, h, 0, w, 0);
	handles[0] = fb.handle;
	pitches[0] = fb.pitch;
	if (drmModeAddFB2(fd, w, h, DRM_FORMAT_XRGB8888, handles, pitches,
			  offsets, &fb_id, 0))
		die("drmModeAddFB2 failed");
	if (drmModeSetCrtc(fd, crtc, fb_id, 0, 0, &conn->connector_id, 1,
			   mode))
		die("drmModeSetCrtc failed");

	dumb_create(fd, &cursor, CURSOR, CURSOR);
	for (j = 0; j < CURSOR; j++) {
		for (i = 0; i < CURSOR; i++) {
			int edge = i < 4 || i >= CURSOR - 4 || j < 4 ||
				   j >= CURSOR - 4;

			cursor.pixels[j * (cursor.pitch / 4) + i] =
				edge ? 0xff000000 : 0xffffffff;
		}
	}
	if (drmModeSetCursor(fd, crtc, cursor.handle, CURSOR, CURSOR))
		die("drmModeSetCursor failed");

	period = 1000000000L / mode->vrefresh;
	clock_gettime(CLOCK_MONOTONIC, &next);
	start = now();
	for (i = 0; i < frames; i++) {
		drmModeClip clips[2];

		x = (i * 16) % (w - BAR);
		draw(&fb, h, last, BAR, 0);
		draw(&fb, h, x, BAR, 1);
		clips[0] = (drmModeClip){ last, 0, last + BAR, h };
		clips[1] = (drmModeClip){ x, 0, x + BAR, h };
		last = x;
		t = now();
		if (drmModeDirtyFB(fd, fb_id, clips, 2))
			die("drmModeDirtyFB failed");
		dirty += now() - t;
		if (drmModeMoveCursor(fd, crtc, x, (i * 4) % (h - CURSOR)))
			die("drmModeMoveCursor failed");

		next.tv_nsec += period;
		if (next.tv_nsec >= 1000000000L) {
			next.tv_nsec -= 1000000000L;
			next.tv_sec++;
		}
		clock_nanosleep(CLOCK_MONOTONIC, TIMER_ABSTIME, &next, NULL);
	}
	t = now() - start;
	printf("%s: %d frames in %.3f s, %.1f Hz\n", prog, frames, t,
	       frames / t);
	printf("%s: dirty %.2f ms on average\n", prog, dirty * 1e3 / frames);
	return 0;
}

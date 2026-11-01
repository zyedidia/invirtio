// SPDX-License-Identifier: GPL-2.0
/*
 * Measure how fast the CPU writes, reads and copies into a host-visible
 * buffer, with Vulkan on the device of a DRM render node. With vram, the
 * buffer is in CPU-visible VRAM, and otherwise in GTT. The buffer is filled
 * once before the timed passes, so that the faults that map it are not
 * timed, and each operation reports its best pass in MiB/s.
 *
 * usage: vk-bandwidth <render node> [vram]
 */
#include <string.h>
#include <time.h>

#include "vkutil.h"

#define SIZE (64ull << 20)
#define WORDS (SIZE / sizeof(uint64_t))
#define PASSES 3

static double now(void)
{
	struct timespec ts;

	clock_gettime(CLOCK_MONOTONIC, &ts);
	return ts.tv_sec + ts.tv_nsec / 1e9;
}

static void write_words(volatile uint64_t *p, uint64_t value)
{
	size_t i;

	for (i = 0; i < WORDS; i++)
		p[i] = value + i;
}

static void read_words(const volatile uint64_t *p)
{
	size_t i;

	for (i = 0; i < WORDS; i++)
		(void)p[i];
}

int main(int argc, char **argv)
{
	double best[3] = { 0 }, t;
	static const char *const ops[] = { "write", "read", "copy" };
	VkMemoryRequirements req;
	VkDeviceMemory mem;
	struct gpu g;
	VkBuffer b;
	uint64_t *src;
	void *map;
	int vram, pass, op;

	setvbuf(stdout, NULL, _IOLBF, 0);
	prog = "vk-bandwidth";
	role = "main";
	vram = argc == 3 && !strcmp(argv[2], "vram");
	if (argc != 2 && !vram) {
		fprintf(stderr, "usage: %s <render node> [vram]\n", prog);
		return 2;
	}
	src = malloc(SIZE);
	if (!src) {
		perror("malloc");
		return 1;
	}
	memset(src, 0x5a, SIZE);
	gpu_init(&g, argv[1], NULL, 0);

	VkBufferCreateInfo bci = {
		.sType = VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO,
		.size = SIZE,
		.usage = VK_BUFFER_USAGE_TRANSFER_DST_BIT,
		.sharingMode = VK_SHARING_MODE_EXCLUSIVE,
	};
	check(vkCreateBuffer(g.dev, &bci, NULL, &b), "vkCreateBuffer");
	vkGetBufferMemoryRequirements(g.dev, b, &req);
	VkMemoryAllocateInfo mai = {
		.sType = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,
		.allocationSize = req.size,
		.memoryTypeIndex = memory_type(&g, req.memoryTypeBits,
					       host_visible_flags(vram), 1),
	};
	check(vkAllocateMemory(g.dev, &mai, NULL, &mem), "vkAllocateMemory");
	check(vkBindBufferMemory(g.dev, b, mem, 0), "vkBindBufferMemory");
	check(vkMapMemory(g.dev, mem, 0, SIZE, 0, &map), "vkMapMemory");
	write_words(map, 0);

	for (pass = 0; pass < PASSES; pass++) {
		for (op = 0; op < 3; op++) {
			t = now();
			if (op == 0)
				write_words(map, pass);
			else if (op == 1)
				read_words(map);
			else
				memcpy(map, src, SIZE);
			t = now() - t;
			if ((SIZE >> 20) / t > best[op])
				best[op] = (SIZE >> 20) / t;
		}
	}
	for (op = 0; op < 3; op++)
		printf("%s: main: %s %s %.0f MiB/s\n", prog,
		       vram ? "vram" : "gtt", ops[op], best[op]);

	vkUnmapMemory(g.dev, mem);
	vkDestroyBuffer(g.dev, b, NULL);
	vkFreeMemory(g.dev, mem, NULL);
	gpu_destroy(&g);
	free(src);
	return 0;
}

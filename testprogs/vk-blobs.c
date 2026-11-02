// SPDX-License-Identifier: GPL-2.0
/*
 * Allocate and map host-visible buffers with Vulkan on the device of a DRM
 * render node, and exit without freeing them, so that the process's exit
 * frees them all at once.
 *
 * usage: vk-blobs <render node> <buffers>
 */
#include "vkutil.h"

#define SIZE (1ull << 20)

int main(int argc, char **argv)
{
	VkDeviceMemory mem;
	struct gpu g;
	int n, i;
	void *map;

	setvbuf(stdout, NULL, _IOLBF, 0);
	prog = "vk-blobs";
	role = "main";
	if (argc != 3 || (n = atoi(argv[2])) < 1) {
		fprintf(stderr, "usage: %s <render node> <buffers>\n", prog);
		return 2;
	}
	gpu_init(&g, argv[1], NULL, 0);
	for (i = 0; i < n; i++) {
		VkMemoryAllocateInfo mai = {
			.sType = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,
			.allocationSize = SIZE,
			.memoryTypeIndex = memory_type(&g, ~0u,
						       host_visible_flags(0), 1),
		};

		check(vkAllocateMemory(g.dev, &mai, NULL, &mem),
		      "vkAllocateMemory");
		check(vkMapMemory(g.dev, mem, 0, SIZE, 0, &map),
		      "vkMapMemory");
		*(volatile uint32_t *)map = i;
	}
	printf("%s: main: %d buffers of %llu KiB\n", prog, n, SIZE >> 10);
	return 0;
}

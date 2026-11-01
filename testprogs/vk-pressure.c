// SPDX-License-Identifier: GPL-2.0
/*
 * Use more host-visible memory than the GTT of the GPU's driver holds, or with
 * vram, than its CPU-visible VRAM, with Vulkan on the device of a DRM render
 * node, so that the driver evicts buffers to make room for the ones in use.
 * The program allocates buffers of BUF bytes until they hold a quarter more
 * than the size that it is given, fills each through its mapping, and in
 * each of PASSES passes checks and refills every buffer through its mapping.
 * The buffers are only accessed by the CPU meanwhile, since a submission
 * needs every buffer of the device resident. The program then frees all but
 * the first buffer, fills its second half on the GPU and checks it.
 *
 * usage: vk-pressure <render node> <size in bytes> [vram]
 */
#include <string.h>

#include "vkutil.h"

#define BUF (64ull << 20)
#define WORDS (BUF / sizeof(uint64_t))
#define PASSES 3
#define MAX_BUFS 64

static uint64_t pattern(unsigned int pass, unsigned int buf, size_t i)
{
	return 0x5a00000000000000ull ^ ((uint64_t)pass << 48) ^
	       ((uint64_t)buf << 32) ^ i;
}

static void fill(volatile uint64_t *p, unsigned int pass, unsigned int buf,
		 size_t n)
{
	size_t i;

	for (i = 0; i < n; i++)
		p[i] = pattern(pass, buf, i);
}

static size_t differ(const volatile uint64_t *p, unsigned int pass,
		     unsigned int buf, size_t n)
{
	size_t i, bad = 0;

	for (i = 0; i < n; i++)
		bad += p[i] != pattern(pass, buf, i);
	return bad;
}

/* The arguments of the recorded fill. */
struct work {
	VkBuffer b;
	uint32_t value;
};

/* Fill the buffer's second half and make it visible to the host. */
static void record_fill(VkCommandBuffer cmd, void *arg)
{
	struct work *w = arg;

	vkCmdFillBuffer(cmd, w->b, BUF / 2, BUF / 2, w->value);
	barrier(cmd, w->b, VK_PIPELINE_STAGE_TRANSFER_BIT,
		VK_ACCESS_TRANSFER_WRITE_BIT, VK_PIPELINE_STAGE_HOST_BIT,
		VK_ACCESS_HOST_READ_BIT, VK_QUEUE_FAMILY_IGNORED,
		VK_QUEUE_FAMILY_IGNORED);
}

int main(int argc, char **argv)
{
	VkDeviceMemory mem[MAX_BUFS];
	volatile uint64_t *p[MAX_BUFS];
	VkMemoryRequirements req;
	unsigned long long size;
	int vram;
	unsigned int n, i, pass;
	const volatile uint32_t *q;
	uint32_t type;
	struct gpu g;
	VkBuffer b;
	size_t bad;

	setvbuf(stdout, NULL, _IOLBF, 0);
	prog = "vk-pressure";
	role = "main";
	vram = argc == 4 && !strcmp(argv[3], "vram");
	if ((argc != 3 && !vram) || !(size = strtoull(argv[2], NULL, 0))) {
		fprintf(stderr, "usage: %s <render node> <size in bytes> [vram]\n",
			prog);
		return 2;
	}
	n = (size + size / 4) / BUF + 1;
	if (n > MAX_BUFS) {
		fprintf(stderr, "%s: main: a size of %llu MiB needs more than %d buffers\n",
			prog, size >> 20, MAX_BUFS);
		return 1;
	}
	gpu_init(&g, argv[1], NULL, 0);

	VkBufferCreateInfo bci = {
		.sType = VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO,
		.size = BUF,
		.usage = VK_BUFFER_USAGE_TRANSFER_DST_BIT,
		.sharingMode = VK_SHARING_MODE_EXCLUSIVE,
	};
	check(vkCreateBuffer(g.dev, &bci, NULL, &b), "vkCreateBuffer");
	vkGetBufferMemoryRequirements(g.dev, b, &req);
	type = memory_type(&g, req.memoryTypeBits, host_visible_flags(vram), 1);
	printf("%s: main: %u buffers of %llu MiB for %llu MiB of %s\n", prog, n,
	       BUF >> 20, size >> 20, vram ? "visible VRAM" : "GTT");

	for (i = 0; i < n; i++) {
		VkMemoryAllocateInfo mai = {
			.sType = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,
			.allocationSize = req.size,
			.memoryTypeIndex = type,
		};

		check(vkAllocateMemory(g.dev, &mai, NULL, &mem[i]),
		      "vkAllocateMemory");
		check(vkMapMemory(g.dev, mem[i], 0, BUF, 0, (void **)&p[i]),
		      "vkMapMemory");
		fill(p[i], 0, i, WORDS);
	}
	check(vkBindBufferMemory(g.dev, b, mem[0], 0), "vkBindBufferMemory");

	for (pass = 0; pass < PASSES; pass++) {
		for (i = 0; i < n; i++) {
			bad = differ(p[i], pass, i, WORDS);
			if (bad) {
				printf("%s: main: pass %u: %zu of %llu words of buffer %u differ\n",
				       prog, pass, bad, WORDS, i);
				return 1;
			}
			fill(p[i], pass + 1, i, WORDS);
		}
		printf("%s: main: pass %u: the buffers hold their patterns\n",
		       prog, pass);
	}

	for (i = 1; i < n; i++)
		vkFreeMemory(g.dev, mem[i], NULL);
	struct work w = { .b = b, .value = 0xa5a5a5a5u };

	run(&g, record_fill, &w);
	bad = differ(p[0], PASSES, 0, WORDS / 2);
	q = (const volatile uint32_t *)(p[0] + WORDS / 2);
	for (i = 0; i < BUF / 2 / sizeof(*q); i++)
		bad += q[i] != w.value;
	if (bad) {
		printf("%s: main: %zu words of buffer 0 differ after the GPU's fill\n",
		       prog, bad);
		return 1;
	}
	printf("%s: main: the GPU's fill and the last pattern are in buffer 0\n",
	       prog);

	vkDestroyBuffer(g.dev, b, NULL);
	vkFreeMemory(g.dev, mem[0], NULL);
	gpu_destroy(&g);
	return 0;
}

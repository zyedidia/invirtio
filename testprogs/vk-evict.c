// SPDX-License-Identifier: GPL-2.0
/*
 * Use a host-visible buffer across evictions of its memory, with Vulkan on
 * the device of a DRM render node. The program fills the buffer through its
 * mapping and then serves one round for each line on stdin, which comes
 * after its memory was evicted. A round checks the buffer through the
 * mapping, refills its first half through the mapping and its second half on
 * the GPU, and checks it again. The program prints a line that ends in
 * "ready" before each round and exits at the end of stdin.
 *
 * usage: vk-evict <render node>
 */
#include "vkutil.h"

#define SIZE (4 << 20)
#define WORDS (SIZE / sizeof(uint32_t))

static uint32_t pattern(unsigned int round, size_t i)
{
	return 0x5a000000u ^ (round << 16) ^ (uint32_t)i;
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

	vkCmdFillBuffer(cmd, w->b, SIZE / 2, SIZE / 2, w->value);
	barrier(cmd, w->b, VK_PIPELINE_STAGE_TRANSFER_BIT,
		VK_ACCESS_TRANSFER_WRITE_BIT, VK_PIPELINE_STAGE_HOST_BIT,
		VK_ACCESS_HOST_READ_BIT, VK_QUEUE_FAMILY_IGNORED,
		VK_QUEUE_FAMILY_IGNORED);
}

/*
 * Return the number of words that differ from the pattern of round in the
 * first half, or from value in the second half.
 */
static size_t differ(const volatile uint32_t *p, unsigned int round,
		     uint32_t value)
{
	size_t i, bad = 0;

	for (i = 0; i < WORDS / 2; i++)
		bad += p[i] != pattern(round, i);
	for (; i < WORDS; i++)
		bad += p[i] != value;
	return bad;
}

int main(int argc, char **argv)
{
	VkMemoryRequirements req;
	VkDeviceMemory mem;
	struct gpu g;
	VkBuffer b;
	volatile uint32_t *p;
	unsigned int round = 0;
	uint32_t value;
	char line[64];
	size_t i, bad;

	setvbuf(stdout, NULL, _IOLBF, 0);
	prog = "vk-evict";
	role = "main";
	if (argc != 2) {
		fprintf(stderr, "usage: %s <render node>\n", prog);
		return 2;
	}
	gpu_init(&g, argv[1], NULL, 0);

	VkBufferCreateInfo bci = {
		.sType = VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO,
		.size = SIZE,
		.usage = VK_BUFFER_USAGE_TRANSFER_DST_BIT,
		.sharingMode = VK_SHARING_MODE_EXCLUSIVE,
	};
	check(vkCreateBuffer(g.dev, &bci, NULL, &b), "vkCreateBuffer");
	vkGetBufferMemoryRequirements(g.dev, b, &req);
	/* RADV places cached host-visible memory in GTT. */
	VkMemoryAllocateInfo mai = {
		.sType = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,
		.allocationSize = req.size,
		.memoryTypeIndex = memory_type(&g, req.memoryTypeBits,
					       VK_MEMORY_PROPERTY_HOST_VISIBLE_BIT |
					       VK_MEMORY_PROPERTY_HOST_COHERENT_BIT |
					       VK_MEMORY_PROPERTY_HOST_CACHED_BIT,
					       1),
	};
	check(vkAllocateMemory(g.dev, &mai, NULL, &mem), "vkAllocateMemory");
	check(vkBindBufferMemory(g.dev, b, mem, 0), "vkBindBufferMemory");
	check(vkMapMemory(g.dev, mem, 0, SIZE, 0, (void **)&p), "vkMapMemory");

	for (i = 0; i < WORDS / 2; i++)
		p[i] = pattern(round, i);
	value = pattern(round, WORDS);
	for (; i < WORDS; i++)
		p[i] = value;

	for (;;) {
		printf("%s: round %u ready\n", prog, round);
		if (!fgets(line, sizeof(line), stdin))
			break;

		bad = differ(p, round, value);
		if (bad) {
			printf("%s: round %u: %zu of %zu words differ after the eviction\n",
			       prog, round, bad, WORDS);
			return 1;
		}

		round++;
		for (i = 0; i < WORDS / 2; i++)
			p[i] = pattern(round, i);
		struct work w = { .b = b, .value = pattern(round, WORDS) };

		run(&g, record_fill, &w);
		value = w.value;
		bad = differ(p, round, value);
		if (bad) {
			printf("%s: round %u: %zu of %zu words differ after the GPU's fill\n",
			       prog, round, bad, WORDS);
			return 1;
		}
		printf("%s: round %u: the buffer holds its patterns\n", prog,
		       round);
	}

	vkUnmapMemory(g.dev, mem);
	vkDestroyBuffer(g.dev, b, NULL);
	vkFreeMemory(g.dev, mem, NULL);
	gpu_destroy(&g);
	return 0;
}

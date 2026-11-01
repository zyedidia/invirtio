// SPDX-License-Identifier: GPL-2.0
/*
 * Share a buffer between two processes through a dma-buf, with Vulkan on
 * the device of a DRM render node. The exporter fills the buffer on the GPU and exports
 * its memory, and an importer, which is a new process with its own Vulkan
 * device, imports the memory, copies it on the GPU into a host-visible
 * buffer and checks it. The program runs the importer itself, by executing
 * itself again with the dma-buf's fd, since a forked process cannot use its
 * parent's Vulkan objects.
 *
 * usage: vk-dmabuf-share <render node>
 */
#include <fcntl.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

#include "vkutil.h"

#define SIZE (1 << 20)
#define PATTERN 0x5a5a1234u

static const char *const exts[] = {
	VK_KHR_EXTERNAL_MEMORY_FD_EXTENSION_NAME,
	VK_EXT_EXTERNAL_MEMORY_DMA_BUF_EXTENSION_NAME,
};
static const char *node;

static VkBuffer buffer(struct gpu *g, VkBufferUsageFlags usage, int external)
{
	VkExternalMemoryBufferCreateInfo ext = {
		.sType = VK_STRUCTURE_TYPE_EXTERNAL_MEMORY_BUFFER_CREATE_INFO,
		.handleTypes = VK_EXTERNAL_MEMORY_HANDLE_TYPE_DMA_BUF_BIT_EXT,
	};
	VkBufferCreateInfo bci = {
		.sType = VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO,
		.pNext = external ? &ext : NULL,
		.size = SIZE,
		.usage = usage,
		.sharingMode = VK_SHARING_MODE_EXCLUSIVE,
	};
	VkBuffer b;

	check(vkCreateBuffer(g->dev, &bci, NULL, &b), "vkCreateBuffer");
	return b;
}

/* The arguments of the recorded commands. */
struct work {
	struct gpu *g;
	VkBuffer src, dst;
};

/* Fill the buffer and release it to the importer's queue family. */
static void record_fill(VkCommandBuffer cmd, void *arg)
{
	struct work *w = arg;

	vkCmdFillBuffer(cmd, w->dst, 0, SIZE, PATTERN);
	barrier(cmd, w->dst, VK_PIPELINE_STAGE_TRANSFER_BIT,
		VK_ACCESS_TRANSFER_WRITE_BIT, VK_PIPELINE_STAGE_BOTTOM_OF_PIPE_BIT,
		0, w->g->family, VK_QUEUE_FAMILY_EXTERNAL);
}

/*
 * Acquire the imported buffer from the exporter's queue family, copy it,
 * and make the copy visible to the host.
 */
static void record_copy(VkCommandBuffer cmd, void *arg)
{
	struct work *w = arg;
	VkBufferCopy region = { .size = SIZE };

	barrier(cmd, w->src, VK_PIPELINE_STAGE_TOP_OF_PIPE_BIT, 0,
		VK_PIPELINE_STAGE_TRANSFER_BIT, VK_ACCESS_TRANSFER_READ_BIT,
		VK_QUEUE_FAMILY_EXTERNAL, w->g->family);
	vkCmdCopyBuffer(cmd, w->src, w->dst, 1, &region);
	barrier(cmd, w->dst, VK_PIPELINE_STAGE_TRANSFER_BIT,
		VK_ACCESS_TRANSFER_WRITE_BIT, VK_PIPELINE_STAGE_HOST_BIT,
		VK_ACCESS_HOST_READ_BIT, VK_QUEUE_FAMILY_IGNORED,
		VK_QUEUE_FAMILY_IGNORED);
}

/*
 * Import the dma-buf, copy it on the GPU into a host-visible buffer and
 * check that every word holds the pattern.
 */
static int import(int fd)
{
	PFN_vkGetMemoryFdPropertiesKHR get_props;
	VkMemoryFdPropertiesKHR fdp = {
		.sType = VK_STRUCTURE_TYPE_MEMORY_FD_PROPERTIES_KHR,
	};
	VkMemoryRequirements req;
	VkDeviceMemory src_mem, dst_mem;
	VkBuffer b[2];
	struct gpu g;
	uint32_t *p;
	size_t i, bad = 0;

	role = "importer";
	gpu_init(&g, node, exts, sizeof(exts) / sizeof(exts[0]));
	get_props = (PFN_vkGetMemoryFdPropertiesKHR)vkGetDeviceProcAddr(
		g.dev, "vkGetMemoryFdPropertiesKHR");
	if (!get_props)
		die("vkGetDeviceProcAddr", VK_ERROR_EXTENSION_NOT_PRESENT);
	check(get_props(g.dev, VK_EXTERNAL_MEMORY_HANDLE_TYPE_DMA_BUF_BIT_EXT,
			fd, &fdp), "vkGetMemoryFdPropertiesKHR");

	b[0] = buffer(&g, VK_BUFFER_USAGE_TRANSFER_SRC_BIT, 1);
	vkGetBufferMemoryRequirements(g.dev, b[0], &req);
	VkMemoryDedicatedAllocateInfo dedicated = {
		.sType = VK_STRUCTURE_TYPE_MEMORY_DEDICATED_ALLOCATE_INFO,
		.buffer = b[0],
	};
	VkImportMemoryFdInfoKHR imp = {
		.sType = VK_STRUCTURE_TYPE_IMPORT_MEMORY_FD_INFO_KHR,
		.pNext = &dedicated,
		.handleType = VK_EXTERNAL_MEMORY_HANDLE_TYPE_DMA_BUF_BIT_EXT,
		.fd = fd,
	};
	VkMemoryAllocateInfo mai = {
		.sType = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,
		.pNext = &imp,
		.allocationSize = req.size,
		.memoryTypeIndex = memory_type(&g,
					       req.memoryTypeBits &
					       fdp.memoryTypeBits, 0, 1),
	};
	check(vkAllocateMemory(g.dev, &mai, NULL, &src_mem),
	      "importing the dma-buf");
	check(vkBindBufferMemory(g.dev, b[0], src_mem, 0),
	      "vkBindBufferMemory");

	b[1] = buffer(&g, VK_BUFFER_USAGE_TRANSFER_DST_BIT, 0);
	vkGetBufferMemoryRequirements(g.dev, b[1], &req);
	VkMemoryAllocateInfo dai = {
		.sType = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,
		.allocationSize = req.size,
		.memoryTypeIndex = memory_type(&g, req.memoryTypeBits,
					       VK_MEMORY_PROPERTY_HOST_VISIBLE_BIT |
					       VK_MEMORY_PROPERTY_HOST_COHERENT_BIT,
					       1),
	};
	check(vkAllocateMemory(g.dev, &dai, NULL, &dst_mem), "vkAllocateMemory");
	check(vkBindBufferMemory(g.dev, b[1], dst_mem, 0),
	      "vkBindBufferMemory");

	struct work w = { .g = &g, .src = b[0], .dst = b[1] };

	run(&g, record_copy, &w);
	check(vkMapMemory(g.dev, dst_mem, 0, SIZE, 0, (void **)&p),
	      "vkMapMemory");
	for (i = 0; i < SIZE / sizeof(*p); i++)
		bad += p[i] != PATTERN;
	vkUnmapMemory(g.dev, dst_mem);

	vkDestroyBuffer(g.dev, b[0], NULL);
	vkDestroyBuffer(g.dev, b[1], NULL);
	vkFreeMemory(g.dev, src_mem, NULL);
	vkFreeMemory(g.dev, dst_mem, NULL);
	gpu_destroy(&g);

	if (bad) {
		printf("%s: importer: %zu of %zu words differ from the pattern\n",
		       prog, bad, SIZE / sizeof(*p));
		return 1;
	}
	printf("%s: importer: the buffer holds the exporter's pattern\n", prog);
	return 0;
}

/*
 * Fill an exportable buffer on the GPU, export its memory as a dma-buf and
 * run the importer on it.
 */
static int export(const char *self)
{
	PFN_vkGetMemoryFdKHR get_fd;
	VkMemoryRequirements req;
	VkDeviceMemory mem;
	char arg[16];
	struct gpu g;
	VkBuffer b;
	int fd, status;
	pid_t pid;

	gpu_init(&g, node, exts, sizeof(exts) / sizeof(exts[0]));
	b = buffer(&g, VK_BUFFER_USAGE_TRANSFER_DST_BIT |
		       VK_BUFFER_USAGE_TRANSFER_SRC_BIT, 1);
	vkGetBufferMemoryRequirements(g.dev, b, &req);
	VkMemoryDedicatedAllocateInfo dedicated = {
		.sType = VK_STRUCTURE_TYPE_MEMORY_DEDICATED_ALLOCATE_INFO,
		.buffer = b,
	};
	VkExportMemoryAllocateInfo exp = {
		.sType = VK_STRUCTURE_TYPE_EXPORT_MEMORY_ALLOCATE_INFO,
		.pNext = &dedicated,
		.handleTypes = VK_EXTERNAL_MEMORY_HANDLE_TYPE_DMA_BUF_BIT_EXT,
	};
	VkMemoryAllocateInfo mai = {
		.sType = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,
		.pNext = &exp,
		.allocationSize = req.size,
		.memoryTypeIndex = memory_type(&g, req.memoryTypeBits,
					       VK_MEMORY_PROPERTY_DEVICE_LOCAL_BIT,
					       0),
	};
	check(vkAllocateMemory(g.dev, &mai, NULL, &mem), "vkAllocateMemory");
	check(vkBindBufferMemory(g.dev, b, mem, 0), "vkBindBufferMemory");
	struct work w = { .g = &g, .dst = b };

	run(&g, record_fill, &w);

	get_fd = (PFN_vkGetMemoryFdKHR)vkGetDeviceProcAddr(g.dev,
							   "vkGetMemoryFdKHR");
	if (!get_fd)
		die("vkGetDeviceProcAddr", VK_ERROR_EXTENSION_NOT_PRESENT);
	VkMemoryGetFdInfoKHR gfi = {
		.sType = VK_STRUCTURE_TYPE_MEMORY_GET_FD_INFO_KHR,
		.memory = mem,
		.handleType = VK_EXTERNAL_MEMORY_HANDLE_TYPE_DMA_BUF_BIT_EXT,
	};
	check(get_fd(g.dev, &gfi, &fd), "exporting the memory");
	printf("%s: exporter: filled and exported the buffer\n", prog);
	fflush(stdout);

	snprintf(arg, sizeof(arg), "%d", fd);
	pid = fork();
	if (pid < 0) {
		perror("fork");
		return 1;
	}
	if (pid == 0) {
		/* The exported fd is close-on-exec, and the importer needs it. */
		if (fcntl(fd, F_SETFD, 0) < 0) {
			perror("fcntl");
			_exit(1);
		}
		execl(self, self, node, "--import", arg, (char *)NULL);
		perror("execl");
		_exit(1);
	}
	close(fd);
	if (waitpid(pid, &status, 0) < 0) {
		perror("waitpid");
		return 1;
	}

	vkDestroyBuffer(g.dev, b, NULL);
	vkFreeMemory(g.dev, mem, NULL);
	gpu_destroy(&g);
	return WIFEXITED(status) ? WEXITSTATUS(status) : 1;
}

int main(int argc, char **argv)
{
	/* Both processes write to one pipe, in the order they run. */
	setvbuf(stdout, NULL, _IOLBF, 0);
	prog = "vk-dmabuf-share";
	role = "exporter";
	if (argc == 4 && !strcmp(argv[2], "--import")) {
		node = argv[1];
		return import(atoi(argv[3]));
	}
	if (argc != 2) {
		fprintf(stderr, "usage: %s <render node>\n", prog);
		return 2;
	}
	node = argv[1];
	return export("/proc/self/exe");
}

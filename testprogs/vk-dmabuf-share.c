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
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/sysmacros.h>
#include <sys/wait.h>
#include <unistd.h>
#include <vulkan/vulkan.h>

#define SIZE (1 << 20)
#define PATTERN 0x5a5a1234u

static const char *prog = "vk-dmabuf-share";
static const char *role = "exporter";
static const char *node;

struct gpu {
	VkInstance instance;
	VkPhysicalDevice phys;
	VkDevice dev;
	VkQueue queue;
	uint32_t family;
	VkCommandPool pool;
};

static void die(const char *msg, VkResult r)
{
	fprintf(stderr, "%s: %s: %s failed: %d\n", prog, role, msg, r);
	exit(1);
}

static void check(VkResult r, const char *msg)
{
	if (r != VK_SUCCESS)
		die(msg, r);
}

static void gpu_init(struct gpu *g)
{
	static const char *const exts[] = {
		VK_KHR_EXTERNAL_MEMORY_FD_EXTENSION_NAME,
		VK_EXT_EXTERNAL_MEMORY_DMA_BUF_EXTENSION_NAME,
	};
	VkApplicationInfo app = {
		.sType = VK_STRUCTURE_TYPE_APPLICATION_INFO,
		.pApplicationName = prog,
		.apiVersion = VK_API_VERSION_1_1,
	};
	VkInstanceCreateInfo ici = {
		.sType = VK_STRUCTURE_TYPE_INSTANCE_CREATE_INFO,
		.pApplicationInfo = &app,
	};
	VkPhysicalDeviceDrmPropertiesEXT drm = {
		.sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_DRM_PROPERTIES_EXT,
	};
	VkPhysicalDeviceProperties2 props = {
		.sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_PROPERTIES_2,
		.pNext = &drm,
	};
	VkPhysicalDevice devs[16];
	VkQueueFamilyProperties families[16];
	uint32_t n = 16, nfam = 16, i;
	float prio = 1.0f;
	struct stat st;

	if (stat(node, &st) < 0 || !S_ISCHR(st.st_mode)) {
		fprintf(stderr, "%s: %s: %s is not a device node\n", prog, role,
			node);
		exit(1);
	}
	check(vkCreateInstance(&ici, NULL, &g->instance), "vkCreateInstance");
	vkEnumeratePhysicalDevices(g->instance, &n, devs);
	g->phys = VK_NULL_HANDLE;
	for (i = 0; i < n && !g->phys; i++) {
		drm.hasRender = VK_FALSE;
		vkGetPhysicalDeviceProperties2(devs[i], &props);
		if (drm.hasRender && drm.renderMajor == major(st.st_rdev) &&
		    drm.renderMinor == minor(st.st_rdev))
			g->phys = devs[i];
	}
	if (!g->phys) {
		fprintf(stderr, "%s: %s: no Vulkan device has the render node %s\n",
			prog, role, node);
		exit(1);
	}
	printf("%s: %s: %s on %s\n", prog, role, props.properties.deviceName,
	       node);

	vkGetPhysicalDeviceQueueFamilyProperties(g->phys, &nfam, families);
	for (i = 0; i < nfam; i++) {
		if (families[i].queueFlags &
		    (VK_QUEUE_GRAPHICS_BIT | VK_QUEUE_COMPUTE_BIT))
			break;
	}
	if (i == nfam)
		die("finding a queue", VK_ERROR_INITIALIZATION_FAILED);
	g->family = i;

	VkDeviceQueueCreateInfo qci = {
		.sType = VK_STRUCTURE_TYPE_DEVICE_QUEUE_CREATE_INFO,
		.queueFamilyIndex = g->family,
		.queueCount = 1,
		.pQueuePriorities = &prio,
	};
	VkDeviceCreateInfo dci = {
		.sType = VK_STRUCTURE_TYPE_DEVICE_CREATE_INFO,
		.queueCreateInfoCount = 1,
		.pQueueCreateInfos = &qci,
		.enabledExtensionCount = sizeof(exts) / sizeof(exts[0]),
		.ppEnabledExtensionNames = exts,
	};
	check(vkCreateDevice(g->phys, &dci, NULL, &g->dev), "vkCreateDevice");
	vkGetDeviceQueue(g->dev, g->family, 0, &g->queue);

	VkCommandPoolCreateInfo pci = {
		.sType = VK_STRUCTURE_TYPE_COMMAND_POOL_CREATE_INFO,
		.queueFamilyIndex = g->family,
	};
	check(vkCreateCommandPool(g->dev, &pci, NULL, &g->pool),
	      "vkCreateCommandPool");
}

/*
 * Return a memory type among bits that has the flags, or, unless they are
 * required, any type among bits.
 */
static uint32_t memory_type(struct gpu *g, uint32_t bits,
			    VkMemoryPropertyFlags flags, int required)
{
	VkPhysicalDeviceMemoryProperties mp;
	uint32_t i;

	vkGetPhysicalDeviceMemoryProperties(g->phys, &mp);
	for (i = 0; i < mp.memoryTypeCount; i++) {
		if ((bits & (1u << i)) &&
		    (mp.memoryTypes[i].propertyFlags & flags) == flags)
			return i;
	}
	if (required)
		die("finding a memory type", VK_ERROR_FEATURE_NOT_PRESENT);
	for (i = 0; i < mp.memoryTypeCount; i++) {
		if (bits & (1u << i))
			return i;
	}
	die("finding a memory type", VK_ERROR_FEATURE_NOT_PRESENT);
	return 0;
}

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

/* Record commands with record, submit them and wait for them. */
static void run(struct gpu *g, void (*record)(VkCommandBuffer, void *),
		void *arg)
{
	VkCommandBufferAllocateInfo cai = {
		.sType = VK_STRUCTURE_TYPE_COMMAND_BUFFER_ALLOCATE_INFO,
		.commandPool = g->pool,
		.level = VK_COMMAND_BUFFER_LEVEL_PRIMARY,
		.commandBufferCount = 1,
	};
	VkCommandBufferBeginInfo begin = {
		.sType = VK_STRUCTURE_TYPE_COMMAND_BUFFER_BEGIN_INFO,
		.flags = VK_COMMAND_BUFFER_USAGE_ONE_TIME_SUBMIT_BIT,
	};
	VkFenceCreateInfo fci = { .sType = VK_STRUCTURE_TYPE_FENCE_CREATE_INFO };
	VkCommandBuffer cmd;
	VkFence fence;

	check(vkAllocateCommandBuffers(g->dev, &cai, &cmd),
	      "vkAllocateCommandBuffers");
	check(vkBeginCommandBuffer(cmd, &begin), "vkBeginCommandBuffer");
	record(cmd, arg);
	check(vkEndCommandBuffer(cmd), "vkEndCommandBuffer");

	VkSubmitInfo si = {
		.sType = VK_STRUCTURE_TYPE_SUBMIT_INFO,
		.commandBufferCount = 1,
		.pCommandBuffers = &cmd,
	};
	check(vkCreateFence(g->dev, &fci, NULL, &fence), "vkCreateFence");
	check(vkQueueSubmit(g->queue, 1, &si, fence), "vkQueueSubmit");
	check(vkWaitForFences(g->dev, 1, &fence, VK_TRUE, 10ull * 1000000000),
	      "vkWaitForFences");
	vkDestroyFence(g->dev, fence, NULL);
	vkFreeCommandBuffers(g->dev, g->pool, 1, &cmd);
}

/*
 * A barrier on a whole buffer, which also transfers it between queue
 * families when they differ.
 */
static void barrier(VkCommandBuffer cmd, VkBuffer b,
		    VkPipelineStageFlags src_stage, VkAccessFlags src,
		    VkPipelineStageFlags dst_stage, VkAccessFlags dst,
		    uint32_t src_family, uint32_t dst_family)
{
	VkBufferMemoryBarrier bmb = {
		.sType = VK_STRUCTURE_TYPE_BUFFER_MEMORY_BARRIER,
		.srcAccessMask = src,
		.dstAccessMask = dst,
		.srcQueueFamilyIndex = src_family,
		.dstQueueFamilyIndex = dst_family,
		.buffer = b,
		.size = VK_WHOLE_SIZE,
	};

	vkCmdPipelineBarrier(cmd, src_stage, dst_stage, 0, 0, NULL, 1, &bmb,
			     0, NULL);
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
	gpu_init(&g);
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
	vkDestroyCommandPool(g.dev, g.pool, NULL);
	vkDestroyDevice(g.dev, NULL);
	vkDestroyInstance(g.instance, NULL);

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

	gpu_init(&g);
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
	vkDestroyCommandPool(g.dev, g.pool, NULL);
	vkDestroyDevice(g.dev, NULL);
	vkDestroyInstance(g.instance, NULL);
	return WIFEXITED(status) ? WEXITSTATUS(status) : 1;
}

int main(int argc, char **argv)
{
	/* Both processes write to one pipe, in the order they run. */
	setvbuf(stdout, NULL, _IOLBF, 0);
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

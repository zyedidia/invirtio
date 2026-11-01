/* SPDX-License-Identifier: GPL-2.0 */
/*
 * Vulkan setup shared by the test programs. A program sets prog to its name
 * and role to the part of it that runs, which prefix its messages.
 */
#ifndef VKUTIL_H
#define VKUTIL_H

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/stat.h>
#include <sys/sysmacros.h>
#include <vulkan/vulkan.h>

static const char *prog;
static const char *role;

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

/*
 * Create a device on the Vulkan device of a DRM render node, with the
 * device extensions in exts.
 */
static void gpu_init(struct gpu *g, const char *node,
		     const char *const *exts, uint32_t nexts)
{
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
		.enabledExtensionCount = nexts,
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

static void gpu_destroy(struct gpu *g)
{
	vkDestroyCommandPool(g->dev, g->pool, NULL);
	vkDestroyDevice(g->dev, NULL);
	vkDestroyInstance(g->instance, NULL);
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

#endif

OUT ?= ../build
CFLAGS ?= -O2 -Wall -Wextra
PROGS = $(OUT)/vk-dmabuf-share $(OUT)/vk-evict $(OUT)/vk-pressure $(OUT)/vk-bandwidth

all: $(PROGS)

$(OUT)/%: %.c vkutil.h
	mkdir -p $(OUT)
	$(CC) $(CFLAGS) -o $@ $< -lvulkan

.PHONY: all clean
clean:
	rm -f $(PROGS)

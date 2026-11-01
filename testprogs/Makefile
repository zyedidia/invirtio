OUT ?= ../build
CFLAGS ?= -O2 -Wall -Wextra

$(OUT)/vk-dmabuf-share: vk-dmabuf-share.c
	mkdir -p $(OUT)
	$(CC) $(CFLAGS) -o $@ $< -lvulkan

.PHONY: clean
clean:
	rm -f $(OUT)/vk-dmabuf-share

OUT ?= ../build
CFLAGS ?= -O2 -Wall -Wextra
PROGS = $(OUT)/vk-dmabuf-share $(OUT)/vk-evict $(OUT)/vk-pressure $(OUT)/vk-bandwidth $(OUT)/kms-flip

all: $(PROGS)

$(OUT)/%: %.c vkutil.h
	mkdir -p $(OUT)
	$(CC) $(CFLAGS) -o $@ $< -lvulkan

.PHONY: all clean
clean:
	rm -f $(PROGS)

MESA_SRC ?= ../build/mesa-src
MESA ?= ../build/mesa

$(OUT)/kms-flip: kms-flip.c
	mkdir -p $(OUT)
	$(CC) $(CFLAGS) -I$(MESA_SRC)/include -I$(MESA)/include \
		$$(pkg-config --cflags libdrm) -o $@ $< -L$(MESA)/lib -lgbm \
		-l:libEGL.so.1 -l:libGLESv2.so.2 $$(pkg-config --libs libdrm)

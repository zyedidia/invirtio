OUT ?= ../build
CFLAGS ?= -O2 -Wall -Wextra

$(OUT)/invirtio-launch: invirtio-launch.c
	mkdir -p $(OUT)
	$(CC) $(CFLAGS) -o $@ $<

.PHONY: clean
clean:
	rm -f $(OUT)/invirtio-launch

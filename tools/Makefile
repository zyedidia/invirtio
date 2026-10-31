OUT ?= ../build
SRC := $(shell find . -name '*.go') go.mod go.sum

all: $(OUT)/invirtio $(OUT)/invirtio-test

$(OUT)/%: $(SRC)
	mkdir -p $(OUT)
	go build -o $@ ./cmd/$*

.PHONY: all clean
clean:
	rm -f $(OUT)/invirtio $(OUT)/invirtio-test

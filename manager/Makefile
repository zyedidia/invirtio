OUT ?= ../build

$(OUT)/invirtio: *.go go.mod go.sum
	mkdir -p $(OUT)
	go build -o $@ .

.PHONY: clean
clean:
	rm -f $(OUT)/invirtio

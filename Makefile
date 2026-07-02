VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -s -w -X main.version=$(VERSION)
BIN = mkvgo

.PHONY: build test vet fuzz clean release

build:
	go build -ldflags="$(LDFLAGS)" -o $(BIN) ./cmd/mkvgo/

test:
	go test -race ./...

vet:
	go vet ./...

# Runs each parser fuzzer briefly; CI fuzzes continuously (.github/workflows/fuzz.yml).
FUZZTIME ?= 30s
fuzz:
	go test -fuzz=FuzzRead -fuzztime=$(FUZZTIME) ./mkv/reader/
	go test -fuzz=FuzzReadMeta -fuzztime=$(FUZZTIME) ./mkv/reader/
	go test -fuzz=FuzzBlockReader -fuzztime=$(FUZZTIME) ./mkv/reader/
	go test -fuzz=FuzzCodecColour -fuzztime=$(FUZZTIME) ./mkv/reader/
	go test -fuzz=FuzzParseOGMChapters -fuzztime=$(FUZZTIME) ./mkv/
	go test -fuzz=FuzzParseMP4 -fuzztime=$(FUZZTIME) ./mp4/
	go test -fuzz=FuzzVP9FrameHeader -fuzztime=$(FUZZTIME) ./mp4/

# End-to-end verification against real ffmpeg/ffprobe (opt-in):
#   make e2e                            # local ffmpeg on PATH
#   MKVGO_E2E=docker:evey-server make e2e   # ffmpeg inside a container
e2e:
	sh scripts/e2e.sh

clean:
	rm -rf dist/ $(BIN)

PLATFORMS = \
	linux/amd64 \
	linux/arm64 \
	windows/amd64 \
	darwin/amd64 \
	darwin/arm64

release: clean
	@mkdir -p dist
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; \
		arch=$${platform#*/}; \
		ext=""; \
		if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		out="dist/$(BIN)-$$os-$$arch$$ext"; \
		echo "building $$out"; \
		GOOS=$$os GOARCH=$$arch go build -ldflags="$(LDFLAGS)" -o $$out ./cmd/mkvgo/; \
	done
	@ls -lh dist/

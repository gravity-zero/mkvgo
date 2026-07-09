VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -s -w -X main.version=$(VERSION)
BIN = mkvgo

.PHONY: build test vet fuzz clean release wasm wasm-smoke

# CGO_ENABLED=0: mkvgo is pure Go; keep the binaries static (and buildable
# without a C toolchain — net/http would otherwise link the cgo resolver).
build:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $(BIN) ./cmd/mkvgo/

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
#   MKVGO_E2E=docker:<image> make e2e       # a container image with the tools on PATH
e2e:
	sh scripts/e2e.sh

# WebAssembly build: dist/wasm/mkvgo.wasm + Go's wasm_exec.js runtime.
# See docs/wasm.md; web/mkvgo.ts is the typed wrapper.
wasm:
	mkdir -p dist/wasm
	GOOS=js GOARCH=wasm go build -trimpath -ldflags="$(LDFLAGS)" -o dist/wasm/mkvgo.wasm ./cmd/mkvgo-wasm/
	install -m 0644 "$$(go env GOROOT)/lib/wasm/wasm_exec.js" dist/wasm/wasm_exec.js

# Runs the wasm artifact end to end under Node ≥ 18 (probe/remux/HLS + error paths).
wasm-smoke: wasm
	node scripts/wasm_smoke.mjs

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

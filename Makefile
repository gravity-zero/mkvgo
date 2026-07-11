VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -s -w -X main.version=$(VERSION)
BIN = mkvgo

.PHONY: build test vet fuzz bench clean release wasm wasm-smoke preflight ci-status

# CGO_ENABLED=0: mkvgo is pure Go; keep the binaries static (and buildable
# without a C toolchain — net/http would otherwise link the cgo resolver).
build:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $(BIN) ./cmd/mkvgo/

test:
	go test -race ./...

vet:
	go vet ./...

# Serving benchmarks: segment/manifest serve cost, plan construction, and
# concurrent throughput. allocs/op and B/op are the machine-independent capacity
# signal; the deterministic anti-regression gates run in the normal test suite
# (TestServeAllocDoesNotScaleWithSourceSize, TestServingMemoryPerStream, ...).
bench:
	CGO_ENABLED=0 go test ./mp4/ -run '^$$' -bench 'Benchmark(Serve|PlanHLS)' -benchmem

# Runs each parser fuzzer briefly; CI fuzzes continuously (.github/workflows/fuzz.yml).
FUZZTIME ?= 30s
fuzz:
	go test -fuzz=FuzzRead -fuzztime=$(FUZZTIME) ./mkv/reader/
	go test -fuzz=FuzzReadMeta -fuzztime=$(FUZZTIME) ./mkv/reader/
	go test -fuzz=FuzzBlockReader -fuzztime=$(FUZZTIME) ./mkv/reader/
	go test -fuzz=FuzzCodecColour -fuzztime=$(FUZZTIME) ./mkv/reader/
	go test -fuzz=FuzzResyncToCluster -fuzztime=$(FUZZTIME) ./mkv/reader/
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

# preflight: the pre-push gate. Everything CI can check locally, INCLUDING a
# cross-compile for every OS the CI matrix builds on - a Linux build alone does
# NOT catch a Windows/macOS break. Run before tagging a release.
preflight:
	@echo "== gofmt =="; \
	  bad="$$(gofmt -l . | grep -v '^\.' || true)"; \
	  [ -z "$$bad" ] || { echo "$$bad"; echo "gofmt: files need formatting"; exit 1; }
	@for os in linux windows darwin; do \
	  echo "== GOOS=$$os build+vet =="; \
	  GOOS=$$os CGO_ENABLED=0 go build ./... || exit 1; \
	  GOOS=$$os CGO_ENABLED=0 go vet ./...   || exit 1; \
	done
	@echo "== wasm build =="; GOOS=js GOARCH=wasm CGO_ENABLED=0 go build ./cmd/mkvgo-wasm/
	@echo "== tests =="; CGO_ENABLED=0 go test ./...
	@echo "preflight OK - cross-platform compile + tests pass locally; confirm the real matrix with 'make ci-status' after pushing"

# ci-status: report the real GitHub Actions matrix result (all OSes) for the
# current HEAD. The compile-level preflight cannot catch a RUNTIME platform
# difference (e.g. a signal unsupported on Windows) - only the actual matrix
# can, so verify it before declaring a release done.
ci-status:
	@sh scripts/ci-status.sh

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

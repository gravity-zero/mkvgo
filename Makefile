VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -s -w -X main.version=$(VERSION)
# The golangci-lint the CI lint job runs (.github/workflows/ci.yml); preflight uses the same.
GOLANGCI_LINT_VERSION := v2.14.0
BIN = mkvgo

.PHONY: build test vet fuzz gate heavy bench clean release wasm wasm-smoke preflight ci-status

# CGO_ENABLED=0: mkvgo is pure Go; keep the binaries static (and buildable
# without a C toolchain - net/http would otherwise link the cgo resolver).
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

# Runs every fuzz target briefly, discovered rather than listed (scripts/fuzz-all.sh); CI fuzzes nightly with the
# same script (.github/workflows/fuzz.yml).
FUZZTIME ?= 30s
fuzz:
	FUZZTIME=$(FUZZTIME) scripts/fuzz-all.sh

# gate: the per-commit check - gofmt, tests, vet, lint - ending in a receipt naming the commit.
gate:
	scripts/gate.sh

# heavy: the race suite and every fuzz target, in a container bounded to 4 CPUs and 6 GB so a long run never starves
# the machine it shares; HEAVY_FUZZTIME per target. Output files come back owned by the caller.
HEAVY_FUZZTIME ?= 180s
heavy:
	docker run --rm --cpus 4 --memory 6g -v "$(CURDIR)":/src -w /src \
	  -v mkvgo-gocache:/root/.cache/go-build -v mkvgo-gomod:/go/pkg/mod -e GOFLAGS=-buildvcs=false \
	  golang:1.27.2-trixie sh -c 'go test -race -count=1 ./... && FUZZTIME=$(HEAVY_FUZZTIME) scripts/fuzz-all.sh; rc=$$?; \
	  chown -R $(shell id -u):$(shell id -g) /src/mkv /src/mp4 /src/ebml /src/httpfs /src/internal; exit $$rc'

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
# NOT catch a Windows/macOS break. Run before tagging a release, naming it:
#   make preflight RELEASE=v0.57.0
# scripts/release-check.sh fails when RELEASE is below what gorelease asks for against the previous tag, or above it
# without a Breaking section in the CHANGELOG (scripts/version-check.sh, tested first), and reports the
# WASM's size change against that tag's (a warning past WASM_GROWTH_WARN %).
preflight:
	@echo "== version check test =="; scripts/version-check_test.sh
	@RELEASE=$(RELEASE) scripts/release-check.sh
	@echo "== gofmt =="; \
	  bad="$$(gofmt -l . | grep -v '^\.' || true)"; \
	  [ -z "$$bad" ] || { echo "$$bad"; echo "gofmt: files need formatting"; exit 1; }
	@for os in linux windows darwin; do \
	  echo "== GOOS=$$os build+vet =="; \
	  GOOS=$$os CGO_ENABLED=0 go build ./... || exit 1; \
	  GOOS=$$os CGO_ENABLED=0 go vet ./...   || exit 1; \
	done
	@echo "== wasm build =="; GOOS=js GOARCH=wasm CGO_ENABLED=0 go build ./cmd/mkvgo-wasm/
	@echo "== GOARCH=arm (32-bit) build =="; GOOS=linux GOARCH=arm CGO_ENABLED=0 go build ./...
	@echo "== lint =="; \
	  lint="$$(command -v golangci-lint || echo "$$(go env GOPATH)/bin/golangci-lint")"; \
	  [ -x "$$lint" ] || { echo "golangci-lint missing: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)"; exit 1; }; \
	  CGO_ENABLED=0 "$$lint" run ./... || exit 1
	@echo "== tests =="; CGO_ENABLED=0 go test ./...
	@echo "== tests (386, 32-bit int) =="; GOARCH=386 CGO_ENABLED=0 go test ./...
	@echo "== tests (poisoned buffers) =="; CGO_ENABLED=0 go test -tags mkvgo_poison ./...
	@echo "== doc sizes =="; sh scripts/sizes.sh
	@echo "== wasm smoke (Node) =="; $(MAKE) --no-print-directory wasm >/dev/null && \
	  if command -v node >/dev/null; then node scripts/wasm_smoke.mjs; \
	  elif command -v docker >/dev/null; then docker run --rm -v "$(CURDIR)":/src -w /src node:22 node scripts/wasm_smoke.mjs; \
	  else echo "wasm smoke needs node or docker (CI runs it on Linux)"; exit 1; fi
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
		GOOS=$$os GOARCH=$$arch go build -ldflags="$(LDFLAGS)" -o $$out ./cmd/mkvgo/ || exit 1; \
	done
	@ls -lh dist/

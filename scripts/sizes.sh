#!/usr/bin/env sh
# Checks the artifact sizes the docs state against the real builds (make's
# LDFLAGS): the linux/amd64 binary and the WebAssembly module, raw and gzip -9.
# A stated size more than 15% off the measured one fails the run.
set -eu

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
LDFLAGS="-s -w -X main.version=sizes"

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="$LDFLAGS" -o "$TMP/mkvgo" ./cmd/mkvgo/
GOOS=js GOARCH=wasm go build -trimpath -ldflags="$LDFLAGS" -o "$TMP/mkvgo.wasm" ./cmd/mkvgo-wasm/

mb() { awk -v b="$1" 'BEGIN { printf "%.2f", b / 1000000 }'; }
bin=$(mb "$(wc -c < "$TMP/mkvgo")")
wasm=$(mb "$(wc -c < "$TMP/mkvgo.wasm")")
wasmgz=$(mb "$(gzip -9c "$TMP/mkvgo.wasm" | wc -c)")
echo "  measured: binary $bin MB, wasm $wasm MB ($wasmgz MB gzipped)"

fail=0
# check <label> <stated MB> <measured MB>
check() {
  if [ -z "$2" ]; then
    echo "sizes: $1 not found in the docs (the pattern below must match)" >&2
    fail=1
    return
  fi
  if ! awk -v s="$2" -v m="$3" 'BEGIN { d = (s - m) / m; exit !(d <= 0.15 && d >= -0.15) }'; then
    echo "sizes: $1 says $2 MB, measured $3 MB (more than 15% off)" >&2
    fail=1
  fi
}
grab() { sed -n "s/$2/\1/p" "$1" | head -1; }

check "README.md binary" "$(grab README.md '.*One static binary (about \([0-9.]*\) MB.*')" "$bin"
check "README.md release binaries" "$(grab README.md '.*produces stripped ~\([0-9.]*\) MB binaries.*')" "$bin"
check "README.md wasm" "$(grab README.md '.*module of about \([0-9.]*\) MB that gzips.*')" "$wasm"
check "README.md wasm gzipped" "$(grab README.md '.*that gzips to about \([0-9.]*\) MB.*')" "$wasmgz"
check "docs/wasm.md wasm" "$(grab docs/wasm.md '.*\*\*~\([0-9.]*\) MB raw.*')" "$wasm"
check "docs/wasm.md wasm gzipped" "$(grab docs/wasm.md '.*MB raw, ~\([0-9.]*\) MB gzipped.*')" "$wasmgz"
[ "$fail" = 0 ] || exit 1
echo "  doc sizes OK"

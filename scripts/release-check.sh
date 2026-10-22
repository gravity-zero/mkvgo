#!/usr/bin/env bash
# release-check.sh - preflight's release checks against the previous tag: RELEASE must be at least what gorelease
# suggests, above it only with a Breaking section (scripts/version-check.sh), and the WASM's size is compared with
# the previous tag's, both built here with the same toolchain and flags.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)" || exit 1
GORELEASE=golang.org/x/exp/cmd/gorelease@v0.0.0-20261014143641-f0251eb4aa3e
WASM_GROWTH_WARN=${WASM_GROWTH_WARN:-5} # percent, either way
[ -n "${RELEASE:-}" ] || { echo "RELEASE=vX.Y.Z is required: the tag about to be cut"; exit 1; }
base=${BASE:-$(git describe --tags --abbrev=0 --match 'v*' HEAD)} || exit 1

echo "== gorelease -base=$base =="
report=$(go run "$GORELEASE" -base="$base" 2>&1)
echo "$report"
suggested=$(echo "$report" | sed -n 's/^Suggested version: \(v[^ ]*\).*/\1/p' | tail -1)
[ -n "$suggested" ] || { echo "gorelease suggested no version"; exit 1; }
scripts/version-check.sh "$suggested" "$RELEASE" CHANGELOG.md || exit 1

echo "== WASM size against $base =="
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/base"
git archive "$base" | tar -x -C "$tmp/base" || exit 1
wasm() { (cd "$1" && GOOS=js GOARCH=wasm CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$2" ./cmd/mkvgo-wasm/); }
wasm "$tmp/base" "$tmp/base.wasm" || { echo "building $base's WASM failed (its go directive may need a newer toolchain)"; exit 1; }
wasm . "$tmp/head.wasm" || exit 1
was=$(wc -c <"$tmp/base.wasm")
now=$(wc -c <"$tmp/head.wasm")
delta=$((now - was))
pct=$(awk -v d="$delta" -v w="$was" 'BEGIN { printf "%+.1f", 100 * d / w }')
echo "WASM (-trimpath -s -w, $(go env GOVERSION)): $base $was bytes, HEAD $now bytes, $(printf '%+d' "$delta") bytes ($pct %)"
if awk -v p="$pct" -v t="$WASM_GROWTH_WARN" 'BEGIN { exit !(p > t || p < -t) }'; then
	echo "WARNING: the WASM changed by more than $WASM_GROWTH_WARN % against $base: state it in the CHANGELOG"
	section=$(awk -v v="## [${RELEASE#v}]" 'index($0, v) == 1 || index($0, "## [Unreleased]") == 1 { on = 1; next } /^## \[/ { on = 0 } on' CHANGELOG.md)
	echo "$section" | grep -q WASM || echo "WARNING: the CHANGELOG's ${RELEASE#v} section does not mention the WASM"
fi
echo "release check OK: $RELEASE against $base (gorelease asks for $suggested)"

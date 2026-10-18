#!/usr/bin/env bash
# gate.sh - the per-commit check: gofmt, tests (no cgo, no race), vet, lint. Prints a receipt naming the commit and
# how many paths differ from it, so a "green" always says which tree it was.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)" || exit 1
bad=$(gofmt -l . | grep -v '^bench/' || true)
[ -z "$bad" ] || { echo "gofmt: $bad"; exit 1; }
log=$(mktemp)
trap 'rm -f "$log"' EXIT
CGO_ENABLED=0 go test -count=1 ./... >"$log" 2>&1 || { grep -v '^ok\|no test files' "$log" | tail -40; echo "TEST FAIL"; exit 1; }
CGO_ENABLED=0 go vet ./... || { echo "VET FAIL"; exit 1; }
lint="$(command -v golangci-lint || echo "$(go env GOPATH)/bin/golangci-lint")"
CGO_ENABLED=0 "$lint" run ./... || { echo "LINT FAIL"; exit 1; }
echo "GATE OK $(git rev-parse --short HEAD) dirty=$(git status --short | wc -l)"

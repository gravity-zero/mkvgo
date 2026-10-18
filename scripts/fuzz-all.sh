#!/usr/bin/env bash
# fuzz-all.sh - runs every Fuzz* target of the module, one at a time, for FUZZTIME (default 30s) each.
# The targets are discovered, never listed: a new one is fuzzed the day it lands. Exits 1 when any target fails.
set -uo pipefail
fuzztime="${FUZZTIME:-30s}"
fail=0
for pkg in $(go list ./...); do
	for fn in $(go test -list '^Fuzz' "$pkg" 2>/dev/null | grep '^Fuzz' || true); do
		[ -n "${GITHUB_ACTIONS:-}" ] && echo "::group::$pkg $fn" || echo "== $pkg $fn"
		start=$(date +%s)
		if ! go test "$pkg" -run '^$' -fuzz="^${fn}\$" -fuzztime="$fuzztime"; then
			fail=1
			echo "FAIL $fn"
		fi
		echo "$fn: $(($(date +%s) - start))s"
		[ -n "${GITHUB_ACTIONS:-}" ] && echo "::endgroup::"
	done
done
exit $fail

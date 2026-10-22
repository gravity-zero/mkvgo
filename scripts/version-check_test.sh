#!/usr/bin/env bash
# version-check_test.sh - version-check.sh against changelogs written here: each case states the expected verdict.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)" || exit 1
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
printf '# Changelog\n\n## [Unreleased]\n\n### Breaking\n\n- bytes changed\n\n### Fixed\n\n- a fix\n\n## [0.58.0] - 2026-10-22\n\n### Added\n\n- a feature\n' >"$tmp/unreleased-breaking.md"
printf '# Changelog\n\n## [0.59.0] - 2026-10-22\n\n### Breaking\n\n- bytes changed\n\n## [0.58.0] - 2026-10-22\n\n### Added\n\n- a feature\n' >"$tmp/dated-breaking.md"
printf '# Changelog\n\n## [Unreleased]\n\n### Fixed\n\n- a fix\n\n## [0.58.0] - 2026-10-22\n\n### Breaking\n\n- an older break\n' >"$tmp/no-breaking.md"
printf '# Changelog\n\n## [Unreleased]\n\n### Fixed\n\n- the Breaking section of 0.58.0 named in passing\n' >"$tmp/breaking-in-text.md"
fail=0
check() { # want(ok|refused) suggested release changelog
	out=$(scripts/version-check.sh "$2" "$3" "$tmp/$4" 2>&1)
	rc=$?
	got=refused
	[ $rc -eq 0 ] && got=ok
	if [ "$got" = "$1" ]; then
		echo "ok: $2 suggested, $3 tagged, $4: $got"
	else
		echo "FAIL: $2 suggested, $3 tagged, $4: $got, want $1 ($out)"
		fail=1
	fi
}
check ok v0.58.1 v0.58.1 no-breaking.md
check ok v0.58.1 v0.59.0 unreleased-breaking.md
check ok v0.58.1 v0.59.0 dated-breaking.md
check refused v0.58.1 v0.59.0 no-breaking.md
check refused v0.58.1 v0.59.0 breaking-in-text.md
check refused v0.59.0 v0.58.1 unreleased-breaking.md
check refused v0.59.0 v0.58.9 unreleased-breaking.md
check refused v0.58.10 v0.58.9 unreleased-breaking.md
check ok v0.58.9 v0.58.10 unreleased-breaking.md
check refused v0.58.1 0.59.0 unreleased-breaking.md
exit $fail

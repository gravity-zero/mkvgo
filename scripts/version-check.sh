#!/usr/bin/env bash
# version-check.sh SUGGESTED RELEASE CHANGELOG - the tag about to be cut against gorelease's suggestion: never below it,
# above it only when the CHANGELOG's section for that version (or [Unreleased]) holds a "### Breaking" heading.
set -uo pipefail
[ $# -eq 3 ] || { echo "usage: version-check.sh SUGGESTED RELEASE CHANGELOG"; exit 2; }
suggested=$1 release=$2 changelog=$3
semver='^v[0-9]+\.[0-9]+\.[0-9]+$'
[[ $suggested =~ $semver ]] || { echo "gorelease suggested $suggested, not a vX.Y.Z version"; exit 1; }
[[ $release =~ $semver ]] || { echo "RELEASE=$release is not a vX.Y.Z version"; exit 1; }
[ "$release" = "$suggested" ] && { echo "RELEASE=$release is what gorelease asks for"; exit 0; }
IFS=. read -r s1 s2 s3 <<<"${suggested#v}"
IFS=. read -r r1 r2 r3 <<<"${release#v}"
if ((r1 < s1 || (r1 == s1 && (r2 < s2 || (r2 == s2 && r3 < s3))))); then
	echo "RELEASE=$release is below $suggested, what gorelease asks for: tag $suggested or higher"
	exit 1
fi
section=$(awk -v v="## [${release#v}]" 'index($0, v) == 1 || index($0, "## [Unreleased]") == 1 { on = 1; next } /^## \[/ { on = 0 } on' "$changelog")
if ! grep -q '^### Breaking' <<<"$section"; then
	echo "RELEASE=$release is above $suggested, what gorelease asks for, and the CHANGELOG's ${release#v} section has no ### Breaking: tag $suggested, or state what breaks"
	exit 1
fi
echo "RELEASE=$release is above $suggested, what gorelease asks for, as the CHANGELOG's ${release#v} section states what breaks"

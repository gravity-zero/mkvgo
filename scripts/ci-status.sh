#!/bin/sh
# ci-status.sh - report the GitHub Actions matrix result for a commit (default
# HEAD) across every OS the CI runs on, so a release is never declared green on
# the strength of a local Linux run alone. Unauthenticated (public repo);
# needs curl + python3. Exits non-zero if any check failed or is not yet green.
set -eu

sha="${1:-$(git rev-parse HEAD)}"
repo="gravity-zero/mkvgo"
echo "CI status for $repo @ $sha"

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
curl -sf "https://api.github.com/repos/$repo/commits/$sha/check-runs" > "$tmp" || {
	echo "  could not reach the GitHub API" >&2
	exit 2
}

python3 - "$tmp" <<'PY'
import sys, json
runs = json.load(open(sys.argv[1])).get("check_runs", [])
if not runs:
    print("  no check runs yet (CI may not have started)")
    sys.exit(2)
bad = 0
for r in sorted(runs, key=lambda x: x["name"]):
    status, concl = r.get("status"), r.get("conclusion")
    if concl == "success" or concl in ("neutral", "skipped"):
        mark = "OK "
    elif status != "completed":
        mark, bad = "...", bad + 1
    else:
        mark, bad = "!! ", bad + 1
    name = r["name"]
    print("  %s %-42s %-11s %s" % (mark, name, status, concl))
sys.exit(1 if bad else 0)
PY

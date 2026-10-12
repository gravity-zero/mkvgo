#!/usr/bin/env bash
# keyframes_verify.sh - check a source's keyframe stream against a decoder.
#
#   scripts/keyframes_verify.sh <file.mkv|.webm> [every_ms] [limit_s]
#
# Writes the keyframe-only stream (mkvgo extract-frames -every), decodes it
# with ffmpeg (-copyts, so the PTS are the source's), and checks:
#   1. one decoded picture per sample, in order, each at its sample's PtsMs;
#   2. each picture identical (showinfo checksum) to the picture a FULL decode
#      of the source shows at the same PTS - over the first limit_s seconds
#      (default 60; 0 = the whole file, a full decode).
# Exit 0 only when both hold. Needs: go, ffmpeg (FFMPEG=path to override),
# python3. Reads the source; writes only to a temp dir.
set -euo pipefail

src=${1:?usage: keyframes_verify.sh <file> [every_ms] [limit_s]}
every=${2:-2000}
limit=${3:-60}
ffmpeg=${FFMPEG:-ffmpeg}
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

(cd "$root" && CGO_ENABLED=0 go build -o "$tmp/mkvgo" ./cmd/mkvgo)

"$tmp/mkvgo" extract-frames "$src" -every "$every" -json -o - 2>"$tmp/stats.json" |
	"$ffmpeg" -hide_banner -nostdin -copyts -f matroska -i pipe:0 -an \
		-fps_mode passthrough -vf showinfo -f null - 2>"$tmp/stream.log"

full_limit=()
[ "$limit" != 0 ] && full_limit=(-t "$((limit + 1))")
"$ffmpeg" -hide_banner -nostdin -copyts "${full_limit[@]}" -i "$src" -an \
	-fps_mode passthrough -vf showinfo -f null - 2>"$tmp/full.log"

python3 -I - "$tmp" "$limit" <<'EOF'
import json, re, sys
tmp, limit = sys.argv[1], int(sys.argv[2])
pat = re.compile(r"pts_time:(-?[0-9.]+) .*checksum:([0-9A-F]+)")
def frames(name):
    out = []
    for line in open(f"{tmp}/{name}"):
        m = pat.search(line)
        if m:
            out.append((round(float(m.group(1)) * 1000), m.group(2)))
    return out
st = json.load(open(f"{tmp}/stats.json"))
want = st["PtsMs"] or []
got = frames("stream.log")
full = dict(frames("full.log"))
errors = sum(1 for l in open(f"{tmp}/stream.log") if re.search(r"(?i)error|invalid|duplicate poc", l))
ok = True
print(f"samples={st['Samples']} skipped={st['Skipped']} cues={st['Cues']} decoded={len(got)} decoder_errors={errors}")
if [p for p, _ in got] != want:
    ok = False
    bad = next((i for i, (a, b) in enumerate(zip([p for p, _ in got], want)) if a != b), min(len(got), len(want)))
    print(f"FAIL order/PTS: first difference at index {bad}")
checked = mism = 0
for p, ck in got:
    if limit and p > limit * 1000:
        continue
    if p in full:
        checked += 1
        if full[p] != ck:
            mism += 1
            print(f"FAIL pixels at {p} ms: stream {ck} full decode {full[p]}")
print(f"pixel check: {checked - mism}/{checked} identical to a full decode")
if mism or checked == 0 or errors:
    ok = False
print("PASS" if ok else "FAIL")
sys.exit(0 if ok else 1)
EOF

#!/usr/bin/env sh
# apple-check.sh - what Apple's media stack makes of the composition offsets mkvgo writes (macOS only).
#
# ffmpeg encodes two sources, mkvgo packages them as HLS, AVFoundation reads the init and segments back
# (scripts/apple-pts.swift), and every frame must present where the prober reads it in the source:
#   late        the parameter sets declare the reordering, the opening frames show none: a positive shift, taken back
#               by the init's edit list;
#   undeclared  the header declares no reordering and the stream reorders after its first GOP: negative offsets.
#
# Usage: sh scripts/apple-check.sh   (ffmpeg, ffprobe, go, swiftc and python3 on PATH)
set -eu

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

echo "== build"
CGO_ENABLED=0 go build -o "$TMP/mkvgo" ./cmd/mkvgo
swiftc -O -o "$TMP/apple-pts" scripts/apple-pts.swift 2>"$TMP/swiftc.log" || { cat "$TMP/swiftc.log" >&2; exit 1; }

echo "== fixtures"
enc() { # enc <out> <frames> <x264 arguments...>
  out=$1; frames=$2; shift 2
  ffmpeg -nostdin -v error -y -f lavfi -i "testsrc2=size=320x180:rate=24000/1001" -frames:v "$frames" \
    -c:v libx264 -preset medium -pix_fmt yuv420p "$@" "$TMP/$out"
}
join() { # join <a> <b> <out>
  printf "file '%s'\nfile '%s'\n" "$1" "$2" > "$TMP/list.txt"
  ffmpeg -nostdin -v error -y -f concat -safe 0 -i "$TMP/list.txt" -c copy "$TMP/$3"
}
K="keyint=48:min-keyint=48:scenecut=0"
enc late_a.mkv 48 -bf 3 -x264-params "b-pyramid=normal:$K:b-adapt=1:b-bias=-100"
enc late_b.mkv 96 -bf 3 -x264-params "b-pyramid=normal:$K:b-adapt=0"
join late_a.mkv late_b.mkv late.mkv
enc undeclared_a.mkv 48 -bf 0 -x264-params "$K"
enc undeclared_b.mkv 96 -bf 3 -x264-params "b-pyramid=normal:$K:b-adapt=0"
join undeclared_a.mkv undeclared_b.mkv undeclared.mkv

failed=0
check() { # check <name> <negative|positive>
  name=$1; want=$2
  ffprobe -v error -select_streams v:0 -show_entries packet=pts_time -of csv=p=0 "$TMP/$name.mkv" | tr -d ',\r' | sort -n > "$TMP/$name.want"
  "$TMP/mkvgo" to-hls "$TMP/$name.mkv" -o "$TMP/$name.hls" -segment 2 >/dev/null 2>"$TMP/$name.log" || { cat "$TMP/$name.log" >&2; exit 1; }
  cat "$TMP/$name.hls/init.mp4" "$TMP/$name.hls"/seg*.m4s > "$TMP/$name.mp4"
  "$TMP/apple-pts" "$TMP/$name.mp4" | cut -d' ' -f1 | sort -n > "$TMP/$name.got"
  python3 - "$TMP/$name.mp4" "$TMP/$name.want" "$TMP/$name.got" "$name" "$want" <<'EOF' || failed=1
import struct, sys
path, want_path, got_path, name, sign = sys.argv[1:6]
data = open(path, 'rb').read()
lowest, pos = 0, 0
while True:  # the lowest composition offset of every trun
    pos = data.find(b'trun', pos)
    if pos < 0:
        break
    flags = int.from_bytes(data[pos + 5:pos + 8], 'big')
    count = struct.unpack('>I', data[pos + 8:pos + 12])[0]
    p = pos + 12 + (4 if flags & 1 else 0) + (4 if flags & 4 else 0)
    for _ in range(count):
        p += sum(4 for f in (0x100, 0x200, 0x400) if flags & f)
        if flags & 0x800:
            lowest = min(lowest, struct.unpack('>i', data[p:p + 4])[0])
            p += 4
    pos += 4
want = [float(l) for l in open(want_path) if l.strip()]
got = [float(l) for l in open(got_path) if l.strip()]
problems = []
if sign == 'negative' and lowest >= 0:
    problems.append('the fixture has no negative composition offset: it does not test what it names')
if sign == 'positive' and lowest < 0:
    problems.append('the fixture has a negative composition offset (%d)' % lowest)
if len(got) != len(want):
    problems.append('%d samples read for %d frames in the source' % (len(got), len(want)))
else:
    deltas = sorted(g - w for g, w in zip(got, want))
    if deltas[-1] - deltas[0] > 0.0015:
        problems.append('presentation times move by %.3f to %.3f s against the source' % (deltas[0], deltas[-1]))
    elif abs(deltas[len(deltas) // 2]) > 0.0015:
        problems.append('every presentation time is %.3f s off the source' % deltas[len(deltas) // 2])
    if len(set(got)) != len(set(want)):
        problems.append('%d distinct presentation times for %d in the source' % (len(set(got)), len(set(want))))
detail = '%d frames, lowest composition offset %d' % (len(got), lowest)
if problems:
    print('  FAIL %s (%s): %s' % (name, detail, '; '.join(problems)))
    sys.exit(1)
print('  OK %s: %s, every presentation time is the source\'s' % (name, detail))
EOF
}
echo "== AVFoundation against the source"
check late positive
check undeclared negative
[ "$failed" = 0 ] || { echo "apple-check: FAILED" >&2; exit 1; }
echo "apple-check: ALL PASS"

#!/usr/bin/env sh
# apple-check.sh - what Apple's media stack makes of the composition offsets mkvgo writes (macOS only).
#
# ffmpeg encodes two sources, mkvgo packages them as HLS, AVFoundation decodes the init and segments back
# (scripts/apple-pts.swift), and every frame must present where the prober reads it in the source:
#   late        the parameter sets declare the reordering, the opening frames show none: a positive shift, taken back
#               by the init's edit list;
#   undeclared  the header declares no reordering and the stream reorders after its first GOP: negative offsets.
# Both are read again under --signed-offsets: no shift, no edit list, negative offsets from the first B-frame on.
# The same source remuxed by ffmpeg to a plain MP4 is read the same way and printed for reference.
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
# Two parts with different parameter sets: joined as MPEG-TS bytes, the second offset by the first's duration (the
# concat demuxer stamps the frames after such a joint alike).
enc undeclared_a.mkv 48 -bf 0 -x264-params "$K"
enc undeclared_b.mkv 96 -bf 3 -x264-params "b-pyramid=normal:$K:b-adapt=0"
ffmpeg -nostdin -v error -y -i "$TMP/undeclared_a.mkv" -c copy -bsf:v h264_mp4toannexb -muxdelay 0 -muxpreload 0 "$TMP/undeclared_a.ts"
ffmpeg -nostdin -v error -y -i "$TMP/undeclared_b.mkv" -c copy -bsf:v h264_mp4toannexb -muxdelay 0 -muxpreload 0 -output_ts_offset 2.002 "$TMP/undeclared_b.ts"
cat "$TMP/undeclared_a.ts" "$TMP/undeclared_b.ts" > "$TMP/undeclared.ts"
ffmpeg -nostdin -v error -y -i "$TMP/undeclared.ts" -c copy "$TMP/undeclared.mkv"

cat > "$TMP/compare.py" <<'PY'
# compare.py <mp4 or -> <source pts> <read pts> : prints "<verdict> <detail>"; verdict is ok or a reason
import struct, sys
path, want_path, got_path = sys.argv[1:4]
lowest = 0
if path != '-':
    data, pos = open(path, 'rb').read(), 0
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
want = sorted(float(l) for l in open(want_path) if l.strip())
got = sorted(float(l) for l in open(got_path) if l.strip())
detail = '%d of %d frames, lowest composition offset %d' % (len(got), len(want), lowest)
if len(got) != len(want):
    missing = sorted(set(want) - set(got))[:6]
    print('count|' + detail + ', source times not read: ' + ' '.join('%.3f' % m for m in missing))
    sys.exit(0)
deltas = sorted(g - w for g, w in zip(got, want))
detail += ', against the source %+.3f to %+.3f s' % (deltas[0], deltas[-1])
if deltas[-1] - deltas[0] > 0.0015:
    print('moving|' + detail)
elif abs(deltas[len(deltas) // 2]) > 0.0015:
    print('shifted|' + detail)
elif len(set(got)) != len(set(want)):
    print('duplicates|' + detail)
else:
    print('ok|' + detail)
PY

read_back() { # read_back <name> <file> <trun source or -> -> prints the stored and decoded verdicts, sets $verdict
  for mode in stored decoded; do
    "$TMP/apple-pts" "$2" $mode 2>"$TMP/$1.$mode.err" | cut -d' ' -f1 > "$TMP/$1.$mode" || { cat "$TMP/$1.$mode.err" >&2; }
    line=$(python3 "$TMP/compare.py" "$3" "$TMP/$1.want" "$TMP/$1.$mode")
    echo "    $mode: ${line%%|*} (${line#*|})"
    [ "$mode" = decoded ] && verdict=${line%%|*}
  done
  sort -u "$TMP/$1.decoded.err" | sed 's/^/    /'
}

failed=0
check() { # check <name> <negative|positive> [mkvgo to-hls flags...]
  name=$1; want=$2; shift 2
  ffprobe -v error -select_streams v:0 -show_entries packet=pts_time -of csv=p=0 "$TMP/$name.mkv" | tr -d ',\r' | sort -n > "$TMP/$name.want"
  cp "$TMP/$name.want" "$TMP/$name-reference.want"
  if [ "$(sort -u "$TMP/$name.want" | wc -l)" -ne "$(wc -l < "$TMP/$name.want")" ] || [ "$(wc -l < "$TMP/$name.want")" -ne 144 ]; then
    echo "  FAIL $name: the fixture has $(wc -l < "$TMP/$name.want") frames and $(sort -u "$TMP/$name.want" | wc -l) distinct presentation times, 144 of each expected"; failed=1; return
  fi
  rm -rf "$TMP/$name.hls"
  "$TMP/mkvgo" to-hls "$TMP/$name.mkv" -o "$TMP/$name.hls" -segment 2 "$@" >/dev/null 2>"$TMP/$name.log" || { cat "$TMP/$name.log" >&2; exit 1; }
  cat "$TMP/$name.hls/init.mp4" "$TMP/$name.hls"/seg*.m4s > "$TMP/$name.mp4"
  ffmpeg -nostdin -v error -y -i "$TMP/$name.mkv" -c copy "$TMP/$name-reference.mp4"
  echo "  $name, the reference muxer's plain MP4:"
  read_back "$name-reference" "$TMP/$name-reference.mp4" -
  echo "  $name, mkvgo's HLS $*:"
  read_back "$name" "$TMP/$name.mp4" "$TMP/$name.mp4"
  lowest=$(python3 "$TMP/compare.py" "$TMP/$name.mp4" "$TMP/$name.want" "$TMP/$name.want" | sed 's/.*lowest composition offset \(-*[0-9]*\).*/\1/')
  if [ "$want" = negative ] && [ "$lowest" -ge 0 ]; then echo "  FAIL $name: no negative composition offset, the fixture does not test what it names"; failed=1; fi
  if [ "$want" = positive ] && [ "$lowest" -lt 0 ]; then echo "  FAIL $name: a negative composition offset ($lowest)"; failed=1; fi
  if [ "$verdict" = ok ]; then echo "  OK $name: every frame AVFoundation presents is where the source has it"
  else echo "  FAIL $name: decoded frames are $verdict against the source"; failed=1; fi
}
echo "== AVFoundation against the source"
check late positive
check undeclared negative
check late negative --signed-offsets
check undeclared negative --signed-offsets
[ "$failed" = 0 ] || { echo "apple-check: FAILED" >&2; exit 1; }
echo "apple-check: ALL PASS"

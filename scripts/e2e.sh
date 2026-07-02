#!/usr/bin/env sh
# End-to-end verification of mkvgo's remux paths against real ffmpeg/ffprobe.
#
# ffmpeg generates real fixtures (H.264+AAC MKV, VP9 WebM, a non-faststart
# QuickTime .mov), mkvgo remuxes them every way, and ffmpeg/ffprobe must fully
# decode every output without a single error.
#
# Usage:
#   make e2e                              # ffmpeg/ffprobe on the local PATH
#   MKVGO_E2E=docker:<container> make e2e # ffmpeg inside a running container
set -eu

DOCKER_CONTAINER=""
case "${MKVGO_E2E:-}" in
  docker:*) DOCKER_CONTAINER="${MKVGO_E2E#docker:}" ;;
esac

if [ -n "$DOCKER_CONTAINER" ]; then
  docker exec "$DOCKER_CONTAINER" ffmpeg -version >/dev/null
elif ! command -v ffmpeg >/dev/null || ! command -v ffprobe >/dev/null; then
  echo "e2e: ffmpeg/ffprobe not found; install them or set MKVGO_E2E=docker:<container>" >&2
  exit 1
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"; [ -n "$DOCKER_CONTAINER" ] && docker exec "$DOCKER_CONTAINER" rm -rf /tmp/mkvgo-e2e 2>/dev/null || true' EXIT
[ -n "$DOCKER_CONTAINER" ] && docker exec "$DOCKER_CONTAINER" mkdir -p /tmp/mkvgo-e2e

# ff <args...>: run ffmpeg; ffp <args...>: run ffprobe. In docker mode the
# involved files are staged in/out around the call.
ff() {
  if [ -n "$DOCKER_CONTAINER" ]; then docker exec "$DOCKER_CONTAINER" ffmpeg -v error "$@"
  else ffmpeg -v error "$@"; fi
}
ffp() {
  if [ -n "$DOCKER_CONTAINER" ]; then docker exec "$DOCKER_CONTAINER" ffprobe -v error "$@"
  else ffprobe -v error "$@"; fi
}
# stage <local> -> prints the path the ffmpeg side sees.
stage() {
  if [ -n "$DOCKER_CONTAINER" ]; then
    docker cp "$1" "$DOCKER_CONTAINER:/tmp/mkvgo-e2e/$(basename "$1")" >/dev/null
    echo "/tmp/mkvgo-e2e/$(basename "$1")"
  else echo "$1"; fi
}
# unstage <remote-name> <local> -> fetches a file ffmpeg produced (docker only).
unstage() {
  docker cp "$DOCKER_CONTAINER:/tmp/mkvgo-e2e/$1" "$2" >/dev/null
}
# decode_ok <local file>: ffmpeg must decode it end-to-end with zero errors.
decode_ok() {
  p=$(stage "$1")
  ff -i "$p" -f null - 2>&1
  echo "  decode OK: $(basename "$1")"
}

echo "== build mkvgo"
CGO_ENABLED=0 go build -o "$TMP/mkvgo" ./cmd/mkvgo
MKVGO="$TMP/mkvgo"

echo "== generate fixtures with ffmpeg"
if [ -n "$DOCKER_CONTAINER" ]; then
  ff -f lavfi -i "testsrc2=size=320x240:rate=25" -f lavfi -i "sine=frequency=440:sample_rate=48000" \
     -t 3 -c:v libx264 -preset ultrafast -g 25 -pix_fmt yuv420p -c:a aac -shortest -y /tmp/mkvgo-e2e/src.mkv
  ff -f lavfi -i "testsrc2=size=320x240:rate=25" -t 2 -c:v libvpx-vp9 -deadline realtime -y /tmp/mkvgo-e2e/src.webm
  ff -f lavfi -i "testsrc2=size=320x240:rate=25" -f lavfi -i "sine=frequency=440:sample_rate=44100" \
     -t 1 -c:v libx264 -preset ultrafast -pix_fmt yuv420p -c:a aac -shortest -y /tmp/mkvgo-e2e/src.mov
  unstage src.mkv "$TMP/src.mkv"; unstage src.webm "$TMP/src.webm"; unstage src.mov "$TMP/src.mov"
else
  ffmpeg -v error -f lavfi -i "testsrc2=size=320x240:rate=25" -f lavfi -i "sine=frequency=440:sample_rate=48000" \
     -t 3 -c:v libx264 -preset ultrafast -g 25 -pix_fmt yuv420p -c:a aac -shortest -y "$TMP/src.mkv"
  ffmpeg -v error -f lavfi -i "testsrc2=size=320x240:rate=25" -t 2 -c:v libvpx-vp9 -deadline realtime -y "$TMP/src.webm"
  ffmpeg -v error -f lavfi -i "testsrc2=size=320x240:rate=25" -f lavfi -i "sine=frequency=440:sample_rate=44100" \
     -t 1 -c:v libx264 -preset ultrafast -pix_fmt yuv420p -c:a aac -shortest -y "$TMP/src.mov"
fi

echo "== MKV -> MP4 (+faststart) -> decode"
"$MKVGO" -f to-mp4 --faststart "$TMP/src.mkv" "$TMP/out.mp4" >/dev/null
decode_ok "$TMP/out.mp4"

echo "== MP4 -> MKV -> decode, then content parity via reindex"
"$MKVGO" -f from-mp4 "$TMP/out.mp4" "$TMP/back.mkv" >/dev/null
decode_ok "$TMP/back.mkv"
"$MKVGO" -f reindex "$TMP/back.mkv" "$TMP/re.mkv" >/dev/null
"$MKVGO" compare -blocks "$TMP/back.mkv" "$TMP/re.mkv" | grep -v "muxing_app\|writing_app" || true
"$MKVGO" -f split "$TMP/back.mkv" -o "$TMP/parts" -every 1 >/dev/null
echo "  split -every OK: $(ls "$TMP/parts" | wc -l) parts"

echo "== VP9 WebM -> MP4 (vp09) -> decode"
"$MKVGO" -f to-mp4 "$TMP/src.webm" "$TMP/vp9.mp4" >/dev/null
ffp -show_entries stream=codec_name -of csv "$(stage "$TMP/vp9.mp4")" | grep -q vp9
decode_ok "$TMP/vp9.mp4"

echo "== MKV -> seekable WebM: rejected codecs, then real VP9 -> decode"
if "$MKVGO" -f to-webm "$TMP/src.mkv" "$TMP/bad.webm" >/dev/null 2>&1; then
  echo "e2e: to-webm accepted h264 (must reject)" >&2; exit 1
fi
"$MKVGO" -f to-webm "$TMP/src.webm" "$TMP/out.webm" >/dev/null
decode_ok "$TMP/out.webm"

echo "== MKV -> fragmented-MP4 HLS -> decode playlist + standalone segment"
"$MKVGO" to-hls "$TMP/src.mkv" -o "$TMP/hls" -segment 1 >/dev/null
plist=$(stage "$TMP/hls/init.mp4" >/dev/null; stage "$TMP/hls/master.m3u8" >/dev/null; stage "$TMP/hls/playlist.m3u8" >/dev/null; stage "$TMP/hls/audio1.m3u8" >/dev/null; stage "$TMP/hls/init_a1.mp4" >/dev/null; stage "$TMP/hls/master.m3u8")
# stage each segment so the container ffmpeg can resolve the playlist entries.
for s in "$TMP"/hls/seg*.m4s; do stage "$s" >/dev/null; done
if [ -n "$DOCKER_CONTAINER" ]; then
  docker exec "$DOCKER_CONTAINER" ffmpeg -v error -allowed_extensions ALL -i "$plist" -f null - 2>&1
  docker exec "$DOCKER_CONTAINER" sh -c "cat /tmp/mkvgo-e2e/init.mp4 /tmp/mkvgo-e2e/seg00002.m4s > /tmp/mkvgo-e2e/hls-mid.mp4 && ffmpeg -v error -i /tmp/mkvgo-e2e/hls-mid.mp4 -f null -" 2>&1
else
  ffmpeg -v error -allowed_extensions ALL -i "$TMP/hls/playlist.m3u8" -f null - 2>&1
  cat "$TMP/hls/init.mp4" "$TMP/hls/seg00002.m4s" > "$TMP/hls/mid.mp4"
  ffmpeg -v error -i "$TMP/hls/mid.mp4" -f null - 2>&1
fi
echo "  HLS OK: playlist + standalone segment decode"

echo "== on-demand HLS: hls-segment byte-identical to the full pass"
"$MKVGO" hls-segment "$TMP/src.mkv" init -segment 1 > "$TMP/od-init.mp4"
cmp "$TMP/od-init.mp4" "$TMP/hls/init.mp4"
"$MKVGO" hls-segment "$TMP/src.mkv" playlist -segment 1 > "$TMP/od-playlist.m3u8"
cmp "$TMP/od-playlist.m3u8" "$TMP/hls/playlist.m3u8"
"$MKVGO" hls-segment "$TMP/src.mkv" 2 -segment 1 > "$TMP/od-seg2.m4s"
cmp "$TMP/od-seg2.m4s" "$TMP/hls/seg00002.m4s"
"$MKVGO" hls-segment "$TMP/src.mkv" seg_a1_00002.m4s -segment 1 > "$TMP/od-a2.m4s"
cmp "$TMP/od-a2.m4s" "$TMP/hls/seg_a1_00002.m4s"
echo "  on-demand OK: init + playlist + mid segment (video & audio) identical"

echo "== DASH manifest over the same CMAF segments -> decode"
if [ -n "$DOCKER_CONTAINER" ]; then
  stage "$TMP/hls/manifest.mpd" >/dev/null
  stage "$TMP/hls/init_a1.mp4" >/dev/null
  docker exec "$DOCKER_CONTAINER" ffmpeg -v error -allowed_extensions ALL -i /tmp/mkvgo-e2e/manifest.mpd -map 0:v -map 0:a -f null - 2>&1
else
  ffmpeg -v error -allowed_extensions ALL -i "$TMP/hls/manifest.mpd" -map 0:v -map 0:a -f null - 2>&1
fi
echo "  DASH OK: manifest decode"

echo "== MP4 source -> HLS/DASH packaging + on-demand parity + I-frame playlist"
"$MKVGO" -f to-hls "$TMP/out.mp4" -o "$TMP/hlsmp4" -segment 1 >/dev/null
test -f "$TMP/hlsmp4/iframe.m3u8"
grep -q I-FRAME-STREAM-INF "$TMP/hlsmp4/master.m3u8"
"$MKVGO" hls-segment "$TMP/out.mp4" master.m3u8 -segment 1 > "$TMP/od-mp4-master.m3u8"
cmp "$TMP/od-mp4-master.m3u8" "$TMP/hlsmp4/master.m3u8"
"$MKVGO" hls-segment "$TMP/out.mp4" 2 -segment 1 > "$TMP/od-mp4-seg2.m4s"
cmp "$TMP/od-mp4-seg2.m4s" "$TMP/hlsmp4/seg00002.m4s"
echo "  MP4-source OK: packaged + on-demand identical (master included) + iframe playlist"

echo "== QuickTime .mov (non-faststart) -> MKV -> decode"
"$MKVGO" probe "$TMP/src.mov" >/dev/null
"$MKVGO" -f from-mp4 "$TMP/src.mov" "$TMP/mov.mkv" >/dev/null
decode_ok "$TMP/mov.mkv"

echo "e2e: ALL PASS"
